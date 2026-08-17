package service

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nikunj-taneja/seedhibaat/internal/config"
	"github.com/nikunj-taneja/seedhibaat/internal/meta"
	"github.com/nikunj-taneja/seedhibaat/internal/security"
	"github.com/nikunj-taneja/seedhibaat/internal/shopify"
	"github.com/nikunj-taneja/seedhibaat/internal/store"
	"github.com/nikunj-taneja/seedhibaat/internal/workflow"
)

type Processor struct {
	Config  config.Config
	Store   *store.Store
	Meta    *meta.Client
	Shopify *shopify.Client
	Logger  *slog.Logger
}

type noRetryError struct{ err error }

func (e *noRetryError) Error() string { return e.err.Error() }
func (e *noRetryError) Unwrap() error { return e.err }

func (p *Processor) SyncWorkflowDefinitions(ctx context.Context) error {
	definitions, err := workflow.LoadDir(p.Config.WorkflowDir)
	if err != nil {
		return err
	}
	for _, loaded := range definitions {
		if err := persistWorkflowDefinition(ctx, p.Store.DB, loaded); err != nil {
			return fmt.Errorf("sync workflow %s: %w", loaded.Definition.Name, err)
		}
	}
	return nil
}

func persistWorkflowDefinition(ctx context.Context, db *sql.DB, loaded workflow.Loaded) error {
	var existingHash string
	err := db.QueryRowContext(ctx, `SELECT definition_hash FROM workflow_definitions WHERE name=? AND version=?`, loaded.Definition.Name, loaded.Definition.Version).Scan(&existingHash)
	if err == nil {
		if existingHash != loaded.Hash {
			return fmt.Errorf("workflow %s version %d is immutable; increment the version", loaded.Definition.Name, loaded.Definition.Version)
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	_, err = db.ExecContext(ctx, `INSERT INTO workflow_definitions(name,version,definition_hash,yaml,active,created_at) VALUES(?,?,?,?,0,?)`, loaded.Definition.Name, loaded.Definition.Version, loaded.Hash, string(loaded.YAML), time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

func (p *Processor) Run(ctx context.Context) error {
	if err := p.SyncWorkflowDefinitions(ctx); err != nil {
		return err
	}
	_, _ = p.Store.RecoverStaleJobs(ctx, time.Now().Add(-10*time.Minute))
	if err := p.enqueueReconciliation(ctx, time.Now()); err != nil {
		return fmt.Errorf("enqueue startup reconciliation: %w", err)
	}
	var wg sync.WaitGroup
	for i := 0; i < p.Config.WorkerConcurrency; i++ {
		wg.Add(1)
		go func(worker int) { defer wg.Done(); p.jobLoop(ctx, fmt.Sprintf("worker-%d", worker)) }(i + 1)
	}
	wg.Add(1)
	go func() { defer wg.Done(); p.webhookLoop(ctx) }()
	wg.Add(1)
	go func() { defer wg.Done(); p.reconciliationLoop(ctx) }()
	<-ctx.Done()
	wg.Wait()
	return nil
}

func (p *Processor) webhookLoop(ctx context.Context) {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			event, ok, err := p.Store.ClaimWebhook(ctx)
			if err != nil {
				p.Logger.Error("claim webhook", "error", err)
				continue
			}
			if !ok {
				continue
			}
			err = p.processWebhook(ctx, event)
			if finishErr := p.Store.FinishWebhook(ctx, event, err); finishErr != nil {
				p.Logger.Error("finish webhook", "error", finishErr)
			}
			if err != nil {
				p.Logger.Warn("webhook processing failed", "provider", event.Provider, "event_id", event.EventID, "attempt", event.Attempts, "error", safeLogError(err))
			} else {
				p.scrubWebhook(ctx, event)
			}
		}
	}
}

func (p *Processor) jobLoop(ctx context.Context, workerID string) {
	ticker := time.NewTicker(p.Config.WorkerPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			job, ok, err := p.Store.ClaimJob(ctx, workerID, time.Now())
			if err != nil {
				p.Logger.Error("claim job", "worker", workerID, "error", err)
				continue
			}
			if !ok {
				continue
			}
			err = p.processJob(ctx, job)
			if err == nil {
				err = p.Store.CompleteJob(ctx, job.ID, time.Now())
			} else {
				var permanent *noRetryError
				if errors.As(err, &permanent) {
					_ = p.Store.FailJobPermanently(ctx, job, err, time.Now())
				} else {
					_ = p.Store.FailJob(ctx, job, err, time.Now())
				}
			}
			if err != nil {
				p.Logger.Warn("job failed", "job_id", job.ID, "kind", job.Kind, "attempt", job.Attempts, "error", safeLogError(err))
			}
		}
	}
}

func (p *Processor) reconciliationLoop(ctx context.Context) {
	ticker := time.NewTicker(p.Config.ReconcileInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			if _, err := p.Store.ApplyRetention(ctx, time.Now().AddDate(0, 0, -p.Config.RetentionDays)); err != nil {
				p.Logger.Error("apply retention", "error", err)
			}
			if err := p.enqueueReconciliation(ctx, now); err != nil {
				p.Logger.Error("enqueue reconciliation", "error", err)
			}
		}
	}
}

func (p *Processor) enqueueReconciliation(ctx context.Context, now time.Time) error {
	watermark, exists, err := p.Store.SyncWatermark(ctx, "shopify", "catalog")
	if err != nil {
		return err
	}
	since := now.Add(-p.Config.InitialSyncLookback)
	mode := "initial"
	if exists {
		since = watermark.Add(-p.Config.ReconcileOverlap)
		mode = "incremental"
	}
	payload, err := json.Marshal(map[string]any{
		"since":     since.UTC().Format(time.RFC3339Nano),
		"watermark": now.UTC().Format(time.RFC3339Nano),
		"mode":      mode,
	})
	if err != nil {
		return err
	}
	_, err = p.Store.EnqueueJob(ctx, store.Job{ID: store.NewID("job"), StepID: mode, Kind: "shopify_reconcile", Payload: payload, MaxAttempts: 8}, "shopify-reconcile:"+now.UTC().Format("2006-01-02T15"), now)
	return err
}

func (p *Processor) processWebhook(ctx context.Context, event store.WebhookEvent) error {
	switch event.Provider {
	case "meta":
		return p.processMetaWebhook(ctx, event.Payload)
	case "shopify":
		return p.processShopifyWebhook(ctx, event)
	case "gokwik":
		return nil
	default:
		return fmt.Errorf("unknown webhook provider %q", event.Provider)
	}
}

func (p *Processor) processMetaWebhook(ctx context.Context, body []byte) error {
	webhook, err := meta.ParseWebhook(body)
	if err != nil {
		return err
	}
	for _, entry := range webhook.Entry {
		for _, change := range entry.Changes {
			for _, status := range change.Value.Statuses {
				if err := p.recordMetaStatus(ctx, status); err != nil {
					return err
				}
			}
			if len(change.Value.Messages) > 0 && !p.acceptsInboundPhoneNumber(change.Value.Metadata.PhoneNumberID) {
				p.Logger.Warn("ignored inbound Meta webhook for an unconfigured phone number")
				continue
			}
			for _, message := range change.Value.Messages {
				if err := p.recordInbound(ctx, message); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (p *Processor) acceptsInboundPhoneNumber(phoneNumberID string) bool {
	active := strings.TrimSpace(p.Config.MetaPhoneNumberID)
	test := strings.TrimSpace(p.Config.MetaTestPhoneNumberID)
	if active == "" && test == "" {
		return true
	}
	received := strings.TrimSpace(phoneNumberID)
	return received != "" && (received == active || received == test)
}

func (p *Processor) recordMetaStatus(ctx context.Context, status meta.MessageStatus) error {
	var messageID string
	var customerID int64
	if err := p.Store.DB.QueryRowContext(ctx, `SELECT id,customer_id FROM outbound_messages WHERE meta_message_id=?`, status.ID).Scan(&messageID, &customerID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	stamp := providerTime(status.Timestamp)
	digest := sha256.Sum256([]byte(status.ID + "\x00" + status.Status + "\x00" + status.Timestamp))
	tx, err := p.Store.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `INSERT INTO message_events(message_id,event_type,provider_timestamp,received_at,payload_fingerprint) VALUES(?,?,?,?,?) ON CONFLICT DO NOTHING`, messageID, status.Status, nullableTime(stamp), time.Now().UTC().Format(time.RFC3339Nano), hex.EncodeToString(digest[:]))
	if err != nil {
		return err
	}
	inserted, _ := result.RowsAffected()
	if inserted == 0 {
		return tx.Commit()
	}
	column := map[string]string{"sent": "sent_at", "delivered": "delivered_at", "read": "read_at", "failed": "failed_at"}[status.Status]
	if column != "" {
		stateExpression := map[string]string{"read": "'read'", "delivered": "CASE WHEN state='read' THEN state ELSE 'delivered' END", "sent": "CASE WHEN state IN ('delivered','read') THEN state ELSE 'sent' END", "failed": "CASE WHEN state IN ('sent','delivered','read') THEN state ELSE 'failed' END"}[status.Status]
		query := `UPDATE outbound_messages SET ` + column + `=coalesce(` + column + `,?),state=` + stateExpression + `,updated_at=? WHERE id=?`
		if _, err := tx.ExecContext(ctx, query, stamp.UTC().Format(time.RFC3339Nano), time.Now().UTC().Format(time.RFC3339Nano), messageID); err != nil {
			return err
		}
	}
	permanentDeliveryFailure := false
	if status.Status == "failed" && len(status.Errors) > 0 {
		e := status.Errors[0]
		permanentDeliveryFailure = e.Code == 131026 || e.Code == 131050
		_, err = tx.ExecContext(ctx, `UPDATE outbound_messages SET failure_code=?,failure_reason=? WHERE id=?`, strconv.Itoa(e.Code), truncate(safeLogError(errors.New(e.Title+": "+e.Message+" "+e.ErrorData.Details)), 500), messageID)
		if err != nil {
			return err
		}
		if e.Code == 131026 {
			now := time.Now().UTC().Format(time.RFC3339Nano)
			if _, err := tx.ExecContext(ctx, `UPDATE customers SET invalid_number=1,suppressed_at=coalesce(suppressed_at,?),suppression_reason='Meta reported an undeliverable recipient',updated_at=? WHERE id=?`, now, now, customerID); err != nil {
				return err
			}
			if err := cancelCustomerWorkTx(ctx, tx, customerID, "Meta reported an undeliverable recipient", now); err != nil {
				return err
			}
		}
		if e.Code == 131050 {
			now := time.Now().UTC().Format(time.RFC3339Nano)
			result, err := tx.ExecContext(ctx, `UPDATE customers SET whatsapp_consent='opted_out',consent_updated_at=?,suppressed_at=coalesce(suppressed_at,?),suppression_reason='Meta marketing opt-out',updated_at=? WHERE id=? AND whatsapp_consent<>'opted_out'`, now, now, now, customerID)
			if err != nil {
				return err
			}
			if changed, err := result.RowsAffected(); err != nil {
				return err
			} else if changed == 1 {
				if _, err := tx.ExecContext(ctx, `INSERT INTO audit_log(occurred_at,actor,action,object_type,object_id,details_json) VALUES(?, 'meta', 'customer.opt_out', 'customer', ?, '{"source":"meta_error_131050"}')`, now, fmt.Sprint(customerID)); err != nil {
					return err
				}
			}
			if err := cancelCustomerWorkTx(ctx, tx, customerID, "Meta marketing opt-out", now); err != nil {
				return err
			}
		}
	}
	if status.Status == "failed" && !permanentDeliveryFailure {
		if err := enqueueCampaignDeliveryRetryTx(ctx, tx, messageID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func enqueueCampaignDeliveryRetryTx(ctx context.Context, tx *sql.Tx, messageID string) error {
	var jobID, campaignID, payloadJSON, scheduledAt, idempotency string
	err := tx.QueryRowContext(ctx, `SELECT m.job_id,m.campaign_id,j.payload,j.scheduled_at,j.idempotency_key
		FROM outbound_messages m JOIN scheduled_jobs j ON j.id=m.job_id
		WHERE m.id=? AND m.campaign_id IS NOT NULL`, messageID).Scan(&jobID, &campaignID, &payloadJSON, &scheduledAt, &idempotency)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var payload sendPayload
	if err := json.Unmarshal([]byte(payloadJSON), &payload); err != nil {
		return err
	}
	if payload.DeliveryRetry {
		return nil
	}
	original, err := time.Parse(time.RFC3339Nano, scheduledAt)
	if err != nil {
		return err
	}
	payload.DeliveryRetry = true
	payload.DeliveryRetryOriginalScheduledAt = original.UTC().Format(time.RFC3339Nano)
	retryPayload, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	retryAt := original.Add(24 * time.Hour).UTC().Format(time.RFC3339Nano)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err = tx.ExecContext(ctx, `INSERT INTO scheduled_jobs(id,step_id,idempotency_key,kind,payload,scheduled_at,available_at,max_attempts,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(idempotency_key) DO NOTHING`, store.NewID("job"), "delivery_retry_24h", idempotency+":delivery-retry-24h", "send_whatsapp", retryPayload, retryAt, retryAt, 1, now, now)
	return err
}

func cancelCustomerWorkTx(ctx context.Context, tx *sql.Tx, customerID int64, reason, now string) error {
	if _, err := tx.ExecContext(ctx, `UPDATE workflow_runs SET state='cancelled',cancelled_at=?,cancellation_reason=? WHERE customer_id=? AND state IN ('active','paused')`, now, reason, customerID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE scheduled_jobs SET state='cancelled',last_error=?,completed_at=?,locked_at=NULL,locked_by=NULL,updated_at=? WHERE workflow_run_id IN (SELECT id FROM workflow_runs WHERE customer_id=?) AND state IN ('scheduled','retry','paused')`, reason, now, now, customerID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE scheduled_jobs SET state='cancelled',last_error=?,completed_at=?,locked_at=NULL,locked_by=NULL,updated_at=? WHERE kind='send_whatsapp' AND CAST(json_extract(payload,'$.customer_id') AS INTEGER)=? AND state IN ('scheduled','retry','paused')`, reason, now, now, customerID)
	return err
}

func (p *Processor) recordInbound(ctx context.Context, message meta.InboundMessage) error {
	phoneHash := security.KeyedHash(p.Config.PIIHashKey, normalizePhone(message.From))
	var customerID sql.NullInt64
	_ = p.Store.DB.QueryRowContext(ctx, `SELECT id FROM customers WHERE phone_hash=?`, phoneHash).Scan(&customerID)
	body := strings.TrimSpace(message.Body())
	encrypted, err := security.Encrypt(p.Config.PIIHashKey, []byte(body))
	if err != nil {
		return err
	}
	contextID := ""
	if message.Context != nil {
		contextID = message.Context.ID
	}
	_, err = p.Store.DB.ExecContext(ctx, `INSERT INTO replies(provider_message_id,customer_id,in_reply_to_meta_message_id,received_at,message_type,body_ciphertext,body_hash) VALUES(?,?,?,?,?,?,?) ON CONFLICT(provider_message_id) DO NOTHING`, message.ID, nullableInt(customerID), nullableString(contextID), providerTime(message.Timestamp).Format(time.RFC3339Nano), message.Type, encrypted, security.KeyedHash(p.Config.PIIHashKey, strings.ToLower(body)))
	if err != nil {
		return err
	}
	if customerID.Valid && isOptOut(body) {
		now := time.Now().UTC().Format(time.RFC3339Nano)
		result, err := p.Store.DB.ExecContext(ctx, `UPDATE customers SET whatsapp_consent='opted_out',suppressed_at=coalesce(suppressed_at,?),suppression_reason='customer opt-out',updated_at=? WHERE id=? AND whatsapp_consent<>'opted_out'`, now, now, customerID.Int64)
		if err != nil {
			return err
		}
		if err = p.Store.CancelCustomerWork(ctx, customerID.Int64, "customer opt-out"); err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if changed == 1 {
			return p.Store.Audit(ctx, "webhook", "customer.opt_out", "customer", fmt.Sprint(customerID.Int64), `{"source":"inbound_reply"}`)
		}
		return nil
	}
	return nil
}

func (p *Processor) processShopifyWebhook(ctx context.Context, event store.WebhookEvent) error {
	if event.Topic == "customers/data_request" {
		request, err := shopify.DecodePrivacyRequest(event.Payload)
		if err != nil {
			return err
		}
		return p.Store.Audit(ctx, "webhook", "shopify.customer_data_requested", "customer", security.KeyedHash(p.Config.PIIHashKey, request.CustomerID), fmt.Sprintf(`{"order_count":%d}`, len(request.OrdersRequested)))
	}
	if event.Topic == "customers/redact" {
		request, err := shopify.DecodePrivacyRequest(event.Payload)
		if err != nil {
			return err
		}
		return p.redactCustomerAndOrders(ctx, request.CustomerID, request.OrderIDs, "Shopify privacy redaction")
	}
	if event.Topic == "shop/redact" {
		if _, err := shopify.DecodePrivacyRequest(event.Payload); err != nil {
			return err
		}
		return p.redactShop(ctx)
	}
	if event.Topic == "app/uninstalled" {
		now := time.Now().UTC().Format(time.RFC3339Nano)
		tx, err := p.Store.DB.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if _, err := tx.ExecContext(ctx, `UPDATE workflow_runs SET state='cancelled',cancelled_at=?,cancellation_reason='Shopify app uninstalled' WHERE state IN ('active','paused')`, now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE scheduled_jobs SET state='cancelled',last_error='Shopify app uninstalled',completed_at=?,updated_at=? WHERE state IN ('scheduled','retry','paused')`, now, now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE workflow_definitions SET active=0`); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		return p.Store.Audit(ctx, "webhook", "shopify.app_uninstalled", "shop", "", "{}")
	}
	if event.Topic == "customers/delete" {
		customerID, err := shopify.DecodeResourceID(event.Payload, "customer")
		if err != nil {
			return err
		}
		return p.redactCustomer(ctx, customerID)
	}
	if strings.HasPrefix(event.Topic, "customers/") || strings.HasPrefix(event.Topic, "customers_") {
		customerID, err := shopify.DecodeResourceID(event.Payload, "customer")
		if err != nil {
			return err
		}
		payload, _ := json.Marshal(map[string]string{"customer_id": customerID})
		_, err = p.Store.EnqueueJob(ctx, store.Job{StepID: "customer_webhook", Kind: "shopify_sync_customer", Payload: payload}, "shopify-customer:"+customerID+":"+event.EventID, time.Now())
		return err
	}
	if strings.HasPrefix(event.Topic, "products/") {
		productID, err := shopify.DecodeResourceID(event.Payload, "product")
		if err != nil {
			return err
		}
		payload, _ := json.Marshal(map[string]string{"product_id": productID})
		_, err = p.Store.EnqueueJob(ctx, store.Job{StepID: "product_webhook", Kind: "shopify_sync_product", Payload: payload}, "shopify-product:"+productID+":"+event.EventID, time.Now())
		return err
	}
	if strings.HasPrefix(event.Topic, "inventory_levels/") || strings.HasPrefix(event.Topic, "variants/in_") || strings.HasPrefix(event.Topic, "variants/out_") {
		itemID, err := shopify.DecodeResourceID(event.Payload, "inventory_item")
		if err != nil {
			return err
		}
		payload, _ := json.Marshal(map[string]string{"inventory_item_id": itemID})
		_, err = p.Store.EnqueueJob(ctx, store.Job{StepID: "inventory_webhook", Kind: "shopify_sync_inventory", Payload: payload}, "shopify-inventory:"+itemID+":"+event.EventID, time.Now())
		return err
	}
	orderTopics := strings.HasPrefix(event.Topic, "orders/") || strings.HasPrefix(event.Topic, "refunds/") || strings.HasPrefix(event.Topic, "fulfillments/") || strings.HasPrefix(event.Topic, "returns/")
	if orderTopics {
		orderID, err := shopify.DecodeWebhookOrder(event.Payload)
		if err != nil {
			payload, _ := json.Marshal(map[string]string{"since": time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339)})
			_, err = p.Store.EnqueueJob(ctx, store.Job{StepID: "webhook_fallback", Kind: "shopify_reconcile", Payload: payload}, "shopify-reconcile-event:"+event.EventID, time.Now())
			return err
		}
		payload, _ := json.Marshal(map[string]string{"order_id": orderID})
		_, err = p.Store.EnqueueJob(ctx, store.Job{StepID: "shopify_webhook", Kind: "shopify_sync_order", Payload: payload}, "shopify-order:"+orderID+":"+event.EventID, time.Now())
		return err
	}
	payload, _ := json.Marshal(map[string]string{"topic": event.Topic})
	_, err := p.Store.EnqueueJob(ctx, store.Job{StepID: "shopify_webhook", Kind: "shopify_reconcile", Payload: payload}, "shopify-resource:"+event.EventID, time.Now())
	return err
}

func (p *Processor) processJob(ctx context.Context, job store.Job) error {
	switch job.Kind {
	case "shopify_sync_order":
		var payload struct {
			OrderID string `json:"order_id"`
		}
		if json.Unmarshal(job.Payload, &payload) != nil || payload.OrderID == "" {
			return errors.New("invalid Shopify order job")
		}
		order, err := p.Shopify.OrderByID(ctx, payload.OrderID)
		if err != nil {
			return err
		}
		return p.upsertOrder(ctx, order)
	case "shopify_reconcile":
		return p.reconcile(ctx, job.Payload)
	case "shopify_sync_product":
		var payload struct {
			ProductID string `json:"product_id"`
		}
		if json.Unmarshal(job.Payload, &payload) != nil || payload.ProductID == "" {
			return errors.New("invalid Shopify product job")
		}
		product, err := p.Shopify.ProductByID(ctx, payload.ProductID)
		if err != nil {
			return err
		}
		return p.upsertProduct(ctx, product)
	case "shopify_sync_inventory":
		var payload struct {
			InventoryItemID string `json:"inventory_item_id"`
		}
		if json.Unmarshal(job.Payload, &payload) != nil || payload.InventoryItemID == "" {
			return errors.New("invalid Shopify inventory job")
		}
		item, err := p.Shopify.InventoryItemByID(ctx, payload.InventoryItemID)
		if err != nil {
			return err
		}
		return p.upsertInventory(ctx, item)
	case "shopify_sync_customer":
		var payload struct {
			CustomerID string `json:"customer_id"`
		}
		if json.Unmarshal(job.Payload, &payload) != nil || payload.CustomerID == "" {
			return errors.New("invalid Shopify customer job")
		}
		customer, err := p.Shopify.CustomerByID(ctx, payload.CustomerID)
		if err != nil {
			return err
		}
		return p.upsertCustomer(ctx, customer)
	case "send_whatsapp":
		return p.sendWhatsApp(ctx, job)
	default:
		return fmt.Errorf("unsupported job kind %q", job.Kind)
	}
}

func (p *Processor) reconcile(ctx context.Context, payload []byte) error {
	var input struct {
		Since     string `json:"since"`
		Watermark string `json:"watermark"`
	}
	_ = json.Unmarshal(payload, &input)
	since := time.Now().Add(-24 * time.Hour)
	if input.Since != "" {
		if parsed, err := time.Parse(time.RFC3339, input.Since); err == nil {
			since = parsed
		}
	}
	if err := p.reconcileOrders(ctx, since); err != nil {
		return err
	}
	if err := p.reconcileCustomers(ctx, since); err != nil {
		return err
	}
	if err := p.reconcileProducts(ctx, since); err != nil {
		return err
	}
	if err := p.reconcileKnownInventory(ctx); err != nil {
		return err
	}
	if input.Watermark != "" {
		watermark, err := time.Parse(time.RFC3339Nano, input.Watermark)
		if err != nil {
			return fmt.Errorf("invalid reconciliation watermark: %w", err)
		}
		return p.Store.SetSyncWatermark(ctx, "shopify", "catalog", watermark)
	}
	return nil
}

func (p *Processor) reconcileOrders(ctx context.Context, since time.Time) error {
	cursor := ""
	for pages := 0; pages < maxReconciliationPages; pages++ {
		page, err := p.Shopify.OrdersUpdatedSince(ctx, since, cursor)
		if err != nil {
			return err
		}
		for _, order := range page.Orders.Nodes {
			if err := p.upsertOrder(ctx, order); err != nil {
				return err
			}
		}
		if !page.Orders.PageInfo.HasNextPage {
			return nil
		}
		cursor = page.Orders.PageInfo.EndCursor
		if err := waitForNextShopifyPage(ctx); err != nil {
			return err
		}
	}
	return fmt.Errorf("Shopify order reconciliation exceeded %d pages", maxReconciliationPages)
}

func (p *Processor) reconcileCustomers(ctx context.Context, since time.Time) error {
	cursor := ""
	for pages := 0; pages < maxReconciliationPages; pages++ {
		page, err := p.Shopify.CustomersUpdatedSince(ctx, since, cursor)
		if err != nil {
			return err
		}
		for _, customer := range page.Customers.Nodes {
			if err := p.upsertCustomer(ctx, customer); err != nil {
				return err
			}
		}
		if !page.Customers.PageInfo.HasNextPage {
			return nil
		}
		cursor = page.Customers.PageInfo.EndCursor
		if err := waitForNextShopifyPage(ctx); err != nil {
			return err
		}
	}
	return fmt.Errorf("Shopify customer reconciliation exceeded %d pages", maxReconciliationPages)
}

func (p *Processor) reconcileProducts(ctx context.Context, since time.Time) error {
	cursor := ""
	for pages := 0; pages < maxReconciliationPages; pages++ {
		page, err := p.Shopify.ProductsUpdatedSince(ctx, since, cursor)
		if err != nil {
			return err
		}
		for _, product := range page.Products.Nodes {
			if err := p.upsertProduct(ctx, product); err != nil {
				return err
			}
		}
		if !page.Products.PageInfo.HasNextPage {
			return nil
		}
		cursor = page.Products.PageInfo.EndCursor
		if err := waitForNextShopifyPage(ctx); err != nil {
			return err
		}
	}
	return fmt.Errorf("Shopify product reconciliation exceeded %d pages", maxReconciliationPages)
}

const maxReconciliationPages = 1000

func waitForNextShopifyPage(ctx context.Context) error {
	timer := time.NewTimer(250 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (p *Processor) reconcileKnownInventory(ctx context.Context) error {
	rows, err := p.Store.DB.QueryContext(ctx, `SELECT DISTINCT inventory_item_id FROM variants WHERE inventory_item_id IS NOT NULL ORDER BY inventory_item_id`)
	if err != nil {
		return err
	}
	var itemIDs []string
	for rows.Next() {
		var itemID string
		if err := rows.Scan(&itemID); err != nil {
			rows.Close()
			return err
		}
		itemIDs = append(itemIDs, itemID)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, itemID := range itemIDs {
		item, err := p.Shopify.InventoryItemByID(ctx, itemID)
		if err != nil {
			return err
		}
		if err := p.upsertInventory(ctx, item); err != nil {
			return err
		}
	}
	return nil
}

func (p *Processor) scrubWebhook(ctx context.Context, event store.WebhookEvent) {
	digest := sha256.Sum256(event.Payload)
	_, _ = p.Store.DB.ExecContext(ctx, `UPDATE webhook_events SET payload=? WHERE provider=? AND event_id=? AND status='processed'`, []byte(hex.EncodeToString(digest[:])), event.Provider, event.EventID)
}
func providerTime(value string) time.Time {
	seconds, err := strconv.ParseInt(value, 10, 64)
	if err == nil {
		return time.Unix(seconds, 0).UTC()
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err == nil {
		return parsed.UTC()
	}
	return time.Now().UTC()
}
func nullableTime(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value.UTC().Format(time.RFC3339Nano)
}
func nullableInt(value sql.NullInt64) any {
	if !value.Valid {
		return nil
	}
	return value.Int64
}
func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}
// normalizePhone is the single canonical form for a phone number across every
// producer: Shopify sync, inbound replies, consent import, and the external
// recorder. They must agree, or the same person hashes to two identities and a
// STOP recorded against one keeps receiving campaigns aimed at the other.
func normalizePhone(value string) string {
	var digits strings.Builder
	for _, r := range value {
		if r >= '0' && r <= '9' {
			digits.WriteRune(r)
		}
	}
	normalized := digits.String()
	return strings.TrimPrefix(normalized, "00")
}
func isOptOut(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "stop", "unsubscribe", "opt out", "optout", "cancel", "band":
		return true
	}
	return false
}
func truncate(value string, max int) string {
	value = strings.TrimSpace(value)
	if len(value) > max {
		return value[:max]
	}
	return value
}

var _ = workflow.Definition{}

var (
	logPhonePattern = regexp.MustCompile(`\+?\d[\d\s().-]{8,}\d`)
	logTokenPattern = regexp.MustCompile(`\b(?:EA|shpss_|shpat_|shpca_)[A-Za-z0-9_-]{20,}\b`)
)

func safeLogError(err error) string {
	if err == nil {
		return ""
	}
	value := logPhonePattern.ReplaceAllString(err.Error(), "[redacted-number]")
	return logTokenPattern.ReplaceAllString(value, "[redacted-secret]")
}

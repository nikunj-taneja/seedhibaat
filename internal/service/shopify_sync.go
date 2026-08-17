package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nikunj-taneja/seedhibaat/internal/security"
	"github.com/nikunj-taneja/seedhibaat/internal/shopify"
	"github.com/nikunj-taneja/seedhibaat/internal/store"
	"github.com/nikunj-taneja/seedhibaat/internal/workflow"
)

func (p *Processor) upsertOrder(ctx context.Context, order shopify.Order) error {
	tx, err := p.Store.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	var customerID sql.NullInt64
	if order.Customer != nil && order.Customer.ID != "" {
		customer := *order.Customer
		if customer.EffectivePhone() == "" {
			// The order carries the buyer's number even when Shopify withholds
			// it on the customer record.
			customer.Phone = order.EffectiveCustomerPhone()
		}
		customerID, err = p.upsertCustomerTx(ctx, tx, customer, now)
		if err != nil {
			return err
		}
	}
	amount, err := shopify.AmountMinor(order.CurrentTotalPriceSet.ShopMoney.Amount)
	if err != nil {
		return err
	}
	var previousDelivery sql.NullString
	_ = tx.QueryRowContext(ctx, `SELECT fully_delivered_at FROM orders WHERE shopify_id=?`, order.ID).Scan(&previousDelivery)
	deliveredAt := order.FullyDeliveredAt()
	var deliveredValue any
	if deliveredAt != nil {
		deliveredValue = deliveredAt.UTC().Format(time.RFC3339Nano)
	}
	var cancelled any
	if order.CancelledAt != nil {
		cancelled = *order.CancelledAt
	}
	var refundedAt any
	if len(order.Refunds) > 0 {
		refundedAt = order.Refunds[len(order.Refunds)-1].CreatedAt
	}
	var returnRecordedAt any
	if hasOpenReturn(order) {
		returnRecordedAt = order.UpdatedAt
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO orders(shopify_id,customer_id,order_number,processed_at,cancelled_at,financial_status,currency,total_amount_minor,fully_delivered_at,refunded_at,return_recorded_at,raw_updated_at,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(shopify_id) DO UPDATE SET customer_id=excluded.customer_id,order_number=excluded.order_number,processed_at=excluded.processed_at,cancelled_at=excluded.cancelled_at,financial_status=excluded.financial_status,currency=excluded.currency,total_amount_minor=excluded.total_amount_minor,fully_delivered_at=excluded.fully_delivered_at,refunded_at=excluded.refunded_at,return_recorded_at=coalesce(orders.return_recorded_at,excluded.return_recorded_at),raw_updated_at=excluded.raw_updated_at,updated_at=excluded.updated_at`, order.ID, nullableInt(customerID), order.Name, order.ProcessedAt, cancelled, order.DisplayFinancialStatus, order.CurrencyCode, amount, deliveredValue, refundedAt, returnRecordedAt, order.UpdatedAt, now, now)
	if err != nil {
		return err
	}
	refunded := order.RefundedQuantities()
	deliveredQuantities := map[string]int{}
	for _, fulfillment := range order.Fulfillments {
		if fulfillment.DeliveredAt != nil {
			for _, line := range fulfillment.FulfillmentLineItems.Nodes {
				deliveredQuantities[line.LineItem.ID] += line.Quantity
			}
		}
	}
	for _, line := range order.LineItems.Nodes {
		var productID, variantID any
		if line.Product != nil {
			productID = line.Product.ID
			tags, _ := json.Marshal(line.Product.Tags)
			_, err = tx.ExecContext(ctx, `INSERT INTO products(shopify_id,title,handle,product_type,status,tags_json,updated_at) VALUES(?,?,?,?,?,?,?) ON CONFLICT(shopify_id) DO UPDATE SET title=excluded.title,handle=excluded.handle,product_type=excluded.product_type,status=excluded.status,tags_json=excluded.tags_json,updated_at=excluded.updated_at`, line.Product.ID, line.Product.Title, line.Product.Handle, line.Product.ProductType, line.Product.Status, string(tags), now)
			if err != nil {
				return err
			}
		}
		if line.Variant != nil {
			variantID = line.Variant.ID
			if line.Product != nil {
				var inventoryItemID any
				if line.Variant.InventoryItem != nil {
					inventoryItemID = line.Variant.InventoryItem.ID
				}
				_, err = tx.ExecContext(ctx, `INSERT INTO variants(shopify_id,product_id,title,sku,inventory_item_id,inventory_quantity,updated_at) VALUES(?,?,?,?,?,?,?) ON CONFLICT(shopify_id) DO UPDATE SET title=excluded.title,sku=excluded.sku,inventory_item_id=coalesce(excluded.inventory_item_id,variants.inventory_item_id),inventory_quantity=excluded.inventory_quantity,updated_at=excluded.updated_at`, line.Variant.ID, line.Product.ID, line.Variant.Title, line.Variant.SKU, inventoryItemID, line.Variant.InventoryQuantity, now)
				if err != nil {
					return err
				}
			}
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO order_lines(shopify_id,order_id,product_id,variant_id,title,sku,quantity,current_quantity,delivered_quantity,refunded_quantity) VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(shopify_id) DO UPDATE SET product_id=excluded.product_id,variant_id=excluded.variant_id,title=excluded.title,sku=excluded.sku,quantity=excluded.quantity,current_quantity=excluded.current_quantity,delivered_quantity=excluded.delivered_quantity,refunded_quantity=excluded.refunded_quantity`, line.ID, order.ID, productID, variantID, line.Title, line.SKU, line.Quantity, line.CurrentQuantity, deliveredQuantities[line.ID], refunded[line.ID])
		if err != nil {
			return err
		}
	}
	for _, fulfillment := range order.Fulfillments {
		var at any
		if fulfillment.DeliveredAt != nil {
			at = *fulfillment.DeliveredAt
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO fulfillments(shopify_id,order_id,status,delivered_at,updated_at) VALUES(?,?,?,?,?) ON CONFLICT(shopify_id) DO UPDATE SET status=excluded.status,delivered_at=excluded.delivered_at,updated_at=excluded.updated_at`, fulfillment.ID, order.ID, fulfillment.Status, at, fulfillment.UpdatedAt)
		if err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if customerID.Valid {
		var effectiveConsent string
		if err := p.Store.DB.QueryRowContext(ctx, `SELECT whatsapp_consent FROM customers WHERE id=?`, customerID.Int64).Scan(&effectiveConsent); err != nil {
			return err
		}
		if effectiveConsent == "opted_out" {
			if err := p.Store.CancelCustomerWork(ctx, customerID.Int64, "Shopify WhatsApp opt-out"); err != nil {
				return err
			}
		}
	}
	if customerID.Valid && (order.CancelledAt != nil || len(order.Refunds) > 0 || hasOpenReturn(order)) {
		_, _ = p.Store.DB.ExecContext(ctx, `DELETE FROM conversions WHERE order_id=?`, order.ID)
		reason := "Shopify order cancellation/refund/return"
		if err := p.Store.CancelCustomerWork(ctx, customerID.Int64, reason); err != nil {
			return err
		}
	}
	if customerID.Valid && order.CancelledAt == nil && len(order.Refunds) == 0 && !hasOpenReturn(order) {
		if err := p.cancelConvertedWorkflows(ctx, customerID.Int64, order); err != nil {
			return err
		}
	}
	if customerID.Valid && order.CancelledAt == nil && len(order.Refunds) == 0 && !hasOpenReturn(order) {
		if err := p.attributeConversion(ctx, customerID.Int64, order, amount); err != nil {
			return err
		}
	}
	if customerID.Valid && deliveredAt != nil && !previousDelivery.Valid && order.CancelledAt == nil && len(order.Refunds) == 0 && !hasOpenReturn(order) {
		return p.startDeliveredWorkflows(ctx, customerID.Int64, order, *deliveredAt)
	}
	return nil
}

func (p *Processor) upsertCustomer(ctx context.Context, customer shopify.Customer) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	tx, err := p.Store.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	customerID, err := p.upsertCustomerTx(ctx, tx, customer, now)
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if customerID.Valid {
		var effectiveConsent string
		if err := p.Store.DB.QueryRowContext(ctx, `SELECT whatsapp_consent FROM customers WHERE id=?`, customerID.Int64).Scan(&effectiveConsent); err != nil {
			return err
		}
		if effectiveConsent == "opted_out" {
			return p.Store.CancelCustomerWork(ctx, customerID.Int64, "Shopify WhatsApp opt-out")
		}
	}
	return nil
}

func (p *Processor) redactCustomer(ctx context.Context, shopifyID string) error {
	return p.redactCustomerAndOrders(ctx, shopifyID, nil, "Shopify customer deletion")
}

func (p *Processor) redactCustomerAndOrders(ctx context.Context, shopifyID string, requestedOrderIDs []string, reason string) error {
	tx, err := p.Store.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var customerID int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM customers WHERE shopify_id=?`, shopifyID).Scan(&customerID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil {
		if _, err = tx.ExecContext(ctx, `DELETE FROM conversions WHERE message_id IN (SELECT id FROM outbound_messages WHERE customer_id=?) OR order_id IN (SELECT shopify_id FROM orders WHERE customer_id=?)`, customerID, customerID); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM tracked_links WHERE message_id IN (SELECT id FROM outbound_messages WHERE customer_id=?)`, customerID); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM message_events WHERE message_id IN (SELECT id FROM outbound_messages WHERE customer_id=?)`, customerID); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM outbound_messages WHERE customer_id=?`, customerID); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM scheduled_jobs WHERE workflow_run_id IN (SELECT id FROM workflow_runs WHERE customer_id=?) OR json_extract(payload,'$.customer_id')=?`, customerID, customerID); err != nil {
			return err
		}
		for _, query := range []string{
			`DELETE FROM workflow_runs WHERE customer_id=?`,
			`DELETE FROM campaign_recipients WHERE customer_id=?`,
			`DELETE FROM replies WHERE customer_id=?`,
			`DELETE FROM frequency_caps WHERE customer_id=?`,
			`DELETE FROM orders WHERE customer_id=?`,
			`DELETE FROM customers WHERE id=?`,
		} {
			if _, err = tx.ExecContext(ctx, query, customerID); err != nil {
				return err
			}
		}
	}
	for _, orderID := range requestedOrderIDs {
		if _, err = tx.ExecContext(ctx, `DELETE FROM conversions WHERE order_id=?`, orderID); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM orders WHERE shopify_id=?`, orderID); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return p.Store.Audit(ctx, "webhook", "shopify.customer_redacted", "customer", security.KeyedHash(p.Config.PIIHashKey, shopifyID), fmt.Sprintf(`{"reason":%q,"requested_order_count":%d}`, reason, len(requestedOrderIDs)))
}

func (p *Processor) redactShop(ctx context.Context) error {
	tx, err := p.Store.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, table := range []string{
		"message_events", "tracked_links", "conversions", "replies",
		"campaign_recipients", "outbound_messages", "scheduled_jobs", "workflow_runs",
		"campaigns", "frequency_caps", "fulfillments", "order_lines", "orders",
		"inventory_levels", "variants", "products", "customers", "sync_cursors", "audit_log",
	} {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+table); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE workflow_definitions SET active=0`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE webhook_events SET payload=x''`); err != nil {
		return err
	}
	return tx.Commit()
}

func (p *Processor) upsertCustomerTx(ctx context.Context, tx *sql.Tx, customer shopify.Customer, now string) (sql.NullInt64, error) {
	var previousConsent string
	previousExists := true
	if err := tx.QueryRowContext(ctx, `SELECT whatsapp_consent FROM customers WHERE shopify_id=?`, customer.ID).Scan(&previousConsent); errors.Is(err, sql.ErrNoRows) {
		previousExists = false
	} else if err != nil {
		return sql.NullInt64{}, err
	}
	phone := normalizePhone(customer.EffectivePhone(), p.Config.DefaultCountryCode)
	var phoneHash any
	var phoneCiphertext any
	if phone != "" {
		phoneHash = security.KeyedHash(p.Config.PIIHashKey, phone)
		encrypted, err := security.Encrypt(p.Config.PIIHashKey, []byte(phone))
		if err != nil {
			return sql.NullInt64{}, err
		}
		phoneCiphertext = encrypted
	}
	first, err := security.Encrypt(p.Config.PIIHashKey, []byte(customer.FirstName))
	if err != nil {
		return sql.NullInt64{}, err
	}
	last, err := security.Encrypt(p.Config.PIIHashKey, []byte(customer.LastName))
	if err != nil {
		return sql.NullInt64{}, err
	}
	consent, consentAt := consentFromCustomer(customer)
	_, err = tx.ExecContext(ctx, `INSERT INTO customers(shopify_id,phone_ciphertext,phone_hash,first_name_ciphertext,last_name_ciphertext,whatsapp_consent,consent_updated_at,suppressed_at,suppression_reason,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(shopify_id) DO UPDATE SET
		phone_ciphertext=coalesce(excluded.phone_ciphertext,customers.phone_ciphertext),phone_hash=coalesce(excluded.phone_hash,customers.phone_hash),first_name_ciphertext=excluded.first_name_ciphertext,last_name_ciphertext=excluded.last_name_ciphertext,
		whatsapp_consent=CASE WHEN excluded.whatsapp_consent='unknown' OR (customers.consent_updated_at IS NOT NULL AND excluded.consent_updated_at IS NOT NULL AND excluded.consent_updated_at<customers.consent_updated_at) THEN customers.whatsapp_consent ELSE excluded.whatsapp_consent END,
		consent_updated_at=CASE WHEN excluded.whatsapp_consent='unknown' OR (customers.consent_updated_at IS NOT NULL AND excluded.consent_updated_at IS NOT NULL AND excluded.consent_updated_at<customers.consent_updated_at) THEN customers.consent_updated_at ELSE coalesce(excluded.consent_updated_at,customers.consent_updated_at) END,
		suppressed_at=CASE WHEN (customers.consent_updated_at IS NOT NULL AND excluded.consent_updated_at IS NOT NULL AND excluded.consent_updated_at<customers.consent_updated_at) THEN customers.suppressed_at WHEN excluded.whatsapp_consent='opted_out' THEN coalesce(customers.suppressed_at,excluded.suppressed_at) WHEN excluded.whatsapp_consent='opted_in' AND customers.invalid_number=0 THEN NULL ELSE customers.suppressed_at END,
		suppression_reason=CASE WHEN (customers.consent_updated_at IS NOT NULL AND excluded.consent_updated_at IS NOT NULL AND excluded.consent_updated_at<customers.consent_updated_at) THEN customers.suppression_reason WHEN excluded.whatsapp_consent='opted_out' THEN 'Shopify WhatsApp opt-out' WHEN excluded.whatsapp_consent='opted_in' AND customers.invalid_number=0 THEN NULL ELSE customers.suppression_reason END,
		updated_at=excluded.updated_at`, customer.ID, phoneCiphertext, phoneHash, first, last, consent, consentAt, nullableConsentSuppression(consent, now), nullableConsentReason(consent), now, now)
	if err != nil {
		return sql.NullInt64{}, err
	}
	var customerID sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM customers WHERE shopify_id=?`, customer.ID).Scan(&customerID); err != nil {
		return sql.NullInt64{}, err
	}
	var effectiveConsent string
	if err := tx.QueryRowContext(ctx, `SELECT whatsapp_consent FROM customers WHERE id=?`, customerID.Int64).Scan(&effectiveConsent); err != nil {
		return sql.NullInt64{}, err
	}
	if previousExists && previousConsent != "opted_out" && effectiveConsent == "opted_out" {
		if _, err := tx.ExecContext(ctx, `INSERT INTO audit_log(occurred_at,actor,action,object_type,object_id,details_json) VALUES(?, 'shopify', 'customer.opt_out', 'customer', ?, '{"source":"shopify_whatsapp_consent"}')`, now, fmt.Sprint(customerID.Int64)); err != nil {
			return sql.NullInt64{}, err
		}
	}
	return customerID, nil
}

func consentFromCustomer(customer shopify.Customer) (string, any) {
	state, updatedAt := customer.WhatsAppConsent()
	switch strings.ToUpper(strings.TrimSpace(state)) {
	case "SUBSCRIBED":
		return "opted_in", nullableConsentTimestamp(updatedAt)
	case "UNSUBSCRIBED", "REDACTED":
		return "opted_out", nullableConsentTimestamp(updatedAt)
	case "PENDING", "NEVER_SUBSCRIBED":
		return "not_opted_in", nullableConsentTimestamp(updatedAt)
	case "":
		return consentFromTags(customer.Tags)
	default:
		return "not_opted_in", nullableConsentTimestamp(updatedAt)
	}
}

func consentFromTags(tags []string) (string, any) {
	for _, tag := range tags {
		switch strings.ToLower(strings.TrimSpace(tag)) {
		case "whatsapp-opt-out", "whatsapp_opt_out":
			return "opted_out", time.Now().UTC().Format(time.RFC3339Nano)
		}
	}
	for _, tag := range tags {
		switch strings.ToLower(strings.TrimSpace(tag)) {
		case "whatsapp-opt-in", "whatsapp_opt_in", "whatsapp-consent":
			return "opted_in", time.Now().UTC().Format(time.RFC3339Nano)
		}
	}
	return "unknown", nil
}

func nullableConsentTimestamp(value string) any {
	if value == "" {
		return time.Now().UTC().Format(time.RFC3339Nano)
	}
	if parsed, err := time.Parse(time.RFC3339, value); err == nil {
		return parsed.UTC().Format(time.RFC3339Nano)
	}
	return time.Now().UTC().Format(time.RFC3339Nano)
}
func nullableConsentSuppression(consent, now string) any {
	if consent == "opted_out" {
		return now
	}
	return nil
}
func nullableConsentReason(consent string) any {
	if consent == "opted_out" {
		return "Shopify WhatsApp opt-out"
	}
	return nil
}
func hasOpenReturn(order shopify.Order) bool {
	for _, r := range order.Returns.Nodes {
		if r.Status != "DECLINED" && r.Status != "CANCELED" && r.Status != "CANCELLED" {
			return true
		}
	}
	return false
}
func (p *Processor) cancelConvertedWorkflows(ctx context.Context, customerID int64, order shopify.Order) error {
	rows, err := p.Store.DB.QueryContext(ctx, `SELECT wr.id,wd.yaml FROM workflow_runs wr JOIN workflow_definitions wd ON wd.name=wr.workflow_name AND wd.version=wr.workflow_version WHERE wr.customer_id=? AND wr.state IN ('active','paused')`, customerID)
	if err != nil {
		return err
	}
	type candidate struct{ runID, body string }
	var candidates []candidate
	for rows.Next() {
		var item candidate
		if err := rows.Scan(&item.runID, &item.body); err != nil {
			rows.Close()
			return err
		}
		candidates = append(candidates, item)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, item := range candidates {
		loaded, err := workflow.Parse("database conversion", []byte(item.body))
		if err != nil {
			return err
		}
		if matchesConversion(order, loaded.Definition.Conversion) {
			if err := p.Store.CancelWorkflow(ctx, item.runID, "customer completed configured conversion"); err != nil {
				return err
			}
		}
	}
	return nil
}

func matchesConversion(order shopify.Order, conversion workflow.Conversion) bool {
	for _, line := range order.LineItems.Nodes {
		for _, title := range conversion.ProductTitles {
			if strings.Contains(strings.ToLower(line.Title), strings.ToLower(title)) {
				return true
			}
		}
		if line.Product == nil {
			continue
		}
		for _, handle := range conversion.ProductHandles {
			if strings.EqualFold(handle, line.Product.Handle) {
				return true
			}
		}
		for _, title := range conversion.ProductTitles {
			if strings.Contains(strings.ToLower(line.Product.Title), strings.ToLower(title)) {
				return true
			}
		}
		for _, tag := range line.Product.Tags {
			for _, wanted := range conversion.ProductTags {
				if strings.EqualFold(tag, wanted) {
					return true
				}
			}
		}
	}
	return false
}

func (p *Processor) startDeliveredWorkflows(ctx context.Context, customerID int64, order shopify.Order, deliveredAt time.Time) error {
	rows, err := p.Store.DB.QueryContext(ctx, `SELECT name,version,yaml FROM workflow_definitions WHERE active=1`)
	if err != nil {
		return err
	}
	type definitionRow struct {
		name, body string
		version    int
	}
	var definitions []definitionRow
	for rows.Next() {
		var item definitionRow
		if err := rows.Scan(&item.name, &item.version, &item.body); err != nil {
			rows.Close()
			return err
		}
		definitions = append(definitions, item)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, item := range definitions {
		name, version, body := item.name, item.version, item.body
		loaded, err := workflow.Parse("database:"+name, []byte(body))
		if err != nil {
			return err
		}
		definition := loaded.Definition
		if definition.Trigger.Type != "order_delivered" || !matchesAudience(order, definition.Audience) {
			continue
		}
		var consent string
		var suppressed sql.NullString
		if err := p.Store.DB.QueryRowContext(ctx, `SELECT whatsapp_consent,suppressed_at FROM customers WHERE id=?`, customerID).Scan(&consent, &suppressed); err != nil {
			return err
		}
		if definition.Audience.RequireConsent && (consent != "opted_in" || suppressed.Valid) {
			continue
		}
		runID := store.NewID("run")
		result, err := p.Store.DB.ExecContext(ctx, `INSERT INTO workflow_runs(id,workflow_name,workflow_version,customer_id,trigger_type,trigger_id,state,started_at) VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(workflow_name,workflow_version,customer_id,trigger_type,trigger_id) DO NOTHING`, runID, name, version, customerID, "order_delivered", order.ID, "active", time.Now().UTC().Format(time.RFC3339Nano))
		if err != nil {
			return err
		}
		inserted, _ := result.RowsAffected()
		if inserted == 0 {
			continue
		}
		for _, step := range definition.Steps {
			delay, _ := workflow.ParseWait(step.Wait)
			at, err := workflow.NextAllowedTime(deliveredAt.Add(delay), definition.Timezone, definition.QuietHours.Start, definition.QuietHours.End)
			if err != nil {
				return err
			}
			payload, _ := json.Marshal(sendPayload{CustomerID: customerID, RunID: runID, Template: step.Template, Language: step.Language, Category: step.Category, TrackedURL: step.URL, HeaderImageURL: step.HeaderImageURL, Params: step.Params, Conditions: step.Conditions, FrequencyMessages: definition.Frequency.Messages, FrequencyWindow: definition.Frequency.Window, Timezone: definition.Timezone, QuietStart: definition.QuietHours.Start, QuietEnd: definition.QuietHours.End})
			job := store.Job{ID: store.NewID("job"), WorkflowRunID: sql.NullString{String: runID, Valid: true}, StepID: step.ID, Kind: "send_whatsapp", Payload: payload, MaxAttempts: 8}
			key := fmt.Sprintf("workflow:%s:v%d:%d:%s:%s", name, version, customerID, order.ID, step.ID)
			if _, err := p.Store.EnqueueJob(ctx, job, key, at); err != nil {
				return err
			}
		}
	}
	return nil
}

func matchesAudience(order shopify.Order, a workflow.Audience) bool {
	for _, line := range order.LineItems.Nodes {
		if line.Product == nil {
			continue
		}
		for _, v := range a.ProductHandles {
			if strings.EqualFold(v, line.Product.Handle) {
				return true
			}
		}
		for _, v := range a.ProductTitles {
			if strings.Contains(strings.ToLower(line.Product.Title), strings.ToLower(v)) {
				return true
			}
		}
		for _, tag := range line.Product.Tags {
			for _, v := range a.ProductTags {
				if strings.EqualFold(tag, v) {
					return true
				}
			}
		}
	}
	return false
}

func (p *Processor) attributeConversion(ctx context.Context, customerID int64, order shopify.Order, amount int64) error {
	orderTime, err := time.Parse(time.RFC3339, order.ProcessedAt)
	if err != nil {
		return fmt.Errorf("invalid Shopify order processedAt: %w", err)
	}
	var messageID, runID, campaignID sql.NullString
	window := p.Config.AttributionWindow
	if window <= 0 {
		window = 30 * 24 * time.Hour
	}
	err = p.Store.DB.QueryRowContext(ctx, `SELECT id,workflow_run_id,campaign_id FROM outbound_messages WHERE customer_id=? AND read_at IS NOT NULL AND read_at<=? AND read_at>=? ORDER BY read_at DESC LIMIT 1`, customerID, orderTime.UTC().Format(time.RFC3339Nano), orderTime.Add(-window).UTC().Format(time.RFC3339Nano)).Scan(&messageID, &runID, &campaignID)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil
		}
		return err
	}
	model := "last_read_touch_" + window.String()
	_, err = p.Store.DB.ExecContext(ctx, `INSERT INTO conversions(order_id,message_id,campaign_id,workflow_run_id,attributed_at,amount_minor,currency,attribution_model) VALUES(?,?,?,?,?,?,?,?) ON CONFLICT DO NOTHING`, order.ID, messageID.String, nullableString(campaignID.String), nullableString(runID.String), time.Now().UTC().Format(time.RFC3339Nano), amount, order.CurrencyCode, model)
	return err
}

func (p *Processor) upsertProduct(ctx context.Context, product shopify.ProductDetail) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	tags, _ := json.Marshal(product.Tags)
	tx, err := p.Store.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	transitioned := false
	_, err = tx.ExecContext(ctx, `INSERT INTO products(shopify_id,title,handle,product_type,status,tags_json,updated_at) VALUES(?,?,?,?,?,?,?) ON CONFLICT(shopify_id) DO UPDATE SET title=excluded.title,handle=excluded.handle,product_type=excluded.product_type,status=excluded.status,tags_json=excluded.tags_json,updated_at=excluded.updated_at`, product.ID, product.Title, product.Handle, product.ProductType, product.Status, string(tags), now)
	if err != nil {
		return err
	}
	for _, variant := range product.Variants.Nodes {
		var previous int
		if err := tx.QueryRowContext(ctx, `SELECT inventory_quantity FROM variants WHERE shopify_id=?`, variant.ID).Scan(&previous); err == nil && previous <= 0 && variant.InventoryQuantity > 0 {
			transitioned = true
		}
		var item any
		if variant.InventoryItem != nil {
			item = variant.InventoryItem.ID
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO variants(shopify_id,product_id,title,sku,inventory_item_id,inventory_quantity,updated_at) VALUES(?,?,?,?,?,?,?) ON CONFLICT(shopify_id) DO UPDATE SET title=excluded.title,sku=excluded.sku,inventory_item_id=excluded.inventory_item_id,inventory_quantity=excluded.inventory_quantity,updated_at=excluded.updated_at`, variant.ID, product.ID, variant.Title, variant.SKU, item, variant.InventoryQuantity, now)
		if err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if transitioned {
		return p.startInventoryWorkflows(ctx, product.Product, time.Now())
	}
	return nil
}

func (p *Processor) upsertInventory(ctx context.Context, item shopify.InventoryItem) error {
	if item.Variant == nil || item.Variant.Product == nil {
		return errors.New("inventory item is not connected to a product variant")
	}
	product := shopify.ProductDetail{Product: *item.Variant.Product}
	product.Variants.Nodes = []shopify.Variant{item.Variant.Variant}
	if err := p.upsertProduct(ctx, product); err != nil {
		return err
	}
	tx, err := p.Store.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, level := range item.InventoryLevels.Nodes {
		available := 0
		for _, quantity := range level.Quantities {
			if quantity.Name == "available" {
				available = quantity.Quantity
			}
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO inventory_levels(inventory_item_id,location_id,variant_id,available,updated_at) VALUES(?,?,?,?,?) ON CONFLICT(inventory_item_id,location_id) DO UPDATE SET variant_id=excluded.variant_id,available=excluded.available,updated_at=excluded.updated_at`, item.ID, level.Location.ID, item.Variant.ID, available, level.UpdatedAt)
		if err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	_ = now
	return nil
}

func (p *Processor) startInventoryWorkflows(ctx context.Context, product shopify.Product, triggeredAt time.Time) error {
	triggerID := product.ID + ":" + triggeredAt.UTC().Format(time.RFC3339Nano)
	rows, err := p.Store.DB.QueryContext(ctx, `SELECT name,version,yaml FROM workflow_definitions WHERE active=1`)
	if err != nil {
		return err
	}
	type definitionRow struct {
		name, body string
		version    int
	}
	var definitions []definitionRow
	for rows.Next() {
		var item definitionRow
		if err := rows.Scan(&item.name, &item.version, &item.body); err != nil {
			rows.Close()
			return err
		}
		definitions = append(definitions, item)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, item := range definitions {
		name, version, body := item.name, item.version, item.body
		loaded, err := workflow.Parse("database:"+name, []byte(body))
		if err != nil {
			return err
		}
		definition := loaded.Definition
		if definition.Trigger.Type != "inventory_back_in_stock" || !matchesProduct(product, definition.Audience) {
			continue
		}
		customers, err := p.Store.DB.QueryContext(ctx, `SELECT DISTINCT o.customer_id FROM orders o JOIN order_lines ol ON ol.order_id=o.shopify_id WHERE ol.product_id=? AND o.customer_id IS NOT NULL AND o.cancelled_at IS NULL AND o.refunded_at IS NULL AND o.return_recorded_at IS NULL AND ol.current_quantity>0`, product.ID)
		if err != nil {
			return err
		}
		var customerIDs []int64
		for customers.Next() {
			var customerID int64
			if err := customers.Scan(&customerID); err != nil {
				customers.Close()
				return err
			}
			customerIDs = append(customerIDs, customerID)
		}
		if err := customers.Close(); err != nil {
			return err
		}
		for _, customerID := range customerIDs {
			var consent string
			var suppressed sql.NullString
			if p.Store.DB.QueryRowContext(ctx, `SELECT whatsapp_consent,suppressed_at FROM customers WHERE id=?`, customerID).Scan(&consent, &suppressed) != nil || consent != "opted_in" || suppressed.Valid {
				continue
			}
			runID := store.NewID("run")
			result, err := p.Store.DB.ExecContext(ctx, `INSERT INTO workflow_runs(id,workflow_name,workflow_version,customer_id,trigger_type,trigger_id,state,started_at) VALUES(?,?,?,?,?,?,?,?) ON CONFLICT DO NOTHING`, runID, name, version, customerID, "inventory_back_in_stock", triggerID, "active", time.Now().UTC().Format(time.RFC3339Nano))
			if err != nil {
				return err
			}
			inserted, _ := result.RowsAffected()
			if inserted == 0 {
				continue
			}
			for _, step := range definition.Steps {
				delay, _ := workflow.ParseWait(step.Wait)
				at, _ := workflow.NextAllowedTime(triggeredAt.Add(delay), definition.Timezone, definition.QuietHours.Start, definition.QuietHours.End)
				payload, _ := json.Marshal(sendPayload{CustomerID: customerID, RunID: runID, Template: step.Template, Language: step.Language, Category: step.Category, TrackedURL: step.URL, HeaderImageURL: step.HeaderImageURL, Params: step.Params, Conditions: step.Conditions, FrequencyMessages: definition.Frequency.Messages, FrequencyWindow: definition.Frequency.Window, Timezone: definition.Timezone, QuietStart: definition.QuietHours.Start, QuietEnd: definition.QuietHours.End})
				key := fmt.Sprintf("workflow:%s:v%d:%d:%s:%s", name, version, customerID, triggerID, step.ID)
				_, err = p.Store.EnqueueJob(ctx, store.Job{ID: store.NewID("job"), WorkflowRunID: sql.NullString{String: runID, Valid: true}, StepID: step.ID, Kind: "send_whatsapp", Payload: payload}, key, at)
				if err != nil {
					return err
				}
			}
		}
	}
	return nil
}
func matchesProduct(product shopify.Product, a workflow.Audience) bool {
	for _, v := range a.ProductHandles {
		if strings.EqualFold(v, product.Handle) {
			return true
		}
	}
	for _, v := range a.ProductTitles {
		if strings.Contains(strings.ToLower(product.Title), strings.ToLower(v)) {
			return true
		}
	}
	for _, tag := range product.Tags {
		for _, v := range a.ProductTags {
			if strings.EqualFold(tag, v) {
				return true
			}
		}
	}
	return false
}

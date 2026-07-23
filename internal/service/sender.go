package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nikunj-taneja/seedhibaat/internal/meta"
	"github.com/nikunj-taneja/seedhibaat/internal/security"
	"github.com/nikunj-taneja/seedhibaat/internal/store"
	"github.com/nikunj-taneja/seedhibaat/internal/workflow"
)

type sendPayload struct {
	CustomerID        int64               `json:"customer_id"`
	RunID             string              `json:"run_id"`
	CampaignID        string              `json:"campaign_id,omitempty"`
	Template          string              `json:"template"`
	Language          string              `json:"language"`
	Category          string              `json:"category"`
	TrackedURL        string              `json:"tracked_url,omitempty"`
	Params            map[string]string   `json:"params,omitempty"`
	Conditions        workflow.Conditions `json:"conditions,omitempty"`
	FrequencyMessages int                 `json:"frequency_messages"`
	FrequencyWindow   string              `json:"frequency_window"`
	Timezone          string              `json:"timezone,omitempty"`
	QuietStart        string              `json:"quiet_start,omitempty"`
	QuietEnd          string              `json:"quiet_end,omitempty"`
}

func (p *Processor) sendWhatsApp(ctx context.Context, job store.Job) error {
	if !p.Config.OutboundSendingEnabled {
		return errors.New("outbound sending gate is disabled")
	}
	var payload sendPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return err
	}
	if payload.CustomerID == 0 || payload.Template == "" {
		return errors.New("invalid outbound payload")
	}
	var encrypted []byte
	var firstEncrypted, lastEncrypted []byte
	var consent string
	var suppressed sql.NullString
	var invalid int
	if err := p.Store.DB.QueryRowContext(ctx, `SELECT phone_ciphertext,first_name_ciphertext,last_name_ciphertext,whatsapp_consent,suppressed_at,invalid_number FROM customers WHERE id=?`, payload.CustomerID).Scan(&encrypted, &firstEncrypted, &lastEncrypted, &consent, &suppressed, &invalid); err != nil {
		return err
	}
	if consent != "opted_in" || suppressed.Valid || invalid == 1 {
		return p.cancelJobAsSuppressed(ctx, job, payload, "customer is not eligible")
	}
	if payload.RunID != "" {
		var state string
		if err := p.Store.DB.QueryRowContext(ctx, `SELECT state FROM workflow_runs WHERE id=?`, payload.RunID).Scan(&state); err != nil {
			return err
		}
		if state != "active" {
			return p.cancelJobAsSuppressed(ctx, job, payload, "workflow is not active")
		}
	}
	if payload.CampaignID != "" {
		var state string
		if err := p.Store.DB.QueryRowContext(ctx, `SELECT state FROM campaigns WHERE id=?`, payload.CampaignID).Scan(&state); err != nil {
			return err
		}
		if state != "scheduled" {
			return p.cancelJobAsSuppressed(ctx, job, payload, "campaign is not active")
		}
	}
	if payload.Timezone != "" && payload.QuietStart != "" && payload.QuietEnd != "" {
		now := time.Now()
		allowed, err := workflow.NextAllowedTime(now, payload.Timezone, payload.QuietStart, payload.QuietEnd)
		if err != nil {
			return err
		}
		if allowed.After(now.Add(time.Second)) {
			return p.Store.DeferJob(ctx, job.ID, allowed, "quiet hours")
		}
	}
	eligible, reason, err := p.evaluateStepConditions(ctx, payload)
	if err != nil {
		return err
	}
	if !eligible {
		return p.cancelJobAsSuppressed(ctx, job, payload, reason)
	}
	window, err := time.ParseDuration(payload.FrequencyWindow)
	if err != nil {
		return err
	}
	phone, err := security.Decrypt(p.Config.PIIHashKey, encrypted)
	if err != nil {
		return err
	}
	var idempotency string
	if err := p.Store.DB.QueryRowContext(ctx, `SELECT idempotency_key FROM scheduled_jobs WHERE id=?`, job.ID).Scan(&idempotency); err != nil {
		return err
	}
	messageID := store.NewID("msg")
	now := time.Now().UTC().Format(time.RFC3339Nano)
	components, parameterFingerprint, err := p.renderTemplateParameters(ctx, payload, firstEncrypted, lastEncrypted)
	if err != nil {
		return err
	}
	tx, err := p.Store.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM outbound_messages WHERE customer_id=? AND attempted_at>=? AND idempotency_key<>?`, payload.CustomerID, time.Now().Add(-window).UTC().Format(time.RFC3339Nano), idempotency).Scan(&count); err != nil {
		return err
	}
	if count >= payload.FrequencyMessages {
		if err := tx.Rollback(); err != nil {
			return err
		}
		return p.cancelJobAsSuppressed(ctx, job, payload, "frequency cap reached")
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO outbound_messages(id,job_id,customer_id,workflow_run_id,campaign_id,template_name,template_language,category,parameter_fingerprint,idempotency_key,state,attempted_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,'attempted',?,?,?) ON CONFLICT(idempotency_key) DO NOTHING`, messageID, job.ID, payload.CustomerID, nullableString(payload.RunID), nullableString(payload.CampaignID), payload.Template, payload.Language, payload.Category, nullableString(parameterFingerprint), idempotency, now, now, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	var existingMeta sql.NullString
	var existingFingerprint sql.NullString
	if err := p.Store.DB.QueryRowContext(ctx, `SELECT id,meta_message_id,parameter_fingerprint FROM outbound_messages WHERE idempotency_key=?`, idempotency).Scan(&messageID, &existingMeta, &existingFingerprint); err != nil {
		return err
	}
	if existingMeta.Valid {
		return nil
	}
	if existingFingerprint.Valid && existingFingerprint.String != parameterFingerprint {
		return &noRetryError{err: errors.New("template parameters changed after the first attempt; manual review required")}
	}
	if payload.TrackedURL != "" {
		expires := time.Now().Add(60 * 24 * time.Hour)
		recordID := store.NewID("link")
		token := security.NewTrackedToken(p.Config.LinkSigningKey, recordID, expires)
		tokenHash := security.KeyedHash(p.Config.LinkSigningKey, token)
		_, err = p.Store.DB.ExecContext(ctx, `INSERT INTO tracked_links(token_hash,message_id,destination_url,expires_at,created_at) VALUES(?,?,?,?,?) ON CONFLICT(token_hash) DO NOTHING`, tokenHash, messageID, payload.TrackedURL, expires.UTC().Format(time.RFC3339Nano), now)
		if err != nil {
			return err
		}
		components = append(components, meta.MessageComponent{Type: "button", SubType: "url", Index: "0", Parameters: []meta.MessageParameter{{Type: "text", Text: token}}})
	}
	providerID, err := p.Meta.SendTemplate(ctx, string(phone), payload.Template, payload.Language, components)
	if err != nil {
		var network *meta.NetworkError
		if errors.As(err, &network) {
			stamp := time.Now().UTC().Format(time.RFC3339Nano)
			_, _ = p.Store.DB.ExecContext(ctx, `UPDATE outbound_messages SET state='unknown',failure_reason='network failure after send attempt; manual reconciliation required',updated_at=? WHERE id=?`, stamp, messageID)
			return &noRetryError{err: err}
		}
		var apiError *meta.APIError
		if errors.As(err, &apiError) && !apiError.Retryable() {
			stamp := time.Now().UTC().Format(time.RFC3339Nano)
			_, _ = p.Store.DB.ExecContext(ctx, `UPDATE outbound_messages SET state='failed',failed_at=?,failure_code=?,failure_reason=?,updated_at=? WHERE id=?`, stamp, fmt.Sprint(apiError.Code), truncate(safeLogError(errors.New(apiError.Message)), 500), stamp, messageID)
			if apiError.Code == 131026 || apiError.Code == 131050 {
				consent := "opted_in"
				reason := "Meta reported an undeliverable recipient"
				invalid := 1
				if apiError.Code == 131050 {
					consent = "opted_out"
					reason = "Meta marketing opt-out"
					invalid = 0
				}
				query := `UPDATE customers SET whatsapp_consent=?,invalid_number=CASE WHEN ?=1 THEN 1 ELSE invalid_number END,suppressed_at=coalesce(suppressed_at,?),suppression_reason=?,updated_at=? WHERE id=?`
				if apiError.Code == 131050 {
					query += ` AND whatsapp_consent<>'opted_out'`
				}
				result, _ := p.Store.DB.ExecContext(ctx, query, consent, invalid, stamp, reason, stamp, payload.CustomerID)
				if apiError.Code == 131050 && result != nil {
					if changed, _ := result.RowsAffected(); changed == 1 {
						_ = p.Store.Audit(ctx, "meta", "customer.opt_out", "customer", fmt.Sprint(payload.CustomerID), `{"source":"meta_error_131050"}`)
					}
				}
				_ = p.Store.CancelCustomerWork(ctx, payload.CustomerID, reason)
			}
			return &noRetryError{err: err}
		}
		return err
	}
	accepted := time.Now().UTC().Format(time.RFC3339Nano)
	_, err = p.Store.DB.ExecContext(ctx, `UPDATE outbound_messages SET meta_message_id=?,state='accepted',accepted_at=?,updated_at=? WHERE id=?`, providerID, accepted, accepted, messageID)
	return err
}

func (p *Processor) evaluateStepConditions(ctx context.Context, payload sendPayload) (bool, string, error) {
	conditions := payload.Conditions
	if conditions.OrderNotCancelled || conditions.OrderNotRefunded {
		if payload.RunID == "" {
			return false, "", errors.New("order conditions require a workflow run")
		}
		var triggerType, triggerID string
		if err := p.Store.DB.QueryRowContext(ctx, `SELECT trigger_type,trigger_id FROM workflow_runs WHERE id=?`, payload.RunID).Scan(&triggerType, &triggerID); err != nil {
			return false, "", err
		}
		if triggerType != "order_delivered" {
			return false, "", errors.New("order conditions require an order-delivered workflow")
		}
		var cancelled, refunded sql.NullString
		if err := p.Store.DB.QueryRowContext(ctx, `SELECT cancelled_at,refunded_at FROM orders WHERE shopify_id=?`, triggerID).Scan(&cancelled, &refunded); err != nil {
			return false, "", err
		}
		if conditions.OrderNotCancelled && cancelled.Valid {
			return false, "order cancellation condition was not met", nil
		}
		if conditions.OrderNotRefunded && refunded.Valid {
			return false, "order refund condition was not met", nil
		}
	}
	if conditions.CustomerHasPurchased == nil && conditions.CustomerHasNotPurchased == nil {
		return true, "", nil
	}
	rows, err := p.Store.DB.QueryContext(ctx, `SELECT coalesce(p.handle,''),coalesce(p.title,''),coalesce(p.tags_json,'[]'),ol.title FROM orders o JOIN order_lines ol ON ol.order_id=o.shopify_id LEFT JOIN products p ON p.shopify_id=ol.product_id WHERE o.customer_id=? AND o.cancelled_at IS NULL AND o.refunded_at IS NULL AND o.return_recorded_at IS NULL AND ol.current_quantity>0`, payload.CustomerID)
	if err != nil {
		return false, "", err
	}
	type purchasedProduct struct{ handle, title, tags, lineTitle string }
	var products []purchasedProduct
	for rows.Next() {
		var product purchasedProduct
		if err := rows.Scan(&product.handle, &product.title, &product.tags, &product.lineTitle); err != nil {
			rows.Close()
			return false, "", err
		}
		products = append(products, product)
	}
	if err := rows.Close(); err != nil {
		return false, "", err
	}
	matches := func(condition *workflow.ProductCondition) bool {
		if condition == nil {
			return false
		}
		for _, product := range products {
			for _, handle := range condition.ProductHandles {
				if strings.EqualFold(handle, product.handle) {
					return true
				}
			}
			for _, title := range condition.ProductTitles {
				if strings.Contains(strings.ToLower(product.title+" "+product.lineTitle), strings.ToLower(title)) {
					return true
				}
			}
			var tags []string
			_ = json.Unmarshal([]byte(product.tags), &tags)
			for _, wanted := range condition.ProductTags {
				for _, tag := range tags {
					if strings.EqualFold(wanted, tag) {
						return true
					}
				}
			}
		}
		return false
	}
	if conditions.CustomerHasPurchased != nil && !matches(conditions.CustomerHasPurchased) {
		return false, "required-purchase condition was not met", nil
	}
	if conditions.CustomerHasNotPurchased != nil && matches(conditions.CustomerHasNotPurchased) {
		return false, "excluded-purchase condition was met", nil
	}
	return true, "", nil
}

func (p *Processor) renderTemplateParameters(ctx context.Context, payload sendPayload, firstEncrypted, lastEncrypted []byte) ([]meta.MessageComponent, string, error) {
	header, err := workflow.OrderedParameterBindings(payload.Params, "header")
	if err != nil {
		return nil, "", err
	}
	body, err := workflow.OrderedParameterBindings(payload.Params, "body")
	if err != nil {
		return nil, "", err
	}
	if len(header) == 0 && len(body) == 0 {
		return nil, "", nil
	}
	values := map[string]string{}
	decrypt := func(key string, encrypted []byte) error {
		if _, exists := values[key]; exists {
			return nil
		}
		if len(encrypted) == 0 {
			return fmt.Errorf("%s is unavailable", key)
		}
		plaintext, err := security.Decrypt(p.Config.PIIHashKey, encrypted)
		if err != nil {
			return err
		}
		values[key] = string(plaintext)
		return nil
	}
	all := append(append([]string{}, header...), body...)
	needsOrder := false
	for _, source := range all {
		switch source {
		case "customer.first_name":
			if err := decrypt(source, firstEncrypted); err != nil {
				return nil, "", err
			}
		case "customer.last_name":
			if err := decrypt(source, lastEncrypted); err != nil {
				return nil, "", err
			}
		case "order.number", "order.first_product_title":
			needsOrder = true
		}
	}
	if needsOrder {
		if payload.RunID == "" {
			return nil, "", errors.New("order template parameters require a workflow run")
		}
		var triggerType, orderID string
		if err := p.Store.DB.QueryRowContext(ctx, `SELECT trigger_type,trigger_id FROM workflow_runs WHERE id=?`, payload.RunID).Scan(&triggerType, &orderID); err != nil {
			return nil, "", err
		}
		if triggerType != "order_delivered" {
			return nil, "", errors.New("order template parameters require an order-delivered workflow")
		}
		var orderNumber, firstProductTitle string
		if err := p.Store.DB.QueryRowContext(ctx, `SELECT order_number FROM orders WHERE shopify_id=?`, orderID).Scan(&orderNumber); err != nil {
			return nil, "", err
		}
		if err := p.Store.DB.QueryRowContext(ctx, `SELECT title FROM order_lines WHERE order_id=? ORDER BY shopify_id LIMIT 1`, orderID).Scan(&firstProductTitle); err != nil {
			return nil, "", err
		}
		values["order.number"] = orderNumber
		values["order.first_product_title"] = firstProductTitle
	}
	resolve := func(source string) (string, error) {
		value := values[source]
		if strings.HasPrefix(source, "literal:") {
			value = strings.TrimPrefix(source, "literal:")
		}
		if strings.TrimSpace(value) == "" {
			return "", fmt.Errorf("template parameter %s rendered empty", source)
		}
		return value, nil
	}
	components := make([]meta.MessageComponent, 0, 2)
	fingerprintParts := make([]string, 0, len(all))
	for _, item := range []struct {
		name     string
		bindings []string
	}{{"header", header}, {"body", body}} {
		if len(item.bindings) == 0 {
			continue
		}
		component := meta.MessageComponent{Type: item.name}
		for _, source := range item.bindings {
			value, err := resolve(source)
			if err != nil {
				return nil, "", err
			}
			component.Parameters = append(component.Parameters, meta.MessageParameter{Type: "text", Text: value})
			fingerprintParts = append(fingerprintParts, item.name+"\x00"+value)
		}
		components = append(components, component)
	}
	fingerprint := security.KeyedHash(p.Config.PIIHashKey, strings.Join(fingerprintParts, "\x01"))
	return components, fingerprint, nil
}

func (p *Processor) cancelJobAsSuppressed(ctx context.Context, job store.Job, payload sendPayload, reason string) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := p.Store.DB.ExecContext(ctx, `UPDATE scheduled_jobs SET state='suppressed',last_error=?,completed_at=?,updated_at=? WHERE id=?`, reason, now, now, job.ID)
	if err != nil {
		return err
	}
	return nil
}

package service

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/nikunj-taneja/seedhibaat/internal/config"
	"github.com/nikunj-taneja/seedhibaat/internal/security"
	"github.com/nikunj-taneja/seedhibaat/internal/segment"
	"github.com/nikunj-taneja/seedhibaat/internal/store"
	"github.com/nikunj-taneja/seedhibaat/internal/workflow"
)

type HTTPServer struct {
	Config config.Config
	Store  *store.Store
	Logger *slog.Logger
}

func (s *HTTPServer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /webhooks/meta", s.metaVerify)
	mux.HandleFunc("POST /webhooks/meta", s.metaWebhook)
	mux.HandleFunc("POST /webhooks/shopify", s.shopifyWebhook)
	mux.HandleFunc("POST /webhooks/gokwik/{token}", s.goKwikWebhook)
	mux.HandleFunc("GET /r/{token}", s.redirect)
	mux.Handle("GET /metrics", s.metricsAuthenticated(http.HandlerFunc(s.dashboard)))
	mux.Handle("GET /metrics/assets/dashboard.css", s.metricsAuthenticated(http.HandlerFunc(s.dashboardStyles)))
	mux.Handle("GET /metrics/assets/dashboard.js", s.metricsAuthenticated(http.HandlerFunc(s.dashboardScript)))
	mux.Handle("/api/v1/", s.authenticated(s.apiMux()))
	return s.securityHeaders(s.requestLog(mux))
}

func (s *HTTPServer) apiMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/report", s.report)
	mux.HandleFunc("GET /api/v1/workflows", s.workflows)
	mux.HandleFunc("GET /api/v1/workflows/{name}/preview", s.previewWorkflow)
	mux.HandleFunc("POST /api/v1/workflows/{name}/activate", s.activateWorkflow)
	mux.HandleFunc("POST /api/v1/workflows/{name}/pause", s.pauseWorkflowDefinition)
	mux.HandleFunc("POST /api/v1/workflows/validate", s.validateWorkflow)
	mux.HandleFunc("POST /api/v1/workflows/simulate", s.simulateWorkflow)
	mux.HandleFunc("POST /api/v1/workflows/reload", s.reloadWorkflows)
	mux.HandleFunc("POST /api/v1/runs/{id}/pause", s.pauseRun)
	mux.HandleFunc("POST /api/v1/runs/{id}/resume", s.resumeRun)
	mux.HandleFunc("POST /api/v1/runs/{id}/cancel", s.cancelRun)
	mux.HandleFunc("POST /api/v1/jobs/{id}/replay", s.replayJob)
	mux.HandleFunc("GET /api/v1/audit", s.auditHistory)
	mux.HandleFunc("POST /api/v1/integrity", s.integrity)
	mux.HandleFunc("POST /api/v1/segments/preview", s.segmentPreview)
	mux.HandleFunc("POST /api/v1/campaigns", s.createCampaign)
	mux.HandleFunc("GET /api/v1/campaigns/{id}", s.getCampaign)
	mux.HandleFunc("POST /api/v1/campaigns/{id}/activate", s.activateCampaign)
	mux.HandleFunc("POST /api/v1/campaigns/{id}/cancel", s.cancelCampaign)
	mux.HandleFunc("POST /api/v1/consent/import", s.importConsent)
	mux.HandleFunc("POST /api/v1/messages/reserve", s.reserveExternalMessage)
	mux.HandleFunc("POST /api/v1/messages/record", s.recordExternalMessage)
	mux.HandleFunc("POST /api/v1/customers/import", s.importCustomers)
	return mux
}

func (s *HTTPServer) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.Store.DB.PingContext(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "unhealthy"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "production_flow_enabled": s.Config.ProductionFlowEnabled, "outbound_sending_enabled": s.Config.OutboundSendingEnabled})
}

func (s *HTTPServer) metaVerify(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("hub.mode") != "subscribe" || subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("hub.verify_token")), []byte(s.Config.MetaVerifyToken)) != 1 {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, r.URL.Query().Get("hub.challenge"))
}

func (s *HTTPServer) metaWebhook(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody(w, r, 2<<20)
	if !ok {
		return
	}
	if !security.HMACSHA256HexValid(s.Config.MetaAppSecret, body, r.Header.Get("X-Hub-Signature-256")) {
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}
	digest := sha256.Sum256(body)
	eventID := hex.EncodeToString(digest[:])
	inserted, err := s.Store.RecordWebhook(r.Context(), store.WebhookEvent{Provider: "meta", EventID: eventID, Topic: "whatsapp_business_account", Payload: body}, time.Now())
	if err != nil {
		s.Logger.Error("record Meta webhook", "error", err)
		http.Error(w, "temporary failure", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"accepted": true, "duplicate": !inserted})
}

func (s *HTTPServer) shopifyWebhook(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody(w, r, 4<<20)
	if !ok {
		return
	}
	if !security.HMACBase64Valid(s.Config.ShopifyClientSecret, body, r.Header.Get("X-Shopify-Hmac-Sha256")) {
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}
	shop := strings.ToLower(r.Header.Get("X-Shopify-Shop-Domain"))
	if shop != strings.ToLower(s.Config.ShopifyShopDomain) {
		http.Error(w, "wrong shop", http.StatusForbidden)
		return
	}
	eventID := r.Header.Get("X-Shopify-Webhook-Id")
	if eventID == "" {
		http.Error(w, "missing webhook ID", http.StatusBadRequest)
		return
	}
	topic := r.Header.Get("X-Shopify-Topic")
	inserted, err := s.Store.RecordWebhook(r.Context(), store.WebhookEvent{Provider: "shopify", EventID: eventID, Topic: topic, Payload: body}, time.Now())
	if err != nil {
		s.Logger.Error("record Shopify webhook", "error", err)
		http.Error(w, "temporary failure", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"accepted": true, "duplicate": !inserted})
}

func (s *HTTPServer) goKwikWebhook(w http.ResponseWriter, r *http.Request) {
	configured := strings.TrimSpace(s.Config.GoKwikWebhookToken)
	supplied := strings.TrimSpace(r.PathValue("token"))
	if configured == "" || subtle.ConstantTimeCompare([]byte(supplied), []byte(configured)) != 1 {
		http.NotFound(w, r)
		return
	}
	body, ok := readBody(w, r, 2<<20)
	if !ok {
		return
	}
	if !json.Valid(body) {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	digest := sha256.Sum256(body)
	eventID := hex.EncodeToString(digest[:])
	inserted, err := s.Store.RecordWebhook(r.Context(), store.WebhookEvent{Provider: "gokwik", EventID: eventID, Topic: "abandoned_checkout", Payload: body}, time.Now())
	if err != nil {
		s.Logger.Error("record GoKwik webhook", "error", err)
		http.Error(w, "temporary failure", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"accepted": true, "duplicate": !inserted})
}

func (s *HTTPServer) redirect(w http.ResponseWriter, r *http.Request) {
	recordID, err := security.VerifyTrackedToken(s.Config.LinkSigningKey, r.PathValue("token"), time.Now())
	if err != nil {
		http.Error(w, "link is invalid or expired", http.StatusGone)
		return
	}
	var destination string
	err = s.Store.DB.QueryRowContext(r.Context(), `SELECT destination_url FROM tracked_links WHERE token_hash=? AND expires_at>?`, security.KeyedHash(s.Config.LinkSigningKey, r.PathValue("token")), time.Now().UTC().Format(time.RFC3339Nano)).Scan(&destination)
	if err != nil {
		http.Error(w, "link is invalid or expired", http.StatusGone)
		return
	}
	parsed, err := security.SafeDestination(destination, s.Config.RedirectAllowedHosts)
	if err != nil {
		s.Logger.Error("unsafe tracked destination", "record_id", recordID)
		http.Error(w, "invalid destination", http.StatusInternalServerError)
		return
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, _ = s.Store.DB.ExecContext(r.Context(), `UPDATE tracked_links SET first_clicked_at=coalesce(first_clicked_at,?),click_count=click_count+1 WHERE token_hash=?`, now, security.KeyedHash(s.Config.LinkSigningKey, r.PathValue("token")))
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, parsed.String(), http.StatusFound)
}

func (s *HTTPServer) report(w http.ResponseWriter, r *http.Request) {
	to := time.Now().UTC()
	from := to.AddDate(0, -1, 0)
	if value := r.URL.Query().Get("from"); value != "" {
		parsed, err := time.Parse(time.RFC3339, value)
		if err != nil {
			http.Error(w, "invalid from", 400)
			return
		}
		from = parsed
	}
	if value := r.URL.Query().Get("to"); value != "" {
		parsed, err := time.Parse(time.RFC3339, value)
		if err != nil {
			http.Error(w, "invalid to", 400)
			return
		}
		to = parsed
	}
	metrics, err := s.Store.MetricsFiltered(r.Context(), from, to, store.MetricsFilter{CampaignID: r.URL.Query().Get("campaign"), WorkflowName: r.URL.Query().Get("workflow"), TemplateName: r.URL.Query().Get("template")})
	if err != nil {
		http.Error(w, "report unavailable", 500)
		return
	}
	writeJSON(w, 200, metrics)
}

func (s *HTTPServer) workflows(w http.ResponseWriter, r *http.Request) {
	rows, err := s.Store.DB.QueryContext(r.Context(), `SELECT name,version,definition_hash,active,created_at FROM workflow_definitions ORDER BY name,version`)
	if err != nil {
		http.Error(w, "query failed", 500)
		return
	}
	defer rows.Close()
	var result []map[string]any
	for rows.Next() {
		var name, hash, created string
		var version, active int
		if rows.Scan(&name, &version, &hash, &active, &created) != nil {
			http.Error(w, "query failed", 500)
			return
		}
		result = append(result, map[string]any{"name": name, "version": version, "definition_hash": hash, "active": active == 1, "created_at": created})
	}
	writeJSON(w, 200, map[string]any{"workflows": result, "production_gate": s.Config.ProductionFlowEnabled})
}

func (s *HTTPServer) activateWorkflow(w http.ResponseWriter, r *http.Request) {
	if !s.Config.ProductionFlowEnabled || !s.Config.OutboundSendingEnabled {
		http.Error(w, "production and outbound activation gates must both be enabled", http.StatusConflict)
		return
	}
	name := r.PathValue("name")
	var request struct {
		ConfirmedRecipientCount int `json:"confirmed_recipient_count"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	currentCount, err := s.initialWorkflowRecipientCount(r.Context(), name)
	if err != nil {
		http.Error(w, err.Error(), 404)
		return
	}
	if request.ConfirmedRecipientCount != currentCount {
		http.Error(w, "confirmed recipient count does not match current initial recipients", 409)
		return
	}
	tx, err := s.Store.DB.BeginTx(r.Context(), nil)
	if err != nil {
		http.Error(w, "failed", 500)
		return
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(r.Context(), `UPDATE workflow_definitions SET active=0 WHERE name=?`, name); err == nil {
		_, err = tx.ExecContext(r.Context(), `UPDATE workflow_definitions SET active=1 WHERE name=? AND version=(SELECT max(version) FROM workflow_definitions WHERE name=?)`, name, name)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if err == nil {
		_, err = tx.ExecContext(r.Context(), `UPDATE workflow_runs SET state='active',cancellation_reason=NULL WHERE workflow_name=? AND state='paused'`, name)
	}
	if err == nil {
		_, err = tx.ExecContext(r.Context(), `UPDATE scheduled_jobs SET state='scheduled',available_at=CASE WHEN available_at<? THEN ? ELSE available_at END,locked_at=NULL,locked_by=NULL,updated_at=? WHERE workflow_run_id IN (SELECT id FROM workflow_runs WHERE workflow_name=?) AND state='paused'`, now, now, now, name)
	}
	if err != nil || tx.Commit() != nil {
		http.Error(w, "failed", 500)
		return
	}
	_ = s.Store.Audit(r.Context(), "operator", "workflow.activate", "workflow", name, fmt.Sprintf(`{"initial_recipient_count":%d}`, currentCount))
	writeJSON(w, 200, map[string]any{"activated": name, "initial_recipient_count": currentCount})
}

func (s *HTTPServer) initialWorkflowRecipientCount(ctx context.Context, name string) (int, error) {
	var exists int
	if err := s.Store.DB.QueryRowContext(ctx, `SELECT count(*) FROM workflow_definitions WHERE name=?`, name).Scan(&exists); err != nil {
		return 0, err
	}
	if exists == 0 {
		return 0, fmt.Errorf("workflow not found")
	}
	var count int
	err := s.Store.DB.QueryRowContext(ctx, `SELECT count(DISTINCT customer_id) FROM workflow_runs WHERE workflow_name=? AND state IN ('active','paused')`, name).Scan(&count)
	return count, err
}

func (s *HTTPServer) previewWorkflow(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var version int
	var body string
	err := s.Store.DB.QueryRowContext(r.Context(), `SELECT version,yaml FROM workflow_definitions WHERE name=? ORDER BY version DESC LIMIT 1`, name).Scan(&version, &body)
	if err != nil {
		http.Error(w, "workflow not found", 404)
		return
	}
	loaded, err := workflow.Parse("database:"+name, []byte(body))
	if err != nil {
		http.Error(w, "stored workflow is invalid", 500)
		return
	}
	initialCount, err := s.initialWorkflowRecipientCount(r.Context(), name)
	if err != nil {
		http.Error(w, "query failed", 500)
		return
	}
	var pendingJobs int
	_ = s.Store.DB.QueryRowContext(r.Context(), `SELECT count(*) FROM scheduled_jobs WHERE workflow_run_id IN (SELECT id FROM workflow_runs WHERE workflow_name=?) AND state IN ('scheduled','retry','paused')`, name).Scan(&pendingJobs)
	writeJSON(w, 200, map[string]any{"name": name, "version": version, "definition": loaded.Definition, "initial_recipient_count": initialCount, "pending_jobs": pendingJobs, "historical_backfill_enabled": false, "production_gate": s.Config.ProductionFlowEnabled})
}

func (s *HTTPServer) pauseWorkflowDefinition(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	tx, err := s.Store.DB.BeginTx(r.Context(), nil)
	if err != nil {
		http.Error(w, "failed", 500)
		return
	}
	defer tx.Rollback()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err = tx.ExecContext(r.Context(), `UPDATE workflow_definitions SET active=0 WHERE name=?`, name); err == nil {
		_, err = tx.ExecContext(r.Context(), `UPDATE workflow_runs SET state='paused',cancellation_reason='workflow definition paused' WHERE workflow_name=? AND state='active'`, name)
	}
	if err == nil {
		_, err = tx.ExecContext(r.Context(), `UPDATE scheduled_jobs SET state='paused',locked_at=NULL,locked_by=NULL,updated_at=? WHERE workflow_run_id IN (SELECT id FROM workflow_runs WHERE workflow_name=?) AND state IN ('scheduled','retry')`, now, name)
	}
	if err != nil || tx.Commit() != nil {
		http.Error(w, "failed", 500)
		return
	}
	_ = s.Store.Audit(r.Context(), "operator", "workflow.pause", "workflow", name, "{}")
	writeJSON(w, 200, map[string]any{"paused": name})
}

func (s *HTTPServer) validateWorkflow(w http.ResponseWriter, r *http.Request) {
	var request struct {
		YAML string `json:"yaml"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	loaded, err := workflow.Parse("api", []byte(request.YAML))
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	writeJSON(w, 200, map[string]any{"valid": true, "name": loaded.Definition.Name, "version": loaded.Definition.Version, "definition_hash": loaded.Hash})
}

func (s *HTTPServer) simulateWorkflow(w http.ResponseWriter, r *http.Request) {
	var request struct {
		YAML        string `json:"yaml"`
		TriggeredAt string `json:"triggered_at"`
		AsOf        string `json:"as_of"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	loaded, err := workflow.Parse("simulation", []byte(request.YAML))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	triggeredAt, err := time.Parse(time.RFC3339, request.TriggeredAt)
	if err != nil {
		http.Error(w, "triggered_at must use RFC3339", http.StatusBadRequest)
		return
	}
	asOf := time.Now()
	if request.AsOf != "" {
		asOf, err = time.Parse(time.RFC3339, request.AsOf)
		if err != nil {
			http.Error(w, "as_of must use RFC3339", http.StatusBadRequest)
			return
		}
	}
	location, _ := time.LoadLocation(loaded.Definition.Timezone)
	type simulatedStep struct {
		ID          string `json:"id"`
		Wait        string `json:"wait"`
		Template    string `json:"template"`
		Category    string `json:"category"`
		ScheduledAt string `json:"scheduled_at"`
		State       string `json:"state"`
	}
	steps := make([]simulatedStep, 0, len(loaded.Definition.Steps))
	for _, step := range loaded.Definition.Steps {
		delay, err := workflow.ParseWait(step.Wait)
		if err != nil {
			http.Error(w, "stored workflow wait is invalid", http.StatusInternalServerError)
			return
		}
		scheduledAt, err := workflow.NextAllowedTime(
			triggeredAt.Add(delay),
			loaded.Definition.Timezone,
			loaded.Definition.QuietHours.Start,
			loaded.Definition.QuietHours.End,
		)
		if err != nil {
			http.Error(w, "stored workflow quiet hours are invalid", http.StatusInternalServerError)
			return
		}
		state := "scheduled"
		if !scheduledAt.After(asOf) {
			state = "due"
		}
		steps = append(steps, simulatedStep{
			ID:          step.ID,
			Wait:        step.Wait,
			Template:    step.Template,
			Category:    step.Category,
			ScheduledAt: scheduledAt.In(location).Format(time.RFC3339),
			State:       state,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"name":                     loaded.Definition.Name,
		"version":                  loaded.Definition.Version,
		"trigger":                  loaded.Definition.Trigger.Type,
		"triggered_at":             triggeredAt.Format(time.RFC3339),
		"as_of":                    asOf.Format(time.RFC3339),
		"timezone":                 loaded.Definition.Timezone,
		"quiet_hours":              map[string]string{"start": loaded.Definition.QuietHours.Start, "end": loaded.Definition.QuietHours.End},
		"frequency_cap":            map[string]any{"messages": loaded.Definition.Frequency.Messages, "window": loaded.Definition.Frequency.Window},
		"steps":                    steps,
		"writes_performed":         false,
		"production_gate_enabled":  s.Config.ProductionFlowEnabled,
		"outbound_sending_enabled": s.Config.OutboundSendingEnabled,
	})
}

func (s *HTTPServer) reloadWorkflows(w http.ResponseWriter, r *http.Request) {
	loaded, err := workflow.LoadDir(s.Config.WorkflowDir)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	for _, item := range loaded {
		if err = persistWorkflowDefinition(r.Context(), s.Store.DB, item); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
	}
	_ = s.Store.Audit(r.Context(), "operator", "workflow.reload", "workflow", "", fmt.Sprintf(`{"count":%d}`, len(loaded)))
	writeJSON(w, 200, map[string]any{"reloaded": len(loaded)})
}

func (s *HTTPServer) pauseRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.Store.PauseWorkflow(r.Context(), id, "operator pause"); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	_ = s.Store.Audit(r.Context(), "operator", "workflow_run.pause", "workflow_run", id, "{}")
	writeJSON(w, 200, map[string]any{"paused": id})
}
func (s *HTTPServer) resumeRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.Store.ResumeWorkflow(r.Context(), id); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	_ = s.Store.Audit(r.Context(), "operator", "workflow_run.resume", "workflow_run", id, "{}")
	writeJSON(w, 200, map[string]any{"resumed": id})
}
func (s *HTTPServer) cancelRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.Store.CancelWorkflow(r.Context(), id, "operator cancellation"); err != nil {
		http.Error(w, err.Error(), 409)
		return
	}
	_ = s.Store.Audit(r.Context(), "operator", "workflow_run.cancel", "workflow_run", id, "{}")
	writeJSON(w, 200, map[string]any{"cancelled": id})
}
func (s *HTTPServer) replayJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.Store.ReplayFailedJob(r.Context(), id, time.Now()); err != nil {
		http.Error(w, err.Error(), 409)
		return
	}
	_ = s.Store.Audit(r.Context(), "operator", "job.replay", "scheduled_job", id, "{}")
	writeJSON(w, 200, map[string]any{"replayed": id})
}
func (s *HTTPServer) auditHistory(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if value := r.URL.Query().Get("limit"); value != "" {
		if _, err := fmt.Sscan(value, &limit); err != nil || limit < 1 || limit > 500 {
			http.Error(w, "limit must be between 1 and 500", 400)
			return
		}
	}
	rows, err := s.Store.DB.QueryContext(r.Context(), `SELECT occurred_at,actor,action,object_type,object_id,details_json FROM audit_log ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		http.Error(w, "query failed", 500)
		return
	}
	defer rows.Close()
	entries := make([]map[string]any, 0)
	for rows.Next() {
		var occurred, actor, action, objectType, details string
		var objectID sql.NullString
		if err := rows.Scan(&occurred, &actor, &action, &objectType, &objectID, &details); err != nil {
			http.Error(w, "query failed", 500)
			return
		}
		var decoded any
		if json.Unmarshal([]byte(details), &decoded) != nil {
			decoded = map[string]any{}
		}
		entries = append(entries, map[string]any{"occurred_at": occurred, "actor": actor, "action": action, "object_type": objectType, "object_id": nullableJSON(objectID), "details": decoded})
	}
	writeJSON(w, 200, map[string]any{"entries": entries})
}
func (s *HTTPServer) integrity(w http.ResponseWriter, r *http.Request) {
	if err := s.Store.IntegrityCheck(r.Context()); err != nil {
		http.Error(w, "integrity check failed", 500)
		return
	}
	writeJSON(w, 200, map[string]any{"integrity": "ok"})
}

type campaignRequest struct {
	Name                    string             `json:"name"`
	Segment                 segment.Definition `json:"segment"`
	Template                string             `json:"template"`
	Language                string             `json:"language"`
	TrackedURL              string             `json:"tracked_url,omitempty"`
	HeaderImageURL          string             `json:"header_image_url,omitempty"`
	Params                  map[string]string  `json:"params,omitempty"`
	ScheduledAt             string             `json:"scheduled_at,omitempty"`
	ConfirmedRecipientCount int                `json:"confirmed_recipient_count,omitempty"`
	FrequencyMessages       int                `json:"frequency_messages,omitempty"`
	FrequencyWindow         string             `json:"frequency_window,omitempty"`
	RecipientParams         []recipientParams  `json:"recipient_params,omitempty"`
}

// recipientParams carries the template values that differ from one recipient to
// the next. Its keys must already exist in the campaign's own parameter map, so
// a recipient can change what a slot says but never how many slots there are.
type recipientParams struct {
	CustomerShopifyID string            `json:"customer_shopify_id"`
	Params            map[string]string `json:"params"`
}

// mergeTemplateParams returns the campaign parameters with the recipient's
// values applied on top.
func mergeTemplateParams(campaign, recipient map[string]string) map[string]string {
	if len(recipient) == 0 {
		return campaign
	}
	merged := make(map[string]string, len(campaign))
	for key, value := range campaign {
		merged[key] = value
	}
	for key, value := range recipient {
		merged[key] = value
	}
	return merged
}

// resolveRecipientParams maps each entry onto an internal customer ID and
// rejects anything that would change the approved render contract: an unknown
// recipient, a recipient outside the frozen audience, a duplicate, a parameter
// slot the campaign does not define, or a merged map that no longer renders.
func (s *HTTPServer) resolveRecipientParams(ctx context.Context, entries []recipientParams, campaignParams map[string]string, audience []int64) (map[int64]map[string]string, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	eligible := make(map[int64]struct{}, len(audience))
	for _, customerID := range audience {
		eligible[customerID] = struct{}{}
	}
	resolved := make(map[int64]map[string]string, len(entries))
	seen := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		if entry.CustomerShopifyID == "" {
			return nil, errors.New("recipient_params entries require customer_shopify_id")
		}
		if len(entry.Params) == 0 {
			return nil, errors.New("recipient_params entries require params")
		}
		if _, exists := seen[entry.CustomerShopifyID]; exists {
			return nil, errors.New("recipient_params contains a duplicate customer")
		}
		seen[entry.CustomerShopifyID] = struct{}{}
		for key := range entry.Params {
			if _, defined := campaignParams[key]; !defined {
				return nil, fmt.Errorf("recipient_params key %q is not a campaign template parameter", key)
			}
		}
		merged := mergeTemplateParams(campaignParams, entry.Params)
		if _, err := workflow.OrderedParameterBindings(merged, "header"); err != nil {
			return nil, fmt.Errorf("recipient header parameters are invalid: %w", err)
		}
		if _, err := workflow.OrderedParameterBindings(merged, "body"); err != nil {
			return nil, fmt.Errorf("recipient body parameters are invalid: %w", err)
		}
		var customerID int64
		if err := s.Store.DB.QueryRowContext(ctx, `SELECT id FROM customers WHERE shopify_id=?`, entry.CustomerShopifyID).Scan(&customerID); err != nil {
			return nil, errors.New("recipient_params names a customer that does not exist")
		}
		if _, ok := eligible[customerID]; !ok {
			return nil, errors.New("recipient_params names a customer outside the frozen audience")
		}
		resolved[customerID] = entry.Params
	}
	return resolved, nil
}

// hashSegmentPhones converts the plaintext numbers a caller supplies into the
// canonical phone hashes the customer table stores, then clears the plaintext.
// The operator CLI has no access to the hash key, so this is the only place the
// conversion can happen, and no plaintext number ever reaches storage.
func (s *HTTPServer) hashSegmentPhones(definition segment.Definition) (segment.Definition, error) {
	if definition.Kind != "frozen_phones" {
		return definition, nil
	}
	if len(definition.PhoneHashes) > 0 {
		return definition, errors.New("phone_hashes cannot be supplied; send phones and the daemon will hash them")
	}
	hashes := make([]string, 0, len(definition.Phones))
	for _, phone := range definition.Phones {
		digits := normalizePhone(phone, s.Config.DefaultCountryCode)
		if digits == "" {
			return definition, errors.New("phones contains a value with no digits")
		}
		hashes = append(hashes, security.KeyedHash(s.Config.PIIHashKey, digits))
	}
	definition.Phones = nil
	definition.PhoneHashes = hashes
	return definition, nil
}

func (s *HTTPServer) segmentPreview(w http.ResponseWriter, r *http.Request) {
	var definition segment.Definition
	if !decodeJSON(w, r, &definition) {
		return
	}
	definition, err := s.hashSegmentPhones(definition)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	result, err := segment.Preview(r.Context(), s.Store.DB, definition, time.Now())
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	writeJSON(w, 200, result)
}
func (s *HTTPServer) createCampaign(w http.ResponseWriter, r *http.Request) {
	var request campaignRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	if request.Name == "" || request.Template == "" || request.Language == "" {
		http.Error(w, "name, template, and language are required", 400)
		return
	}
	if request.TrackedURL != "" {
		if _, err := security.SafeDestination(request.TrackedURL, s.Config.RedirectAllowedHosts); err != nil {
			http.Error(w, "tracked URL must use an allowed HTTPS destination host", 400)
			return
		}
	}
	if request.HeaderImageURL != "" {
		if err := workflow.ValidateHeaderImageURL(request.HeaderImageURL); err != nil {
			http.Error(w, "header image URL must be a valid public HTTPS image URL", 400)
			return
		}
	}
	if _, err := workflow.OrderedParameterBindings(request.Params, "header"); err != nil {
		http.Error(w, "invalid header template parameters", 400)
		return
	}
	if _, err := workflow.OrderedParameterBindings(request.Params, "body"); err != nil {
		http.Error(w, "invalid body template parameters", 400)
		return
	}
	if request.ScheduledAt != "" {
		if _, err := time.Parse(time.RFC3339, request.ScheduledAt); err != nil {
			http.Error(w, "scheduled_at must use RFC3339", 400)
			return
		}
	}
	if request.FrequencyMessages == 0 {
		request.FrequencyMessages = 1
	}
	if request.FrequencyWindow == "" {
		request.FrequencyWindow = "24h"
	}
	if request.FrequencyMessages < 1 || request.FrequencyMessages > 100 {
		http.Error(w, "frequency_messages must be between 1 and 100", 400)
		return
	}
	if _, err := time.ParseDuration(request.FrequencyWindow); err != nil {
		http.Error(w, "frequency_window must be a duration", 400)
		return
	}
	hashedSegment, err := s.hashSegmentPhones(request.Segment)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	request.Segment = hashedSegment
	result, err := segment.Preview(r.Context(), s.Store.DB, request.Segment, time.Now())
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	perRecipient, err := s.resolveRecipientParams(r.Context(), request.RecipientParams, request.Params, result.CustomerIDs)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	segmentJSON, _ := json.Marshal(request.Segment)
	paramsJSON, _ := json.Marshal(request.Params)
	exclusionsJSON, _ := json.Marshal(result.Exclusions)
	id := store.NewID("campaign")
	_, err = s.Store.DB.ExecContext(r.Context(), `INSERT INTO campaigns(id,name,segment_json,exclusions_json,template_name,template_language,scheduled_at,state,audience_count,created_at,tracked_url,frequency_messages,frequency_window,header_image_url,template_params_json) VALUES(?,?,?,?,?,?,?,'draft',?,?,?,?,?,?,?)`, id, request.Name, string(segmentJSON), string(exclusionsJSON), request.Template, request.Language, nullableHTTP(request.ScheduledAt), result.EligibleCount, time.Now().UTC().Format(time.RFC3339Nano), nullableHTTP(request.TrackedURL), request.FrequencyMessages, request.FrequencyWindow, nullableHTTP(request.HeaderImageURL), string(paramsJSON))
	if err != nil {
		http.Error(w, "failed", 500)
		return
	}
	for _, customerID := range result.CustomerIDs {
		recipientJSON := "{}"
		if params, ok := perRecipient[customerID]; ok {
			encoded, _ := json.Marshal(params)
			recipientJSON = string(encoded)
		}
		_, err = s.Store.DB.ExecContext(r.Context(), `INSERT INTO campaign_recipients(campaign_id,customer_id,template_params_json) VALUES(?,?,?) ON CONFLICT DO NOTHING`, id, customerID, recipientJSON)
		if err != nil {
			http.Error(w, "failed", 500)
			return
		}
	}
	details, _ := json.Marshal(map[string]any{"audience_count": result.EligibleCount, "segment": request.Segment.Public(), "exclusions": result.Exclusions, "tracked_url": request.TrackedURL, "recipients_with_parameters": len(perRecipient)})
	_ = s.Store.Audit(r.Context(), "operator", "campaign.create", "campaign", id, string(details))
	writeJSON(w, 201, map[string]any{"id": id, "state": "draft", "audience_count": result.EligibleCount, "segment": request.Segment.Public(), "exclusions": result.Exclusions, "tracked_url": request.TrackedURL, "frequency_messages": request.FrequencyMessages, "frequency_window": request.FrequencyWindow, "recipients_with_parameters": len(perRecipient)})
}
func (s *HTTPServer) getCampaign(w http.ResponseWriter, r *http.Request) {
	var id, name, segmentJSON, exclusionsJSON, template, language, state, created string
	var scheduled sql.NullString
	var count int
	var trackedURL sql.NullString
	var frequencyMessages int
	var frequencyWindow, paramsJSON string
	var headerImageURL sql.NullString
	err := s.Store.DB.QueryRowContext(r.Context(), `SELECT id,name,segment_json,exclusions_json,template_name,template_language,scheduled_at,state,audience_count,created_at,tracked_url,frequency_messages,frequency_window,header_image_url,template_params_json FROM campaigns WHERE id=?`, r.PathValue("id")).Scan(&id, &name, &segmentJSON, &exclusionsJSON, &template, &language, &scheduled, &state, &count, &created, &trackedURL, &frequencyMessages, &frequencyWindow, &headerImageURL, &paramsJSON)
	if err != nil {
		http.Error(w, "not found", 404)
		return
	}
	var definition segment.Definition
	var exclusions any
	_ = json.Unmarshal([]byte(segmentJSON), &definition)
	_ = json.Unmarshal([]byte(exclusionsJSON), &exclusions)
	var params map[string]string
	_ = json.Unmarshal([]byte(paramsJSON), &params)
	var recipientsWithParams int
	_ = s.Store.DB.QueryRowContext(r.Context(), `SELECT count(*) FROM campaign_recipients WHERE campaign_id=? AND template_params_json NOT IN ('{}','')`, id).Scan(&recipientsWithParams)
	writeJSON(w, 200, map[string]any{"id": id, "name": name, "segment": definition.Public(), "exclusions": exclusions, "template": template, "language": language, "scheduled_at": nullableJSON(scheduled), "state": state, "audience_count": count, "created_at": created, "tracked_url": nullableJSON(trackedURL), "frequency_messages": frequencyMessages, "frequency_window": frequencyWindow, "has_header_image": headerImageURL.Valid, "template_parameter_count": len(params), "recipients_with_parameters": recipientsWithParams})
}
func (s *HTTPServer) activateCampaign(w http.ResponseWriter, r *http.Request) {
	if !s.Config.ProductionFlowEnabled || !s.Config.OutboundSendingEnabled {
		http.Error(w, "production and outbound gates must both be enabled", 409)
		return
	}
	var request campaignRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	campaignID := r.PathValue("id")
	var template, language, state, scheduledText, frequencyWindow, segmentJSON, paramsJSON string
	var audience int
	var scheduled, trackedURL, headerImageURL sql.NullString
	var frequencyMessages int
	err := s.Store.DB.QueryRowContext(r.Context(), `SELECT template_name,template_language,state,scheduled_at,audience_count,tracked_url,frequency_messages,frequency_window,segment_json,header_image_url,template_params_json FROM campaigns WHERE id=?`, campaignID).Scan(&template, &language, &state, &scheduled, &audience, &trackedURL, &frequencyMessages, &frequencyWindow, &segmentJSON, &headerImageURL, &paramsJSON)
	if err != nil {
		http.Error(w, "not found", 404)
		return
	}
	if state != "draft" {
		http.Error(w, "campaign is not a draft", 409)
		return
	}
	if request.ConfirmedRecipientCount != audience {
		http.Error(w, "confirmed recipient count does not match the frozen audience", 409)
		return
	}
	at := time.Now()
	if scheduled.Valid {
		scheduledText = scheduled.String
		parsed, err := time.Parse(time.RFC3339, scheduled.String)
		if err != nil {
			http.Error(w, "invalid scheduled time", 500)
			return
		}
		at = parsed
	}
	rows, err := s.Store.DB.QueryContext(r.Context(), `SELECT customer_id,template_params_json FROM campaign_recipients WHERE campaign_id=? AND exclusion_reason IS NULL ORDER BY customer_id`, campaignID)
	if err != nil {
		http.Error(w, "failed", 500)
		return
	}
	var customerIDs []int64
	recipientParamsByCustomer := map[int64]map[string]string{}
	for rows.Next() {
		var customerID int64
		var recipientJSON string
		if rows.Scan(&customerID, &recipientJSON) != nil {
			rows.Close()
			http.Error(w, "failed", 500)
			return
		}
		var params map[string]string
		if err := json.Unmarshal([]byte(recipientJSON), &params); err != nil {
			rows.Close()
			http.Error(w, "stored recipient template parameters are invalid", 500)
			return
		}
		if len(params) > 0 {
			recipientParamsByCustomer[customerID] = params
		}
		customerIDs = append(customerIDs, customerID)
	}
	if err := rows.Close(); err != nil {
		http.Error(w, "failed", 500)
		return
	}
	var definition segment.Definition
	if err := json.Unmarshal([]byte(segmentJSON), &definition); err != nil {
		http.Error(w, "stored segment is invalid", 500)
		return
	}
	current, err := segment.Preview(r.Context(), s.Store.DB, definition, time.Now())
	if err != nil {
		http.Error(w, "segment could not be revalidated", 500)
		return
	}
	if !sameCustomerIDs(customerIDs, current.CustomerIDs) {
		http.Error(w, "campaign audience changed; create a new draft and review the new count", http.StatusConflict)
		return
	}
	tx, err := s.Store.DB.BeginTx(r.Context(), nil)
	if err != nil {
		http.Error(w, "failed", 500)
		return
	}
	defer tx.Rollback()
	activatedAt := time.Now().UTC().Format(time.RFC3339Nano)
	var params map[string]string
	if err := json.Unmarshal([]byte(paramsJSON), &params); err != nil {
		http.Error(w, "stored template parameters are invalid", 500)
		return
	}
	for _, customerID := range customerIDs {
		payload, _ := json.Marshal(sendPayload{CustomerID: customerID, CampaignID: campaignID, Template: template, Language: language, Category: "MARKETING", TrackedURL: trackedURL.String, HeaderImageURL: headerImageURL.String, Params: mergeTemplateParams(params, recipientParamsByCustomer[customerID]), FrequencyMessages: frequencyMessages, FrequencyWindow: frequencyWindow})
		jobID := store.NewID("job")
		result, err := tx.ExecContext(r.Context(), `INSERT INTO scheduled_jobs(id,step_id,idempotency_key,kind,payload,scheduled_at,available_at,max_attempts,created_at,updated_at)
			VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(idempotency_key) DO NOTHING`, jobID, "campaign", "campaign:"+campaignID+":"+fmt.Sprint(customerID), "send_whatsapp", payload, at.UTC().Format(time.RFC3339Nano), at.UTC().Format(time.RFC3339Nano), 8, activatedAt, activatedAt)
		if err != nil {
			http.Error(w, "failed", 500)
			return
		}
		inserted, err := result.RowsAffected()
		if err != nil {
			http.Error(w, "failed", 500)
			return
		}
		if inserted == 1 {
			if _, err := tx.ExecContext(r.Context(), `UPDATE campaign_recipients SET queued_at=? WHERE campaign_id=? AND customer_id=?`, activatedAt, campaignID, customerID); err != nil {
				http.Error(w, "failed", 500)
				return
			}
		}
	}
	result, err := tx.ExecContext(r.Context(), `UPDATE campaigns SET state='scheduled',activated_at=? WHERE id=? AND state='draft'`, activatedAt, campaignID)
	if err != nil {
		http.Error(w, "failed", 500)
		return
	}
	changed, err := result.RowsAffected()
	if err != nil || changed != 1 {
		http.Error(w, "campaign activation changed concurrently", http.StatusConflict)
		return
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, "failed", 500)
		return
	}
	_ = scheduledText
	_ = s.Store.Audit(r.Context(), "operator", "campaign.activate", "campaign", campaignID, fmt.Sprintf(`{"audience_count":%d}`, audience))
	writeJSON(w, 200, map[string]any{"id": campaignID, "state": "scheduled", "recipient_count": audience})
}

func sameCustomerIDs(left, right []int64) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func (s *HTTPServer) cancelCampaign(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	now := time.Now().UTC().Format(time.RFC3339Nano)
	tx, err := s.Store.DB.BeginTx(r.Context(), nil)
	if err != nil {
		http.Error(w, "failed", 500)
		return
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(r.Context(), `UPDATE campaigns SET state='cancelled' WHERE id=? AND state IN ('draft','scheduled')`, id)
	if err != nil {
		http.Error(w, "failed", 500)
		return
	}
	count, _ := result.RowsAffected()
	if count != 1 {
		http.Error(w, "campaign is not cancellable", 409)
		return
	}
	if _, err := tx.ExecContext(r.Context(), `UPDATE scheduled_jobs SET state='cancelled',last_error='campaign cancelled',completed_at=?,updated_at=? WHERE idempotency_key LIKE ? AND state IN ('scheduled','retry','paused')`, now, now, "campaign:"+id+":%"); err != nil {
		http.Error(w, "failed", 500)
		return
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, "failed", 500)
		return
	}
	_ = s.Store.Audit(r.Context(), "operator", "campaign.cancel", "campaign", id, "{}")
	writeJSON(w, 200, map[string]any{"cancelled": id})
}

func (s *HTTPServer) importConsent(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Source  string `json:"source"`
		Records []struct {
			Phone     string `json:"phone"`
			Consent   string `json:"consent"`
			ConsentAt string `json:"consent_at"`
		} `json:"records"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	if request.Source == "" || len(request.Records) == 0 || len(request.Records) > 10000 {
		http.Error(w, "source and 1-10000 records are required", 400)
		return
	}
	matched, unmatched := 0, 0
	for _, record := range request.Records {
		if record.Consent != "opted_in" && record.Consent != "opted_out" {
			http.Error(w, "consent must be opted_in or opted_out", 400)
			return
		}
		stamp := record.ConsentAt
		if stamp == "" {
			stamp = time.Now().UTC().Format(time.RFC3339Nano)
		} else if _, err := time.Parse(time.RFC3339, stamp); err != nil {
			http.Error(w, "consent_at must use RFC3339", 400)
			return
		}
		hash := security.KeyedHash(s.Config.PIIHashKey, normalizePhone(record.Phone, s.Config.DefaultCountryCode))
		result, err := s.Store.DB.ExecContext(r.Context(), `UPDATE customers SET whatsapp_consent=?,consent_updated_at=?,suppressed_at=CASE WHEN ?='opted_out' THEN ? ELSE NULL END,suppression_reason=CASE WHEN ?='opted_out' THEN 'consent import' ELSE NULL END,updated_at=? WHERE phone_hash=?`, record.Consent, stamp, record.Consent, stamp, record.Consent, time.Now().UTC().Format(time.RFC3339Nano), hash)
		if err != nil {
			http.Error(w, "failed", 500)
			return
		}
		count, _ := result.RowsAffected()
		if count == 0 {
			unmatched++
			continue
		}
		matched++
		if record.Consent == "opted_out" {
			var customerID int64
			if s.Store.DB.QueryRowContext(r.Context(), `SELECT id FROM customers WHERE phone_hash=?`, hash).Scan(&customerID) == nil {
				_ = s.Store.CancelCustomerWork(r.Context(), customerID, "consent import opt-out")
			}
		}
	}
	details, _ := json.Marshal(map[string]any{"source": request.Source, "matched": matched, "unmatched": unmatched})
	_ = s.Store.Audit(r.Context(), "operator", "consent.import", "consent", "", string(details))
	writeJSON(w, 200, map[string]any{"matched": matched, "unmatched": unmatched, "source": request.Source})
}

// reserveExternalMessage accounts for a CLI send before Meta is called. A
// refusal here means the caller must not send: either the recipient has no
// customer record, so the message could never be attributed, or this exact
// message was already sent.
func (s *HTTPServer) reserveExternalMessage(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Phone                string `json:"phone"`
		Template             string `json:"template"`
		Language             string `json:"language"`
		Category             string `json:"category"`
		IdempotencyKey       string `json:"idempotency_key"`
		ParameterFingerprint string `json:"parameter_fingerprint,omitempty"`
		AttemptedAt          string `json:"attempted_at"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	phone := normalizePhone(request.Phone, s.Config.DefaultCountryCode)
	if phone == "" || request.Template == "" || request.Language == "" || request.IdempotencyKey == "" {
		http.Error(w, "phone, template, language, and idempotency_key are required", 400)
		return
	}
	attempted := request.AttemptedAt
	if attempted == "" {
		attempted = time.Now().UTC().Format(time.RFC3339Nano)
	} else if _, err := time.Parse(time.RFC3339, attempted); err != nil {
		http.Error(w, "attempted_at must use RFC3339", 400)
		return
	}
	category := request.Category
	if category == "" {
		category = "MARKETING"
	}
	result, err := s.Store.ReserveExternalMessage(r.Context(), store.ExternalMessage{
		PhoneHash:            security.KeyedHash(s.Config.PIIHashKey, phone),
		Template:             request.Template,
		Language:             request.Language,
		Category:             category,
		IdempotencyKey:       request.IdempotencyKey,
		ParameterFingerprint: request.ParameterFingerprint,
		AttemptedAt:          attempted,
	})
	if err != nil {
		s.Logger.Error("reserve external message", "error", err)
		http.Error(w, "failed", 500)
		return
	}
	switch {
	case result.Unresolved:
		writeJSON(w, 200, map[string]any{"reserved": false, "reason": "unknown_recipient",
			"detail": "no customer matches this number; import the recipient from Shopify first"})
	case result.AlreadySent:
		writeJSON(w, 200, map[string]any{"reserved": false, "reason": "already_sent", "message_id": result.MessageID})
	case result.Unreconciled:
		writeJSON(w, 200, map[string]any{"reserved": false, "reason": "unreconciled", "message_id": result.MessageID,
			"detail": "an earlier attempt never reported its outcome; reconcile it before sending again"})
	default:
		writeJSON(w, 200, map[string]any{"reserved": true, "message_id": result.MessageID})
	}
}

// recordExternalMessage puts a send made outside the daemon — today only the
// operator CLI — into outbound_messages, so its Meta status webhooks resolve,
// it appears in reporting, and read-touch revenue attribution can reach it.
// It is idempotent on the caller's idempotency key, which lets the CLI replay
// its ledger after the daemon was unreachable.
func (s *HTTPServer) recordExternalMessage(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Phone                string `json:"phone"`
		Template             string `json:"template"`
		Language             string `json:"language"`
		Category             string `json:"category"`
		IdempotencyKey       string `json:"idempotency_key"`
		ParameterFingerprint string `json:"parameter_fingerprint,omitempty"`
		MetaMessageID        string `json:"meta_message_id,omitempty"`
		Status               string `json:"status"`
		FailureCode          string `json:"failure_code,omitempty"`
		FailureReason        string `json:"failure_reason,omitempty"`
		AttemptedAt          string `json:"attempted_at"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	phone := normalizePhone(request.Phone, s.Config.DefaultCountryCode)
	if phone == "" || request.Template == "" || request.Language == "" || request.IdempotencyKey == "" {
		http.Error(w, "phone, template, language, and idempotency_key are required", 400)
		return
	}
	if request.Status != "accepted" && request.Status != "failed" {
		http.Error(w, "status must be accepted or failed", 400)
		return
	}
	if request.Status == "accepted" && request.MetaMessageID == "" {
		http.Error(w, "accepted records require meta_message_id", 400)
		return
	}
	attempted := request.AttemptedAt
	if attempted == "" {
		attempted = time.Now().UTC().Format(time.RFC3339Nano)
	} else if _, err := time.Parse(time.RFC3339, attempted); err != nil {
		http.Error(w, "attempted_at must use RFC3339", 400)
		return
	}
	category := request.Category
	if category == "" {
		category = "MARKETING"
	}

	result, err := s.Store.RecordExternalMessage(r.Context(), store.ExternalMessage{
		PhoneHash:            security.KeyedHash(s.Config.PIIHashKey, phone),
		Template:             request.Template,
		Language:             request.Language,
		Category:             category,
		IdempotencyKey:       request.IdempotencyKey,
		ParameterFingerprint: request.ParameterFingerprint,
		MetaMessageID:        request.MetaMessageID,
		Status:               request.Status,
		FailureCode:          request.FailureCode,
		// Meta puts the recipient's number in some error bodies, and this
		// column is written in plaintext and copied into backups.
		FailureReason: truncate(safeLogError(errors.New(request.FailureReason)), 500),
		AttemptedAt:   attempted,
	})
	if err != nil {
		s.Logger.Error("record external message", "error", err)
		http.Error(w, "failed", 500)
		return
	}
	if result.Unresolved {
		// Inventing a customer here would create a phone-only row that breaks
		// the Shopify upsert for that person; the operator has to reconcile.
		writeJSON(w, 200, map[string]any{
			"recorded": false,
			"reason":   "unknown_recipient",
			"detail":   "no customer matches this number; sync the recipient from Shopify or import consent, then replay with 'ledger sync'",
		})
		return
	}
	if result.Created || result.Upgraded {
		details, _ := json.Marshal(map[string]any{"source": "cli", "template": request.Template, "status": request.Status, "upgraded": result.Upgraded})
		_ = s.Store.Audit(r.Context(), "operator", "message.record_external", "message", result.MessageID, string(details))
	}
	if request.Status == "failed" && request.FailureCode != "" && result.CustomerID != 0 {
		if code, err := strconv.Atoi(request.FailureCode); err == nil {
			if _, err := s.Store.SuppressForMetaFailure(r.Context(), result.CustomerID, code); err != nil {
				s.Logger.Error("suppress after external failure", "error", err)
			}
		}
	}
	writeJSON(w, 200, map[string]any{
		"message_id":       result.MessageID,
		"recorded":         result.Created,
		"upgraded":         result.Upgraded,
		"already_recorded": !result.Created && !result.Upgraded,
	})
}

// importCustomers brings Shopify customers into the database without touching
// orders. A campaign can only target a customer that exists here, and a CLI
// send can only be recorded against one, so an audience drawn from Shopify
// must be imported before either can happen.
//
// It enqueues the same shopify_sync_customer job the webhook path uses, so the
// daemon fetches each customer itself and the consent rules stay in one place.
// It deliberately does not sync orders: order upserts start workflows and can
// send messages.
func (s *HTTPServer) importCustomers(w http.ResponseWriter, r *http.Request) {
	var request struct {
		ShopifyIDs []string `json:"shopify_ids"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	if len(request.ShopifyIDs) == 0 || len(request.ShopifyIDs) > 5000 {
		http.Error(w, "shopify_ids must hold 1-5000 entries", 400)
		return
	}
	// The key carries the minute so an accidental double submit dedupes while a
	// later re-import still runs; a customer's details change over time.
	batch := time.Now().UTC().Format("2006-01-02T15:04")
	queued, duplicate := 0, 0
	for _, shopifyID := range request.ShopifyIDs {
		shopifyID = strings.TrimSpace(shopifyID)
		if !strings.HasPrefix(shopifyID, "gid://shopify/Customer/") {
			http.Error(w, "every entry must be a Shopify customer GID", 400)
			return
		}
		payload, err := json.Marshal(map[string]string{"customer_id": shopifyID})
		if err != nil {
			http.Error(w, "failed", 500)
			return
		}
		enqueued, err := s.Store.EnqueueJob(r.Context(), store.Job{
			StepID:  "customer_import",
			Kind:    "shopify_sync_customer",
			Payload: payload,
		}, "customer-import:"+shopifyID+":"+batch, time.Now())
		if err != nil {
			s.Logger.Error("queue customer import", "error", err)
			http.Error(w, "failed", 500)
			return
		}
		if enqueued {
			queued++
		} else {
			duplicate++
		}
	}
	details, _ := json.Marshal(map[string]any{"queued": queued, "already_queued": duplicate})
	_ = s.Store.Audit(r.Context(), "operator", "customer.import", "customer", "", string(details))
	writeJSON(w, 202, map[string]any{"queued": queued, "already_queued": duplicate})
}

func (s *HTTPServer) authenticated(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		supplied := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if supplied == "" || subtle.ConstantTimeCompare([]byte(supplied), []byte(s.Config.APIKey)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
func (s *HTTPServer) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; script-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}
func (s *HTTPServer) requestLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		next.ServeHTTP(w, r)
		s.Logger.Info("http request", "method", r.Method, "path", safePath(r.URL.Path), "duration_ms", time.Since(started).Milliseconds())
	})
}
func safePath(path string) string {
	if strings.HasPrefix(path, "/r/") {
		return "/r/[redacted]"
	}
	if strings.HasPrefix(path, "/webhooks/gokwik/") {
		return "/webhooks/gokwik/[redacted]"
	}
	return path
}
func readBody(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		http.Error(w, "invalid body", 400)
		return nil, false
	}
	if len(body) == 0 {
		http.Error(w, "empty body", 400)
		return nil, false
	}
	return body, true
}
func decodeJSON(w http.ResponseWriter, r *http.Request, destination any) bool {
	body, ok := readBody(w, r, 1<<20)
	if !ok {
		return false
	}
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		http.Error(w, "invalid JSON", 400)
		return false
	}
	return true
}
func nullableHTTP(value string) any {
	if value == "" {
		return nil
	}
	return value
}
func nullableJSON(value sql.NullString) any {
	if !value.Valid {
		return nil
	}
	return value.String
}
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

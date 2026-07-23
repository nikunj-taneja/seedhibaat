package service

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
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
	mux.HandleFunc("GET /r/{token}", s.redirect)
	mux.Handle("GET /metrics", s.metricsAuthenticated(http.HandlerFunc(s.dashboard)))
	mux.Handle("GET /metrics/assets/dashboard.css", s.metricsAuthenticated(http.HandlerFunc(s.dashboardStyles)))
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
	ScheduledAt             string             `json:"scheduled_at,omitempty"`
	ConfirmedRecipientCount int                `json:"confirmed_recipient_count,omitempty"`
	FrequencyMessages       int                `json:"frequency_messages,omitempty"`
	FrequencyWindow         string             `json:"frequency_window,omitempty"`
}

func (s *HTTPServer) segmentPreview(w http.ResponseWriter, r *http.Request) {
	var definition segment.Definition
	if !decodeJSON(w, r, &definition) {
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
	result, err := segment.Preview(r.Context(), s.Store.DB, request.Segment, time.Now())
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	segmentJSON, _ := json.Marshal(request.Segment)
	exclusionsJSON, _ := json.Marshal(result.Exclusions)
	id := store.NewID("campaign")
	_, err = s.Store.DB.ExecContext(r.Context(), `INSERT INTO campaigns(id,name,segment_json,exclusions_json,template_name,template_language,scheduled_at,state,audience_count,created_at,tracked_url,frequency_messages,frequency_window) VALUES(?,?,?,?,?,?,?,'draft',?,?,?,?,?)`, id, request.Name, string(segmentJSON), string(exclusionsJSON), request.Template, request.Language, nullableHTTP(request.ScheduledAt), result.EligibleCount, time.Now().UTC().Format(time.RFC3339Nano), nullableHTTP(request.TrackedURL), request.FrequencyMessages, request.FrequencyWindow)
	if err != nil {
		http.Error(w, "failed", 500)
		return
	}
	for _, customerID := range result.CustomerIDs {
		_, err = s.Store.DB.ExecContext(r.Context(), `INSERT INTO campaign_recipients(campaign_id,customer_id) VALUES(?,?) ON CONFLICT DO NOTHING`, id, customerID)
		if err != nil {
			http.Error(w, "failed", 500)
			return
		}
	}
	details, _ := json.Marshal(map[string]any{"audience_count": result.EligibleCount, "segment": request.Segment, "exclusions": result.Exclusions, "tracked_url": request.TrackedURL})
	_ = s.Store.Audit(r.Context(), "operator", "campaign.create", "campaign", id, string(details))
	writeJSON(w, 201, map[string]any{"id": id, "state": "draft", "audience_count": result.EligibleCount, "segment": request.Segment, "exclusions": result.Exclusions, "tracked_url": request.TrackedURL, "frequency_messages": request.FrequencyMessages, "frequency_window": request.FrequencyWindow})
}
func (s *HTTPServer) getCampaign(w http.ResponseWriter, r *http.Request) {
	var id, name, segmentJSON, exclusionsJSON, template, language, state, created string
	var scheduled sql.NullString
	var count int
	var trackedURL sql.NullString
	var frequencyMessages int
	var frequencyWindow string
	err := s.Store.DB.QueryRowContext(r.Context(), `SELECT id,name,segment_json,exclusions_json,template_name,template_language,scheduled_at,state,audience_count,created_at,tracked_url,frequency_messages,frequency_window FROM campaigns WHERE id=?`, r.PathValue("id")).Scan(&id, &name, &segmentJSON, &exclusionsJSON, &template, &language, &scheduled, &state, &count, &created, &trackedURL, &frequencyMessages, &frequencyWindow)
	if err != nil {
		http.Error(w, "not found", 404)
		return
	}
	var definition any
	var exclusions any
	_ = json.Unmarshal([]byte(segmentJSON), &definition)
	_ = json.Unmarshal([]byte(exclusionsJSON), &exclusions)
	writeJSON(w, 200, map[string]any{"id": id, "name": name, "segment": definition, "exclusions": exclusions, "template": template, "language": language, "scheduled_at": nullableJSON(scheduled), "state": state, "audience_count": count, "created_at": created, "tracked_url": nullableJSON(trackedURL), "frequency_messages": frequencyMessages, "frequency_window": frequencyWindow})
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
	var template, language, state, scheduledText, frequencyWindow, segmentJSON string
	var audience int
	var scheduled, trackedURL sql.NullString
	var frequencyMessages int
	err := s.Store.DB.QueryRowContext(r.Context(), `SELECT template_name,template_language,state,scheduled_at,audience_count,tracked_url,frequency_messages,frequency_window,segment_json FROM campaigns WHERE id=?`, campaignID).Scan(&template, &language, &state, &scheduled, &audience, &trackedURL, &frequencyMessages, &frequencyWindow, &segmentJSON)
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
	rows, err := s.Store.DB.QueryContext(r.Context(), `SELECT customer_id FROM campaign_recipients WHERE campaign_id=? AND exclusion_reason IS NULL ORDER BY customer_id`, campaignID)
	if err != nil {
		http.Error(w, "failed", 500)
		return
	}
	var customerIDs []int64
	for rows.Next() {
		var customerID int64
		if rows.Scan(&customerID) != nil {
			rows.Close()
			http.Error(w, "failed", 500)
			return
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
	for _, customerID := range customerIDs {
		payload, _ := json.Marshal(sendPayload{CustomerID: customerID, CampaignID: campaignID, Template: template, Language: language, Category: "MARKETING", TrackedURL: trackedURL.String, FrequencyMessages: frequencyMessages, FrequencyWindow: frequencyWindow})
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
		hash := security.KeyedHash(s.Config.PIIHashKey, normalizePhone(record.Phone))
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
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
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

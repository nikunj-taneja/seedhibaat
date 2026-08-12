package service

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nikunj-taneja/seedhibaat/internal/config"
	"github.com/nikunj-taneja/seedhibaat/internal/store"
)

func testHTTPServer(t *testing.T) (*HTTPServer, *store.Store) {
	t.Helper()
	db, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return &HTTPServer{Config: config.Config{MetaAppSecret: "meta-secret", MetaVerifyToken: "verify", ShopifyClientSecret: "shop-secret", ShopifyShopDomain: "store.myshopify.com", APIKey: "api", RedirectAllowedHosts: map[string]bool{"shop.example.com": true}}, Store: db, Logger: logger}, db
}

func TestWebhookSignaturesAndDeduplication(t *testing.T) {
	server, db := testHTTPServer(t)
	defer db.Close()
	handler := server.Handler()
	body := []byte(`{"id":1}`)
	req := httptest.NewRequest(http.MethodPost, "/webhooks/shopify", bytes.NewReader(body))
	req.Header.Set("X-Shopify-Shop-Domain", "store.myshopify.com")
	req.Header.Set("X-Shopify-Webhook-Id", "one")
	req.Header.Set("X-Shopify-Topic", "orders/updated")
	mac := hmac.New(sha256.New, []byte("shop-secret"))
	mac.Write(body)
	req.Header.Set("X-Shopify-Hmac-Sha256", base64.StdEncoding.EncodeToString(mac.Sum(nil)))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != 200 {
		t.Fatalf("code=%d body=%s", response.Code, response.Body.String())
	}
	req = httptest.NewRequest(http.MethodPost, "/webhooks/shopify", bytes.NewReader(body))
	req.Header.Set("X-Shopify-Shop-Domain", "store.myshopify.com")
	req.Header.Set("X-Shopify-Webhook-Id", "one")
	req.Header.Set("X-Shopify-Topic", "orders/updated")
	req.Header.Set("X-Shopify-Hmac-Sha256", base64.StdEncoding.EncodeToString(mac.Sum(nil)))
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != 200 || !bytes.Contains(response.Body.Bytes(), []byte(`"duplicate":true`)) {
		t.Fatalf("duplicate response=%s", response.Body.String())
	}
	bad := httptest.NewRequest(http.MethodPost, "/webhooks/meta", bytes.NewReader(body))
	bad.Header.Set("X-Hub-Signature-256", "sha256="+fmt.Sprintf("%x", make([]byte, 32)))
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, bad)
	if response.Code != 401 {
		t.Fatalf("bad Meta signature code=%d", response.Code)
	}
}

func TestMetaVerificationAndAPIAuth(t *testing.T) {
	server, db := testHTTPServer(t)
	defer db.Close()
	handler := server.Handler()
	req := httptest.NewRequest(http.MethodGet, "/webhooks/meta?hub.mode=subscribe&hub.verify_token=verify&hub.challenge=hello", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != 200 || response.Body.String() != "hello" {
		t.Fatal(response.Code, response.Body.String())
	}
	req = httptest.NewRequest(http.MethodGet, "/api/v1/report", nil)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != 401 {
		t.Fatalf("code=%d", response.Code)
	}
}

func TestFrozenCampaignResponsesAndAuditDoNotExposeCustomerIDs(t *testing.T) {
	server, db := testHTTPServer(t)
	defer db.Close()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	shopifyID := "gid://shopify/Customer/private"
	_, err := db.DB.Exec(`INSERT INTO customers(id,shopify_id,phone_ciphertext,whatsapp_consent,created_at,updated_at) VALUES(1,?,x'01','opted_in',?,?)`, shopifyID, now, now)
	if err != nil {
		t.Fatal(err)
	}
	payload := fmt.Sprintf(`{"name":"private audience","segment":{"kind":"frozen_csv","require_whatsapp_consent":true,"customer_shopify_ids":[%q]},"template":"approved","language":"en_US"}`, shopifyID)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/campaigns", strings.NewReader(payload))
	req.Header.Set("Authorization", "Bearer api")
	req.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, req)
	if response.Code != http.StatusCreated {
		t.Fatalf("code=%d body=%s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), shopifyID) || !strings.Contains(response.Body.String(), `"frozen_count":1`) {
		t.Fatalf("unsafe response: %s", response.Body.String())
	}
	var created map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	campaignID, _ := created["id"].(string)
	req = httptest.NewRequest(http.MethodGet, "/api/v1/campaigns/"+campaignID, nil)
	req.Header.Set("Authorization", "Bearer api")
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, req)
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), shopifyID) {
		t.Fatalf("unsafe campaign response: %s", response.Body.String())
	}
	var auditDetails string
	if err := db.DB.QueryRow(`SELECT details_json FROM audit_log WHERE action='campaign.create' ORDER BY occurred_at DESC LIMIT 1`).Scan(&auditDetails); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(auditDetails, shopifyID) {
		t.Fatalf("audit leaked customer ID: %s", auditDetails)
	}
}

func TestWorkflowSimulationCalculatesScheduleWithoutWrites(t *testing.T) {
	server, db := testHTTPServer(t)
	defer db.Close()
	workflowYAML := `name: simulated_followup
version: 1
description: Simulation
enabled: false
timezone: Asia/Kolkata
trigger:
  type: order_delivered
audience:
  require_whatsapp_consent: true
quiet_hours:
  start: "21:00"
  end: "10:00"
frequency_cap:
  messages: 2
  window: 48h
conversion: {}
steps:
  - id: day_1
    wait: 1d
    template: first
    language: en_US
    category: MARKETING
  - id: day_28
    wait: 28d
    template: deadline
    language: en_US
    category: MARKETING
`
	requestBody, _ := json.Marshal(map[string]string{
		"yaml":         workflowYAML,
		"triggered_at": "2026-07-01T21:30:00+05:30",
		"as_of":        "2026-07-10T12:00:00+05:30",
	})
	request := httptest.NewRequest(http.MethodPost, "/api/v1/workflows/simulate", bytes.NewReader(requestBody))
	request.Header.Set("Authorization", "Bearer api")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("simulation code=%d body=%s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	if !strings.Contains(body, `"scheduled_at":"2026-07-03T10:00:00+05:30"`) ||
		!strings.Contains(body, `"scheduled_at":"2026-07-30T10:00:00+05:30"`) ||
		!strings.Contains(body, `"writes_performed":false`) {
		t.Fatalf("unexpected simulation: %s", body)
	}
	var definitions, runs, jobs int
	_ = db.DB.QueryRow(`SELECT count(*) FROM workflow_definitions`).Scan(&definitions)
	_ = db.DB.QueryRow(`SELECT count(*) FROM workflow_runs`).Scan(&runs)
	_ = db.DB.QueryRow(`SELECT count(*) FROM scheduled_jobs`).Scan(&jobs)
	if definitions != 0 || runs != 0 || jobs != 0 {
		t.Fatalf("simulation wrote state: definitions=%d runs=%d jobs=%d", definitions, runs, jobs)
	}
}

func TestMetricsDashboardIsReadOnlyAuthenticatedAndPrivate(t *testing.T) {
	server, db := testHTTPServer(t)
	defer db.Close()
	server.Config.MetricsEnabled = true
	server.Config.MetricsUsername = "operator"
	server.Config.MetricsPassword = "a-very-long-dashboard-password-value"
	server.Config.ReportTimezone = "UTC"
	server.Config.AttributionWindow = 30 * 24 * time.Hour
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, _ = db.DB.Exec(`INSERT INTO customers(id,shopify_id,phone_ciphertext,first_name_ciphertext,created_at,updated_at) VALUES(1,'private-customer',x'0102',x'0304',?,?)`, now, now)
	_, _ = db.DB.Exec(`INSERT INTO outbound_messages(id,customer_id,template_name,template_language,category,idempotency_key,state,attempted_at,accepted_at,sent_at,delivered_at,read_at,created_at,updated_at) VALUES('message',1,'welcome','en_US','MARKETING','key','read',?,?,?,?,?,?,?)`, now, now, now, now, now, now, now)
	workflowYAML := `name: example_followup
version: 1
description: Example
enabled: false
timezone: UTC
trigger:
  type: order_delivered
audience:
  require_whatsapp_consent: true
quiet_hours:
  start: "21:00"
  end: "09:00"
frequency_cap:
  messages: 1
  window: 24h
conversion: {}
steps:
  - id: follow_up
    wait: 1d
    template: example
    language: en_US
    category: MARKETING
`
	_, _ = db.DB.Exec(`INSERT INTO workflow_definitions(name,version,definition_hash,yaml,active,created_at) VALUES('example_followup',1,'hash',?,1,?)`, workflowYAML, now)
	_, _ = db.DB.Exec(`INSERT INTO workflow_runs(id,workflow_name,workflow_version,customer_id,trigger_type,trigger_id,state,started_at) VALUES('workflow-run','example_followup',1,1,'order_delivered','order','active',?)`, now)
	_, _ = db.DB.Exec(`INSERT INTO scheduled_jobs(id,workflow_run_id,step_id,idempotency_key,kind,payload,state,scheduled_at,available_at,created_at,updated_at) VALUES('workflow-job','workflow-run','follow_up','workflow-key','send_template',x'00','scheduled',?,?,?,?)`, now, now, now, now)

	request := httptest.NewRequest(http.MethodGet, "/metrics?range=7d", nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated code=%d", response.Code)
	}

	request = httptest.NewRequest(http.MethodGet, "/metrics?range=7d", nil)
	request.SetBasicAuth("operator", "a-very-long-dashboard-password-value")
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("dashboard code=%d body=%s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	if !strings.Contains(body, "Your WhatsApp analytics") || !strings.Contains(body, "Performance ledger") || !strings.Contains(body, "Active workflows") || !strings.Contains(body, "example_followup") {
		t.Fatalf("dashboard missing expected content")
	}
	if !strings.Contains(body, "Read rate") || !strings.Contains(body, "Meta read receipts") || strings.Contains(body, "Observed read") {
		t.Fatalf("dashboard read labels are not simplified")
	}
	if !strings.Contains(body, "CVR") || !strings.Contains(body, "converted ÷") {
		t.Fatalf("dashboard delivered-based CVR card is missing")
	}
	if !strings.Contains(body, "Unique clickers: 0") || !strings.Contains(body, `class="chart-hit"`) {
		t.Fatalf("dashboard chart hover statistics are missing")
	}
	if !strings.Contains(body, `fill="transparent" pointer-events="all"`) {
		t.Fatalf("dashboard chart hover targets are visibly rendered")
	}
	if strings.Contains(body, "private-customer") || strings.Contains(body, "0102") || strings.Contains(body, "0304") {
		t.Fatalf("dashboard leaked private customer data")
	}
	if response.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatalf("cache control=%q", response.Header().Get("Cache-Control"))
	}
}

func TestDailyChartBarsAreCenteredAndDoNotOverlap(t *testing.T) {
	bars := makeChartBars([]store.DailyMetric{
		{Date: "1 Jan", Delivered: 3, UniqueClicks: 1},
		{Date: "2 Jan", Delivered: 2, UniqueClicks: 2},
	})
	if len(bars) != 2 {
		t.Fatalf("bars=%d", len(bars))
	}
	for _, bar := range bars {
		if bar.ClickX < bar.X+9 {
			t.Fatalf("overlapping bars: delivered_x=%d click_x=%d", bar.X, bar.ClickX)
		}
		if bar.LabelX <= bar.X || bar.LabelX >= bar.ClickX+6 {
			t.Fatalf("label is not centered under pair: %+v", bar)
		}
	}
	if bars[0].X < 50 || bars[len(bars)-1].ClickX+6 > 930 {
		t.Fatalf("bars escaped plot bounds: first=%+v last=%+v", bars[0], bars[len(bars)-1])
	}
}

func TestCampaignActivationMaterializesAudienceBeforeQueueWrites(t *testing.T) {
	server, db := testHTTPServer(t)
	defer db.Close()
	server.Config.ProductionFlowEnabled = true
	server.Config.OutboundSendingEnabled = true
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, _ = db.DB.Exec(`INSERT INTO customers(id,shopify_id,phone_ciphertext,created_at,updated_at) VALUES(1,'customer',x'01',?,?)`, now, now)
	_, _ = db.DB.Exec(`INSERT INTO orders(shopify_id,customer_id,created_at,updated_at) VALUES('order',1,?,?)`, now, now)
	_, _ = db.DB.Exec(`INSERT INTO campaigns(id,name,segment_json,exclusions_json,template_name,template_language,state,audience_count,created_at,tracked_url,frequency_messages,frequency_window) VALUES('campaign','test','{"kind":"not_reordered","require_whatsapp_consent":false}','{}','template','en_US','draft',1,?,'https://shop.example.com/products/test',3,'168h')`, now)
	_, _ = db.DB.Exec(`INSERT INTO campaign_recipients(campaign_id,customer_id) VALUES('campaign',1)`)
	body := []byte(`{"confirmed_recipient_count":1}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/campaigns/campaign/activate", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer api")
	req.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, req)
	if response.Code != 200 {
		t.Fatalf("code=%d body=%s", response.Code, response.Body.String())
	}
	var queued int
	if err := db.DB.QueryRow(`SELECT count(*) FROM scheduled_jobs WHERE state='scheduled'`).Scan(&queued); err != nil || queued != 1 {
		t.Fatalf("queued=%d err=%v", queued, err)
	}
	var payload []byte
	if err := db.DB.QueryRow(`SELECT payload FROM scheduled_jobs`).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(payload, []byte(`"tracked_url":"https://shop.example.com/products/test"`)) || !bytes.Contains(payload, []byte(`"frequency_messages":3`)) {
		t.Fatalf("frozen delivery configuration missing: %s", payload)
	}
}

func TestCampaignActivationRejectsAudienceDriftAfterRefund(t *testing.T) {
	server, db := testHTTPServer(t)
	defer db.Close()
	server.Config.ProductionFlowEnabled = true
	server.Config.OutboundSendingEnabled = true
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, _ = db.DB.Exec(`INSERT INTO customers(id,shopify_id,phone_ciphertext,created_at,updated_at) VALUES(1,'customer',x'01',?,?)`, now, now)
	_, _ = db.DB.Exec(`INSERT INTO orders(shopify_id,customer_id,refunded_at,created_at,updated_at) VALUES('order',1,?, ?,?)`, now, now, now)
	_, _ = db.DB.Exec(`INSERT INTO campaigns(id,name,segment_json,exclusions_json,template_name,template_language,state,audience_count,created_at,frequency_messages,frequency_window) VALUES('campaign','test','{"kind":"not_reordered","require_whatsapp_consent":false}','{}','template','en_US','draft',1,?,1,'24h')`, now)
	_, _ = db.DB.Exec(`INSERT INTO campaign_recipients(campaign_id,customer_id) VALUES('campaign',1)`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/campaigns/campaign/activate", bytes.NewReader([]byte(`{"confirmed_recipient_count":1}`)))
	req.Header.Set("Authorization", "Bearer api")
	req.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, req)
	if response.Code != http.StatusConflict {
		t.Fatalf("code=%d body=%s", response.Code, response.Body.String())
	}
	var jobs int
	_ = db.DB.QueryRow(`SELECT count(*) FROM scheduled_jobs`).Scan(&jobs)
	if jobs != 0 {
		t.Fatalf("queued stale audience jobs=%d", jobs)
	}
}

func TestWorkflowActivationRequiresExactInitialRecipientCount(t *testing.T) {
	server, db := testHTTPServer(t)
	defer db.Close()
	server.Config.ProductionFlowEnabled = true
	server.Config.OutboundSendingEnabled = true
	body, err := os.ReadFile("../../config/workflows/post_delivery_followup_example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, _ = db.DB.Exec(`INSERT INTO workflow_definitions(name,version,definition_hash,yaml,active,created_at) VALUES('post_delivery_followup_example',1,'hash',?,0,?)`, string(body), now)
	request := func(count int) *httptest.ResponseRecorder {
		payload := []byte(fmt.Sprintf(`{"confirmed_recipient_count":%d}`, count))
		req := httptest.NewRequest(http.MethodPost, "/api/v1/workflows/post_delivery_followup_example/activate", bytes.NewReader(payload))
		req.Header.Set("Authorization", "Bearer api")
		req.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, req)
		return response
	}
	if response := request(1); response.Code != http.StatusConflict {
		t.Fatalf("mismatch code=%d body=%s", response.Code, response.Body.String())
	}
	if response := request(0); response.Code != http.StatusOK {
		t.Fatalf("activation code=%d body=%s", response.Code, response.Body.String())
	}
}

func TestWorkflowPauseAndConfirmedActivationControlExistingRuns(t *testing.T) {
	server, db := testHTTPServer(t)
	defer db.Close()
	server.Config.ProductionFlowEnabled = true
	server.Config.OutboundSendingEnabled = true
	body, _ := os.ReadFile("../../config/workflows/post_delivery_followup_example.yaml")
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, _ = db.DB.Exec(`INSERT INTO customers(id,shopify_id,created_at,updated_at) VALUES(1,'customer',?,?)`, now, now)
	_, _ = db.DB.Exec(`INSERT INTO workflow_definitions(name,version,definition_hash,yaml,active,created_at) VALUES('post_delivery_followup_example',1,'hash',?,1,?)`, string(body), now)
	_, _ = db.DB.Exec(`INSERT INTO workflow_runs(id,workflow_name,workflow_version,customer_id,trigger_type,trigger_id,state,started_at) VALUES('run','post_delivery_followup_example',1,1,'order_delivered','order','active',?)`, now)
	_, _ = db.DB.Exec(`INSERT INTO scheduled_jobs(id,workflow_run_id,step_id,idempotency_key,kind,payload,scheduled_at,available_at,state,created_at,updated_at) VALUES('job','run','step','key','send_whatsapp',x'7b7d',?,?, 'scheduled',?,?)`, now, now, now, now)
	request := func(path, payload string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader([]byte(payload)))
		req.Header.Set("Authorization", "Bearer api")
		req.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, req)
		return response
	}
	if response := request("/api/v1/workflows/post_delivery_followup_example/pause", `{}`); response.Code != http.StatusOK {
		t.Fatalf("pause code=%d body=%s", response.Code, response.Body.String())
	}
	var runState, jobState string
	_ = db.DB.QueryRow(`SELECT state FROM workflow_runs WHERE id='run'`).Scan(&runState)
	_ = db.DB.QueryRow(`SELECT state FROM scheduled_jobs WHERE id='job'`).Scan(&jobState)
	if runState != "paused" || jobState != "paused" {
		t.Fatalf("paused run=%s job=%s", runState, jobState)
	}
	if response := request("/api/v1/workflows/post_delivery_followup_example/activate", `{"confirmed_recipient_count":1}`); response.Code != http.StatusOK {
		t.Fatalf("activate code=%d body=%s", response.Code, response.Body.String())
	}
	_ = db.DB.QueryRow(`SELECT state FROM workflow_runs WHERE id='run'`).Scan(&runState)
	_ = db.DB.QueryRow(`SELECT state FROM scheduled_jobs WHERE id='job'`).Scan(&jobState)
	if runState != "active" || jobState != "scheduled" {
		t.Fatalf("resumed run=%s job=%s", runState, jobState)
	}
}

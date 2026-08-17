package service

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
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
	"github.com/nikunj-taneja/seedhibaat/internal/meta"
	"github.com/nikunj-taneja/seedhibaat/internal/security"
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
	if !strings.Contains(body, "Delivery rate") || !strings.Contains(body, "delivered of") {
		t.Fatalf("dashboard delivery-rate card is missing")
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

	request = httptest.NewRequest(http.MethodGet, "/metrics?range=1d", nil)
	request.SetBasicAuth("operator", "a-very-long-dashboard-password-value")
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("dashboard 1d code=%d body=%s", response.Code, response.Body.String())
	}
	body = response.Body.String()
	if !strings.Contains(body, "Last 24 hours") {
		t.Fatalf("1d range fell back to another window")
	}
	if !strings.Contains(body, `href="/metrics?range=1d"`) {
		t.Fatalf("1d range selector is missing")
	}
}

func TestDashboardScriptIsServedAsAnAssetAndAllowedByPolicy(t *testing.T) {
	server, db := testHTTPServer(t)
	defer db.Close()
	server.Config.MetricsEnabled = true
	server.Config.MetricsUsername = "operator"
	server.Config.MetricsPassword = "a-very-long-dashboard-password-value"
	server.Config.ReportTimezone = "UTC"
	server.Config.AttributionWindow = 30 * 24 * time.Hour

	request := httptest.NewRequest(http.MethodGet, "/metrics?range=7d", nil)
	request.SetBasicAuth("operator", "a-very-long-dashboard-password-value")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("dashboard code=%d body=%s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `src="/metrics/assets/dashboard.js"`) {
		t.Fatalf("dashboard does not reference the script asset")
	}
	if policy := response.Header().Get("Content-Security-Policy"); !strings.Contains(policy, "script-src 'self'") {
		t.Fatalf("content security policy blocks the script asset: %q", policy)
	}

	request = httptest.NewRequest(http.MethodGet, "/metrics/assets/dashboard.js", nil)
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("script asset served without authentication: code=%d", response.Code)
	}

	request = httptest.NewRequest(http.MethodGet, "/metrics/assets/dashboard.js", nil)
	request.SetBasicAuth("operator", "a-very-long-dashboard-password-value")
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("script asset code=%d", response.Code)
	}
	if contentType := response.Header().Get("Content-Type"); contentType != "application/javascript; charset=utf-8" {
		t.Fatalf("script asset content type=%q", contentType)
	}
	if !strings.Contains(response.Body.String(), "chart-tip") {
		t.Fatalf("script asset does not carry the tooltip code")
	}
}

func TestDailyChartBarsAreCenteredAndDoNotOverlap(t *testing.T) {
	bars, ticks := makeChart([]store.DailyMetric{
		{Date: "1 Jan", Delivered: 3, UniqueClicks: 1},
		{Date: "2 Jan", Delivered: 2, UniqueClicks: 2},
	})
	if len(bars) != 2 {
		t.Fatalf("bars=%d", len(bars))
	}
	for _, bar := range bars {
		if bar.ClickX < bar.X+bar.Width {
			t.Fatalf("overlapping bars: delivered_x=%d width=%d click_x=%d", bar.X, bar.Width, bar.ClickX)
		}
		if bar.LabelX <= bar.X || bar.LabelX >= bar.ClickX+bar.ClickW {
			t.Fatalf("label is not centered under pair: %+v", bar)
		}
		if bar.TopY > bar.Y || bar.TopY > bar.ClickY {
			t.Fatalf("tooltip anchor is below a bar top: %+v", bar)
		}
	}
	if bars[0].X < 50 || bars[len(bars)-1].ClickX+bars[len(bars)-1].ClickW > 930 {
		t.Fatalf("bars escaped plot bounds: first=%+v last=%+v", bars[0], bars[len(bars)-1])
	}
	if len(ticks) != 4 || ticks[0].Label != "0" {
		t.Fatalf("axis ticks=%+v", ticks)
	}
	if ticks[len(ticks)-1].Y >= ticks[0].Y {
		t.Fatalf("axis ticks are not ordered from the baseline up: %+v", ticks)
	}
}

func TestChartAxisRoundsUpToReadableSteps(t *testing.T) {
	for _, testCase := range []struct {
		peak int64
		want string
	}{
		{peak: 3, want: "1"},
		{peak: 72, want: "50"},
		{peak: 1000, want: "500"},
		{peak: 4200, want: "2k"},
	} {
		_, ticks := makeChart([]store.DailyMetric{{Date: "1 Jan", Delivered: testCase.peak}})
		if ticks[1].Label != testCase.want {
			t.Fatalf("peak=%d first step=%q want %q", testCase.peak, ticks[1].Label, testCase.want)
		}
	}
}

func TestChartBarsStayVisibleOnShortRanges(t *testing.T) {
	single, _ := makeChart([]store.DailyMetric{{Date: "1 Jan", Delivered: 5, UniqueClicks: 2}})
	if single[0].Width < 12 {
		t.Fatalf("single-day bar is too thin to read: width=%d", single[0].Width)
	}
	daily := make([]store.DailyMetric, 30)
	for index := range daily {
		daily[index] = store.DailyMetric{Date: "1 Jan", Delivered: 5, UniqueClicks: 2}
	}
	month, _ := makeChart(daily)
	for index := 1; index < len(month); index++ {
		if month[index].X < month[index-1].ClickX+month[index-1].ClickW {
			t.Fatalf("bars collide at 30 days: %+v %+v", month[index-1], month[index])
		}
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

func TestPhoneKeyedSegmentHashesInTheDaemonAndStoresNoPlaintext(t *testing.T) {
	server, db := testHTTPServer(t)
	defer db.Close()
	server.Config.PIIHashKey = "hash-key-value-for-tests"
	server.Config.DefaultCountryCode = "91"
	now := time.Now().UTC().Format(time.RFC3339Nano)
	// The CSV carries a local number; the stored hash is on the canonical form.
	local, canonical := "98765 00042", "919876500042"
	_, _ = db.DB.Exec(`INSERT INTO customers(id,shopify_id,phone_ciphertext,phone_hash,whatsapp_consent,created_at,updated_at) VALUES(1,'gid://shopify/Customer/1',x'01',?,'opted_in',?,?)`,
		security.KeyedHash(server.Config.PIIHashKey, canonical), now, now)

	body := fmt.Sprintf(`{"name":"phone audience","segment":{"kind":"frozen_phones","require_whatsapp_consent":true,"phones":[%q]},"template":"approved","language":"en_US"}`, local)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/campaigns", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer api")
	req.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, req)
	if response.Code != http.StatusCreated {
		t.Fatalf("code=%d body=%s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"audience_count":1`) {
		t.Fatalf("the local number did not match the canonical hash: %s", response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"frozen_count":1`) || strings.Contains(response.Body.String(), "phone_hashes") {
		t.Fatalf("response did not redact the audience: %s", response.Body.String())
	}

	var storedSegment string
	if err := db.DB.QueryRow(`SELECT segment_json FROM campaigns`).Scan(&storedSegment); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(storedSegment, local) || strings.Contains(storedSegment, canonical) || strings.Contains(storedSegment, `"phones"`) {
		t.Fatalf("stored segment kept a plaintext number: %s", storedSegment)
	}
	if !strings.Contains(storedSegment, security.KeyedHash(server.Config.PIIHashKey, canonical)) {
		t.Fatalf("stored segment lost its hashes: %s", storedSegment)
	}

	// A caller cannot bypass the daemon's hashing by supplying hashes itself.
	req = httptest.NewRequest(http.MethodPost, "/api/v1/segments/preview", strings.NewReader(`{"kind":"frozen_phones","require_whatsapp_consent":true,"phone_hashes":["forged"]}`))
	req.Header.Set("Authorization", "Bearer api")
	req.Header.Set("Content-Type", "application/json")
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, req)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("supplied hashes were accepted: code=%d body=%s", response.Code, response.Body.String())
	}
}

func TestPerRecipientTemplateParametersOverrideOnlyTheirOwnSlot(t *testing.T) {
	server, db := testHTTPServer(t)
	defer db.Close()
	server.Config.ProductionFlowEnabled = true
	server.Config.OutboundSendingEnabled = true
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, _ = db.DB.Exec(`INSERT INTO customers(id,shopify_id,phone_ciphertext,whatsapp_consent,created_at,updated_at) VALUES(1,'gid://shopify/Customer/1',x'01','opted_in',?,?)`, now, now)
	_, _ = db.DB.Exec(`INSERT INTO customers(id,shopify_id,phone_ciphertext,whatsapp_consent,created_at,updated_at) VALUES(2,'gid://shopify/Customer/2',x'02','opted_in',?,?)`, now, now)

	create := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/campaigns", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer api")
		req.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, req)
		return response
	}
	segmentJSON := `"segment":{"kind":"frozen_csv","require_whatsapp_consent":true,"customer_shopify_ids":["gid://shopify/Customer/1","gid://shopify/Customer/2"]}`
	campaignParams := `"params":{"body.1":"literal:SHARED"}`

	if response := create(`{"name":"extra slot",` + segmentJSON + `,"template":"approved","language":"en_US",` + campaignParams + `,"recipient_params":[{"customer_shopify_id":"gid://shopify/Customer/1","params":{"body.2":"literal:EXTRA"}}]}`); response.Code != http.StatusBadRequest {
		t.Fatalf("a recipient added a parameter slot: code=%d body=%s", response.Code, response.Body.String())
	}
	// A recipient outside the audience receives nothing, so their values are
	// moot rather than an error. The count says so instead of staying silent.
	response := create(`{"name":"outsider",` + segmentJSON + `,"template":"approved","language":"en_US",` + campaignParams + `,"recipient_params":[{"customer_shopify_id":"gid://shopify/Customer/absent","params":{"body.1":"literal:X"}}]}`)
	if response.Code != http.StatusCreated {
		t.Fatalf("an unmatched recipient broke the draft: code=%d body=%s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"recipient_parameters_not_in_audience":1`) {
		t.Fatalf("the unmatched recipient was not reported: %s", response.Body.String())
	}

	response = create(`{"name":"coupons",` + segmentJSON + `,"template":"approved","language":"en_US",` + campaignParams + `,"recipient_params":[{"customer_shopify_id":"gid://shopify/Customer/2","params":{"body.1":"literal:OWN-CODE"}}]}`)
	if response.Code != http.StatusCreated {
		t.Fatalf("create code=%d body=%s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"recipients_with_parameters":1`) {
		t.Fatalf("per-recipient count is not reported: %s", response.Body.String())
	}
	var created map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	campaignID, _ := created["id"].(string)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/campaigns/"+campaignID+"/activate", strings.NewReader(`{"confirmed_recipient_count":2}`))
	req.Header.Set("Authorization", "Bearer api")
	req.Header.Set("Content-Type", "application/json")
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, req)
	if response.Code != http.StatusOK {
		t.Fatalf("activate code=%d body=%s", response.Code, response.Body.String())
	}

	payloads := map[int64]string{}
	rows, err := db.DB.Query(`SELECT payload FROM scheduled_jobs`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var payload sendPayload
		if err := json.Unmarshal(raw, &payload); err != nil {
			t.Fatal(err)
		}
		payloads[payload.CustomerID] = payload.Params["body.1"]
	}
	if payloads[1] != "literal:SHARED" {
		t.Fatalf("recipient without its own value lost the campaign value: %q", payloads[1])
	}
	if payloads[2] != "literal:OWN-CODE" {
		t.Fatalf("recipient value did not reach the frozen payload: %q", payloads[2])
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

func recordExternal(t *testing.T, server *HTTPServer, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/messages/record", bytes.NewReader([]byte(body)))
	request.Header.Set("Authorization", "Bearer api")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	return response
}

func TestExternalMessageRecordingIsTraceableAndIdempotent(t *testing.T) {
	server, db := testHTTPServer(t)
	defer db.Close()
	server.Config.PIIHashKey = "hash-key-value-for-tests"
	now := time.Now().UTC().Format(time.RFC3339Nano)
	phoneHash := security.KeyedHash(server.Config.PIIHashKey, "919876500001")
	_, _ = db.DB.Exec(`INSERT INTO customers(id,shopify_id,phone_ciphertext,phone_hash,created_at,updated_at) VALUES(1,'gid://shopify/Customer/1',x'01',?,?,?)`, phoneHash, now, now)

	body := `{"phone":"+919876500001","template":"stay_upgrade","language":"en_US","category":"MARKETING",
		"idempotency_key":"key-one","parameter_fingerprint":"fp","meta_message_id":"wamid.ONE",
		"status":"accepted","attempted_at":"` + now + `"}`
	response := recordExternal(t, server, body)
	if response.Code != 200 {
		t.Fatalf("code=%d body=%s", response.Code, response.Body.String())
	}
	var first map[string]any
	_ = json.Unmarshal(response.Body.Bytes(), &first)
	if first["recorded"] != true {
		t.Fatalf("unexpected first response: %v", first)
	}

	var customerID int64
	var state, source, metaID string
	if err := db.DB.QueryRow(`SELECT customer_id,state,source,meta_message_id FROM outbound_messages WHERE idempotency_key='key-one'`).Scan(&customerID, &state, &source, &metaID); err != nil {
		t.Fatal(err)
	}
	if customerID != 1 || state != "accepted" || source != "cli" || metaID != "wamid.ONE" {
		t.Fatalf("row=%d %s %s %s", customerID, state, source, metaID)
	}

	// Replaying the same ledger line must not create a second message.
	repeat := recordExternal(t, server, body)
	var second map[string]any
	_ = json.Unmarshal(repeat.Body.Bytes(), &second)
	if second["recorded"] != false || second["already_recorded"] != true {
		t.Fatalf("replay was not idempotent: %v", second)
	}
	var count int
	if err := db.DB.QueryRow(`SELECT count(*) FROM outbound_messages`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("count=%d err=%v", count, err)
	}
}

func TestExternalMessageRecordingResolvesWebhooksAndAttribution(t *testing.T) {
	server, db := testHTTPServer(t)
	defer db.Close()
	server.Config.PIIHashKey = "hash-key-value-for-tests"
	now := time.Now().UTC().Format(time.RFC3339Nano)
	phoneHash := security.KeyedHash(server.Config.PIIHashKey, "919876500002")
	_, _ = db.DB.Exec(`INSERT INTO customers(id,shopify_id,phone_ciphertext,phone_hash,created_at,updated_at) VALUES(7,'gid://shopify/Customer/7',x'01',?,?,?)`, phoneHash, now, now)

	response := recordExternal(t, server, `{"phone":"919876500002","template":"stay_upgrade","language":"en_US",
		"idempotency_key":"key-two","meta_message_id":"wamid.TWO","status":"accepted","attempted_at":"`+now+`"}`)
	if response.Code != 200 {
		t.Fatalf("code=%d body=%s", response.Code, response.Body.String())
	}

	// A Meta status webhook for a CLI send must now resolve to the row.
	processor := &Processor{Store: db, Config: server.Config, Logger: server.Logger}
	if err := processor.recordMetaStatus(context.Background(), meta.MessageStatus{
		ID: "wamid.TWO", Status: "read", Timestamp: fmt.Sprint(time.Now().Unix()),
	}); err != nil {
		t.Fatal(err)
	}
	var readAt sql.NullString
	var state string
	if err := db.DB.QueryRow(`SELECT read_at,state FROM outbound_messages WHERE idempotency_key='key-two'`).Scan(&readAt, &state); err != nil {
		t.Fatal(err)
	}
	if !readAt.Valid || state != "read" {
		t.Fatalf("CLI send did not absorb its read receipt: read_at=%v state=%s", readAt, state)
	}
}

func TestExternalMessageRecordingRefusesToInventARecipient(t *testing.T) {
	server, db := testHTTPServer(t)
	defer db.Close()
	server.Config.PIIHashKey = "hash-key-value-for-tests"
	response := recordExternal(t, server, `{"phone":"919876500003","template":"stay_upgrade","language":"en_US",
		"idempotency_key":"key-three","meta_message_id":"wamid.THREE","status":"accepted"}`)
	if response.Code != 200 {
		t.Fatalf("code=%d body=%s", response.Code, response.Body.String())
	}
	var body map[string]any
	_ = json.Unmarshal(response.Body.Bytes(), &body)
	if body["recorded"] != false || body["reason"] != "unknown_recipient" {
		t.Fatalf("unknown recipient was not reported: %v", body)
	}
	var customers int
	if err := db.DB.QueryRow(`SELECT count(*) FROM customers`).Scan(&customers); err != nil || customers != 0 {
		t.Fatalf("a phone-only customer was invented: customers=%d err=%v", customers, err)
	}
}

// Regression: a phone-only customer row makes SQLite abort the Shopify upsert,
// which infers ON CONFLICT(shopify_id), so that customer's orders stop syncing.
func TestRecorderNeverCreatesRowsThatBreakTheShopifyUpsert(t *testing.T) {
	server, db := testHTTPServer(t)
	defer db.Close()
	server.Config.PIIHashKey = "hash-key-value-for-tests"
	phone := "919876500009"
	recordExternal(t, server, `{"phone":"`+phone+`","template":"t","language":"en_US",
		"idempotency_key":"key-nine","meta_message_id":"wamid.NINE","status":"accepted"}`)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := db.DB.Exec(`INSERT INTO customers(shopify_id,phone_ciphertext,phone_hash,whatsapp_consent,created_at,updated_at)
		VALUES('gid://shopify/Customer/9',x'01',?,'opted_in',?,?)
		ON CONFLICT(shopify_id) DO UPDATE SET phone_hash=excluded.phone_hash,updated_at=excluded.updated_at`,
		security.KeyedHash(server.Config.PIIHashKey, phone), now, now)
	if err != nil {
		t.Fatalf("Shopify upsert still blocked by a recorder-created row: %v", err)
	}
}

func TestExternalFailureCodeSuppressesTheRecipient(t *testing.T) {
	server, db := testHTTPServer(t)
	defer db.Close()
	server.Config.PIIHashKey = "hash-key-value-for-tests"
	now := time.Now().UTC().Format(time.RFC3339Nano)
	phone := "919876500010"
	_, _ = db.DB.Exec(`INSERT INTO customers(id,shopify_id,phone_ciphertext,phone_hash,whatsapp_consent,created_at,updated_at) VALUES(10,'gid://shopify/Customer/10',x'01',?,'opted_in',?,?)`,
		security.KeyedHash(server.Config.PIIHashKey, phone), now, now)
	response := recordExternal(t, server, `{"phone":"`+phone+`","template":"t","language":"en_US",
		"idempotency_key":"key-ten","status":"failed","failure_code":"131050","failure_reason":"opted out"}`)
	if response.Code != 200 {
		t.Fatalf("code=%d body=%s", response.Code, response.Body.String())
	}
	var consent string
	if err := db.DB.QueryRow(`SELECT whatsapp_consent FROM customers WHERE id=10`).Scan(&consent); err != nil {
		t.Fatal(err)
	}
	if consent != "opted_out" {
		t.Fatalf("consent=%q: Meta reported an opt-out and the daemon would keep sending", consent)
	}
}

func TestExternalFailureReasonIsScrubbedOfPhoneNumbers(t *testing.T) {
	server, db := testHTTPServer(t)
	defer db.Close()
	server.Config.PIIHashKey = "hash-key-value-for-tests"
	now := time.Now().UTC().Format(time.RFC3339Nano)
	phone := "919876500011"
	_, _ = db.DB.Exec(`INSERT INTO customers(id,shopify_id,phone_ciphertext,phone_hash,whatsapp_consent,created_at,updated_at) VALUES(11,'gid://shopify/Customer/11',x'01',?,'opted_in',?,?)`,
		security.KeyedHash(server.Config.PIIHashKey, phone), now, now)
	recordExternal(t, server, `{"phone":"`+phone+`","template":"t","language":"en_US",
		"idempotency_key":"key-eleven","status":"failed","failure_code":"131030",
		"failure_reason":"Recipient phone number not in allowed list: +919876500011"}`)
	var reason string
	if err := db.DB.QueryRow(`SELECT failure_reason FROM outbound_messages WHERE idempotency_key='key-eleven'`).Scan(&reason); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(reason, "919876500011") {
		t.Fatalf("failure_reason stored a plaintext phone number: %q", reason)
	}
}

func TestExternalMessageRecordingRejectsIncompleteRecords(t *testing.T) {
	server, db := testHTTPServer(t)
	defer db.Close()
	server.Config.PIIHashKey = "hash-key-value-for-tests"
	for name, body := range map[string]string{
		"missing phone":       `{"template":"t","language":"en_US","idempotency_key":"k","status":"accepted","meta_message_id":"w"}`,
		"missing template":    `{"phone":"919876500004","language":"en_US","idempotency_key":"k","status":"accepted","meta_message_id":"w"}`,
		"missing key":         `{"phone":"919876500004","template":"t","language":"en_US","status":"accepted","meta_message_id":"w"}`,
		"bad status":          `{"phone":"919876500004","template":"t","language":"en_US","idempotency_key":"k","status":"queued"}`,
		"accepted without id": `{"phone":"919876500004","template":"t","language":"en_US","idempotency_key":"k","status":"accepted"}`,
	} {
		if code := recordExternal(t, server, body).Code; code != 400 {
			t.Fatalf("%s: code=%d, want 400", name, code)
		}
	}
}

func TestExternalMessageRecordingRequiresAuthentication(t *testing.T) {
	server, db := testHTTPServer(t)
	defer db.Close()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/messages/record", bytes.NewReader([]byte(`{}`)))
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("code=%d", response.Code)
	}
}

func TestRatesWithoutADenominatorRenderAsUnmeasured(t *testing.T) {
	if got := ratio(0, 0); got != "—" {
		t.Fatalf("ratio with no denominator = %q, want an em dash", got)
	}
	if got := ratio(0.25, 4); got != "25.0%" {
		t.Fatalf("ratio with a denominator = %q", got)
	}
}

func TestCustomerImportQueuesSyncJobsWithoutTouchingOrders(t *testing.T) {
	server, db := testHTTPServer(t)
	defer db.Close()
	body := `{"shopify_ids":["gid://shopify/Customer/1","gid://shopify/Customer/2","gid://shopify/Customer/1"]}`
	request := httptest.NewRequest(http.MethodPost, "/api/v1/customers/import", bytes.NewReader([]byte(body)))
	request.Header.Set("Authorization", "Bearer api")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != 202 {
		t.Fatalf("code=%d body=%s", response.Code, response.Body.String())
	}
	var result map[string]any
	_ = json.Unmarshal(response.Body.Bytes(), &result)
	if result["queued"] != float64(2) || result["already_queued"] != float64(1) {
		t.Fatalf("result=%v", result)
	}
	var kinds string
	if err := db.DB.QueryRow(`SELECT group_concat(DISTINCT kind) FROM scheduled_jobs`).Scan(&kinds); err != nil {
		t.Fatal(err)
	}
	if kinds != "shopify_sync_customer" {
		t.Fatalf("import queued more than customer syncs: %s", kinds)
	}
}

func TestCustomerImportRejectsAnythingButACustomerGID(t *testing.T) {
	server, db := testHTTPServer(t)
	defer db.Close()
	for _, body := range []string{
		`{"shopify_ids":[]}`,
		`{"shopify_ids":["gid://shopify/Order/1"]}`,
		`{"shopify_ids":["919876543210"]}`,
	} {
		request := httptest.NewRequest(http.MethodPost, "/api/v1/customers/import", bytes.NewReader([]byte(body)))
		request.Header.Set("Authorization", "Bearer api")
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		if response.Code != 400 {
			t.Fatalf("body=%s code=%d, want 400", body, response.Code)
		}
	}
}

func reserveExternal(t *testing.T, server *HTTPServer, body string) map[string]any {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/messages/reserve", bytes.NewReader([]byte(body)))
	request.Header.Set("Authorization", "Bearer api")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != 200 {
		t.Fatalf("code=%d body=%s", response.Code, response.Body.String())
	}
	var parsed map[string]any
	_ = json.Unmarshal(response.Body.Bytes(), &parsed)
	return parsed
}

func TestReservationRefusesARecipientItCannotAccountFor(t *testing.T) {
	server, db := testHTTPServer(t)
	defer db.Close()
	server.Config.PIIHashKey = "hash-key-value-for-tests"
	answer := reserveExternal(t, server, `{"phone":"919876500020","template":"t","language":"en_US","idempotency_key":"r1"}`)
	if answer["reserved"] != false || answer["reason"] != "unknown_recipient" {
		t.Fatalf("answer=%v", answer)
	}
	var messages int
	_ = db.DB.QueryRow(`SELECT count(*) FROM outbound_messages`).Scan(&messages)
	if messages != 0 {
		t.Fatalf("a refused reservation still wrote a row: %d", messages)
	}
}

func TestReservationRecordsTheMessageBeforeItIsSent(t *testing.T) {
	server, db := testHTTPServer(t)
	defer db.Close()
	server.Config.PIIHashKey = "hash-key-value-for-tests"
	now := time.Now().UTC().Format(time.RFC3339Nano)
	phone := "919876500021"
	_, _ = db.DB.Exec(`INSERT INTO customers(id,shopify_id,phone_ciphertext,phone_hash,whatsapp_consent,created_at,updated_at) VALUES(21,'gid://shopify/Customer/21',x'01',?,'opted_in',?,?)`,
		security.KeyedHash(server.Config.PIIHashKey, phone), now, now)

	answer := reserveExternal(t, server, `{"phone":"`+phone+`","template":"stay_upgrade","language":"en_US","idempotency_key":"r2","attempted_at":"`+now+`"}`)
	if answer["reserved"] != true {
		t.Fatalf("answer=%v", answer)
	}
	var state, template string
	var attempted string
	if err := db.DB.QueryRow(`SELECT state,template_name,attempted_at FROM outbound_messages WHERE idempotency_key='r2'`).Scan(&state, &template, &attempted); err != nil {
		t.Fatal(err)
	}
	if state != "queued" || template != "stay_upgrade" || attempted != now {
		t.Fatalf("state=%s template=%s attempted=%s", state, template, attempted)
	}

	// A second reservation must refuse. The first attempt has not reported an
	// outcome, so Meta may already have accepted it; re-sending could spend
	// money twice and deliver a duplicate.
	repeat := reserveExternal(t, server, `{"phone":"`+phone+`","template":"stay_upgrade","language":"en_US","idempotency_key":"r2"}`)
	if repeat["reserved"] != false || repeat["reason"] != "unreconciled" {
		t.Fatalf("repeat=%v", repeat)
	}

	// Reporting the outcome settles the reserved row rather than adding one.
	recordExternal(t, server, `{"phone":"`+phone+`","template":"stay_upgrade","language":"en_US",
		"idempotency_key":"r2","meta_message_id":"wamid.R2","status":"accepted","attempted_at":"`+now+`"}`)
	var settled, metaID string
	var count int
	_ = db.DB.QueryRow(`SELECT count(*) FROM outbound_messages`).Scan(&count)
	if err := db.DB.QueryRow(`SELECT state,meta_message_id FROM outbound_messages WHERE idempotency_key='r2'`).Scan(&settled, &metaID); err != nil {
		t.Fatal(err)
	}
	if count != 1 || settled != "accepted" || metaID != "wamid.R2" {
		t.Fatalf("count=%d state=%s meta=%s", count, settled, metaID)
	}
}

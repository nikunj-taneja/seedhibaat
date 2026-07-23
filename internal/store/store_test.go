package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMigrationIntegrityAndWAL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.IntegrityCheck(context.Background()); err != nil {
		t.Fatal(err)
	}
	var mode string
	if err := s.DB.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "wal" {
		t.Fatalf("journal mode=%s", mode)
	}
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal("idempotent migrate:", err)
	}
	var campaignColumns int
	if err := s.DB.QueryRow(`SELECT count(*) FROM pragma_table_info('campaigns') WHERE name IN ('tracked_url','frequency_messages','frequency_window')`).Scan(&campaignColumns); err != nil || campaignColumns != 3 {
		t.Fatalf("campaign migration columns=%d err=%v", campaignColumns, err)
	}
	var returnColumn int
	if err := s.DB.QueryRow(`SELECT count(*) FROM pragma_table_info('orders') WHERE name='return_recorded_at'`).Scan(&returnColumn); err != nil || returnColumn != 1 {
		t.Fatalf("order return migration column=%d err=%v", returnColumn, err)
	}
}

func TestMetricsCanBeFilteredByCampaign(t *testing.T) {
	s, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now().UTC()
	stamp := now.Format(time.RFC3339Nano)
	_, _ = s.DB.Exec(`INSERT INTO customers(id,shopify_id,created_at,updated_at) VALUES(1,'customer',?,?)`, stamp, stamp)
	for _, campaign := range []string{"one", "two"} {
		_, err = s.DB.Exec(`INSERT INTO outbound_messages(id,customer_id,campaign_id,template_name,template_language,category,idempotency_key,state,attempted_at,accepted_at,created_at,updated_at) VALUES(?,?,?,?,?,'MARKETING',?,'accepted',?,?,?,?)`, "message-"+campaign, 1, campaign, "template", "en_US", "key-"+campaign, stamp, stamp, stamp, stamp)
		if err != nil {
			t.Fatal(err)
		}
	}
	metrics, err := s.MetricsFiltered(context.Background(), now.Add(-time.Hour), now.Add(time.Hour), MetricsFilter{CampaignID: "one"})
	if err != nil {
		t.Fatal(err)
	}
	if metrics.Attempted != 1 || metrics.Accepted != 1 {
		t.Fatalf("metrics=%+v", metrics)
	}
}

func TestMetricsUseUniqueRecipientsAndSeparateCurrencies(t *testing.T) {
	s, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now().UTC()
	stamp := now.Format(time.RFC3339Nano)
	for id := 1; id <= 2; id++ {
		_, _ = s.DB.Exec(`INSERT INTO customers(id,shopify_id,created_at,updated_at) VALUES(?,?,?,?)`, id, fmt.Sprintf("customer-%d", id), stamp, stamp)
		_, _ = s.DB.Exec(`INSERT INTO outbound_messages(id,customer_id,template_name,template_language,category,idempotency_key,state,accepted_at,delivered_at,created_at,updated_at) VALUES(?,?,?,?,? ,?,'delivered',?,?,?,?)`, fmt.Sprintf("message-%d", id), id, "template", "en_US", "MARKETING", fmt.Sprintf("key-%d", id), stamp, stamp, stamp, stamp)
	}
	for id := 1; id <= 2; id++ {
		orderID := fmt.Sprintf("order-%d", id)
		_, _ = s.DB.Exec(`INSERT INTO orders(shopify_id,customer_id,currency,total_amount_minor,created_at,updated_at) VALUES(?,1,'INR',10000,?,?)`, orderID, stamp, stamp)
		_, _ = s.DB.Exec(`INSERT INTO conversions(order_id,message_id,attributed_at,amount_minor,currency,attribution_model) VALUES(?,'message-1',?,10000,'INR','last_touch')`, orderID, stamp)
	}
	metrics, err := s.Metrics(context.Background(), now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if metrics.Conversions != 2 || metrics.ConvertedRecipients != 1 || metrics.ConversionRate != 0.5 {
		t.Fatalf("unexpected conversion metrics: %+v", metrics)
	}
	if len(metrics.RevenueByCurrency) != 1 || metrics.RevenueByCurrency[0].Currency != "INR" || metrics.RevenueByCurrency[0].AmountMinor != 20000 {
		t.Fatalf("unexpected currency revenue: %+v", metrics.RevenueByCurrency)
	}
}

func TestWebhookAndJobDeduplication(t *testing.T) {
	s, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	e := WebhookEvent{Provider: "shopify", EventID: "event-1", Topic: "orders/updated", Payload: []byte("{}")}
	inserted, err := s.RecordWebhook(context.Background(), e, time.Now())
	if err != nil || !inserted {
		t.Fatalf("first webhook %v %v", inserted, err)
	}
	inserted, err = s.RecordWebhook(context.Background(), e, time.Now())
	if err != nil || inserted {
		t.Fatalf("duplicate webhook %v %v", inserted, err)
	}
	job := Job{ID: "job-1", WorkflowRunID: sql.NullString{}, StepID: "one", Kind: "test", Payload: []byte("{}")}
	inserted, err = s.EnqueueJob(context.Background(), job, "same", time.Now())
	if err != nil || !inserted {
		t.Fatal(inserted, err)
	}
	job.ID = "job-2"
	inserted, err = s.EnqueueJob(context.Background(), job, "same", time.Now())
	if err != nil || inserted {
		t.Fatal(inserted, err)
	}
}

func TestRestartRecoversStaleJob(t *testing.T) {
	s, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_, _ = s.EnqueueJob(context.Background(), Job{ID: "job", StepID: "one", Kind: "test", Payload: []byte("{}")}, "key", time.Now())
	claimed, ok, err := s.ClaimJob(context.Background(), "worker", time.Now())
	if err != nil || !ok || claimed.ID != "job" {
		t.Fatal(claimed, ok, err)
	}
	_, _ = s.DB.Exec(`UPDATE scheduled_jobs SET locked_at=? WHERE id='job'`, time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano))
	count, err := s.RecoverStaleJobs(context.Background(), time.Now().Add(-10*time.Minute))
	if err != nil || count != 1 {
		t.Fatal(count, err)
	}
	claimed, ok, err = s.ClaimJob(context.Background(), "worker2", time.Now().Add(time.Second))
	if err != nil || !ok {
		t.Fatal(claimed, ok, err)
	}
}

func TestRetentionScrubsPayloadsRepliesAndDormantCustomerPII(t *testing.T) {
	s, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	old := time.Now().Add(-90 * 24 * time.Hour).UTC().Format(time.RFC3339Nano)
	if _, err := s.DB.Exec(`INSERT INTO customers(id,shopify_id,phone_ciphertext,phone_hash,first_name_ciphertext,last_name_ciphertext,created_at,updated_at) VALUES(1,'gid://shopify/Customer/1',x'01','hash',x'02',x'03',?,?)`, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`INSERT INTO replies(provider_message_id,customer_id,received_at,message_type,body_ciphertext,body_hash) VALUES('reply',1,?,'text',x'04','body-hash')`, old); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`INSERT INTO webhook_events(provider,event_id,topic,received_at,available_at,status,payload) VALUES('meta','event','messages',?,?,'processed',x'05')`, old, old); err != nil {
		t.Fatal(err)
	}
	result, err := s.ApplyRetention(context.Background(), time.Now().Add(-30*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if result.RepliesScrubbed != 1 || result.WebhookPayloadsScrubbed != 1 || result.CustomersScrubbed != 1 {
		t.Fatalf("unexpected retention result: %+v", result)
	}
	var phone, reply []byte
	var payload []byte
	if err := s.DB.QueryRow(`SELECT phone_ciphertext FROM customers WHERE id=1`).Scan(&phone); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.QueryRow(`SELECT body_ciphertext FROM replies WHERE provider_message_id='reply'`).Scan(&reply); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.QueryRow(`SELECT payload FROM webhook_events WHERE provider='meta' AND event_id='event'`).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if phone != nil || reply != nil || len(payload) != 0 {
		t.Fatalf("retained sensitive data: phone=%v reply=%v payload=%v", phone, reply, payload)
	}
}

func TestCancelWorkflowAndReplayFailedJob(t *testing.T) {
	s, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err = s.DB.Exec(`INSERT INTO customers(id,shopify_id,created_at,updated_at) VALUES(1,'customer',?,?)`, now, now)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.DB.Exec(`INSERT INTO workflow_runs(id,workflow_name,workflow_version,customer_id,trigger_type,trigger_id,state,started_at) VALUES('run','flow',1,1,'order_delivered','order','active',?)`, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnqueueJob(context.Background(), Job{ID: "job", WorkflowRunID: sql.NullString{String: "run", Valid: true}, StepID: "one", Kind: "test", Payload: []byte(`{}`)}, "workflow:key", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s.CancelWorkflow(context.Background(), "run", "test cancellation"); err != nil {
		t.Fatal(err)
	}
	var runState, jobState string
	if err := s.DB.QueryRow(`SELECT state FROM workflow_runs WHERE id='run'`).Scan(&runState); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.QueryRow(`SELECT state FROM scheduled_jobs WHERE id='job'`).Scan(&jobState); err != nil {
		t.Fatal(err)
	}
	if runState != "cancelled" || jobState != "cancelled" {
		t.Fatalf("states run=%s job=%s", runState, jobState)
	}
	_, _ = s.DB.Exec(`UPDATE scheduled_jobs SET state='failed',workflow_run_id=NULL WHERE id='job'`)
	if err := s.ReplayFailedJob(context.Background(), "job", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.QueryRow(`SELECT state FROM scheduled_jobs WHERE id='job'`).Scan(&jobState); err != nil || jobState != "scheduled" {
		t.Fatalf("replayed state=%s err=%v", jobState, err)
	}
}

func TestCancelCustomerWorkAlsoCancelsCampaignJobs(t *testing.T) {
	s, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, _ = s.DB.Exec(`INSERT INTO customers(id,shopify_id,created_at,updated_at) VALUES(1,'customer',?,?)`, now, now)
	_, _ = s.EnqueueJob(context.Background(), Job{ID: "campaign-job", StepID: "campaign", Kind: "send_whatsapp", Payload: []byte(`{"customer_id":1,"campaign_id":"campaign"}`)}, "campaign:key", time.Now())
	if err := s.CancelCustomerWork(context.Background(), 1, "refund"); err != nil {
		t.Fatal(err)
	}
	var state string
	_ = s.DB.QueryRow(`SELECT state FROM scheduled_jobs WHERE id='campaign-job'`).Scan(&state)
	if state != "cancelled" {
		t.Fatalf("state=%s", state)
	}
}

func TestReplayBlocksUnknownAcceptance(t *testing.T) {
	s, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, _ = s.DB.Exec(`INSERT INTO customers(id,shopify_id,created_at,updated_at) VALUES(1,'customer',?,?)`, now, now)
	_, _ = s.EnqueueJob(context.Background(), Job{ID: "job", StepID: "one", Kind: "send_whatsapp", Payload: []byte(`{}`)}, "message:key", time.Now())
	_, _ = s.DB.Exec(`UPDATE scheduled_jobs SET state='failed' WHERE id='job'`)
	_, _ = s.DB.Exec(`INSERT INTO outbound_messages(id,job_id,customer_id,template_name,template_language,category,idempotency_key,state,attempted_at,created_at,updated_at) VALUES('message','job',1,'template','en_US','MARKETING','message:key','unknown',?,?,?)`, now, now, now)
	if err := s.ReplayFailedJob(context.Background(), "job", time.Now()); err == nil {
		t.Fatal("unknown provider acceptance was replayed")
	}
}

func TestRetryBackoffStopsAtMaximumAttempts(t *testing.T) {
	s, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	start := time.Now()
	_, _ = s.EnqueueJob(context.Background(), Job{ID: "job", StepID: "step", Kind: "test", Payload: []byte(`{}`), MaxAttempts: 2}, "retry-key", start)
	job, ok, err := s.ClaimJob(context.Background(), "worker", start.Add(time.Second))
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	if err := s.FailJob(context.Background(), job, errors.New("temporary"), start.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	var state string
	var available string
	_ = s.DB.QueryRow(`SELECT state,available_at FROM scheduled_jobs WHERE id='job'`).Scan(&state, &available)
	if state != "retry" || available <= start.UTC().Format(time.RFC3339Nano) {
		t.Fatalf("state=%s available=%s", state, available)
	}
	job, ok, err = s.ClaimJob(context.Background(), "worker", start.Add(time.Hour))
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	if err := s.FailJob(context.Background(), job, errors.New("still temporary"), start.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	_ = s.DB.QueryRow(`SELECT state FROM scheduled_jobs WHERE id='job'`).Scan(&state)
	if state != "failed" {
		t.Fatalf("state=%s", state)
	}
}

func TestSyncWatermarkRoundTrip(t *testing.T) {
	s, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, exists, err := s.SyncWatermark(context.Background(), "shopify", "catalog"); err != nil || exists {
		t.Fatalf("exists=%v err=%v", exists, err)
	}
	want := time.Date(2026, 7, 22, 12, 0, 0, 123, time.UTC)
	if err := s.SetSyncWatermark(context.Background(), "shopify", "catalog", want); err != nil {
		t.Fatal(err)
	}
	got, exists, err := s.SyncWatermark(context.Background(), "shopify", "catalog")
	if err != nil || !exists || !got.Equal(want) {
		t.Fatalf("got=%s exists=%v err=%v", got, exists, err)
	}
}

func TestPersistedJobErrorsAreRedacted(t *testing.T) {
	s, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	start := time.Now()
	_, _ = s.EnqueueJob(context.Background(), Job{ID: "job", StepID: "step", Kind: "test", Payload: []byte(`{}`)}, "error-key", start)
	job, ok, err := s.ClaimJob(context.Background(), "worker", start.Add(time.Second))
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	prefix := "shp" + "ss_"
	if err := s.FailJob(context.Background(), job, fmt.Errorf("phone +91 99999 99999 secret %sabcdefghijklmnopqrstuvwxyz", prefix), start.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	var persisted string
	_ = s.DB.QueryRow(`SELECT last_error FROM scheduled_jobs WHERE id='job'`).Scan(&persisted)
	if strings.Contains(persisted, "99999") || strings.Contains(persisted, prefix) {
		t.Fatalf("unsafe persisted error=%q", persisted)
	}
}

func TestDeferRunningJobSurvivesQuietHours(t *testing.T) {
	s, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now()
	_, _ = s.EnqueueJob(context.Background(), Job{ID: "quiet-job", StepID: "step", Kind: "test", Payload: []byte(`{}`)}, "quiet-key", now)
	if _, ok, err := s.ClaimJob(context.Background(), "worker", now.Add(time.Second)); err != nil || !ok {
		t.Fatal(ok, err)
	}
	until := now.Add(8 * time.Hour)
	if err := s.DeferJob(context.Background(), "quiet-job", until, "quiet hours"); err != nil {
		t.Fatal(err)
	}
	var state, available string
	var locked sql.NullString
	if err := s.DB.QueryRow(`SELECT state,available_at,locked_by FROM scheduled_jobs WHERE id='quiet-job'`).Scan(&state, &available, &locked); err != nil {
		t.Fatal(err)
	}
	if state != "scheduled" || available != until.UTC().Format(time.RFC3339Nano) || locked.Valid {
		t.Fatalf("state=%s available=%s locked=%v", state, available, locked)
	}
}

func TestPauseAndResumeRequireValidRunState(t *testing.T) {
	s, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.PauseWorkflow(context.Background(), "missing", "test"); err == nil {
		t.Fatal("missing workflow run paused")
	}
	if err := s.ResumeWorkflow(context.Background(), "missing"); err == nil {
		t.Fatal("missing workflow run resumed")
	}
}

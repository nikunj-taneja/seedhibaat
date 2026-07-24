package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nikunj-taneja/seedhibaat/internal/config"
	"github.com/nikunj-taneja/seedhibaat/internal/security"
	"github.com/nikunj-taneja/seedhibaat/internal/shopify"
	"github.com/nikunj-taneja/seedhibaat/internal/store"
	"github.com/nikunj-taneja/seedhibaat/internal/workflow"
)

func testProcessor(t *testing.T) (*Processor, *store.Store) {
	t.Helper()
	database, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	processor := &Processor{Config: config.Config{PIIHashKey: "pii-key", ProductionFlowEnabled: true}, Store: database, Logger: logger}
	return processor, database
}

func deliveredExampleOrder(id, customerID string) shopify.Order {
	delivered := "2026-07-20T10:00:00Z"
	order := shopify.Order{ID: id, Name: "#100", ProcessedAt: "2026-07-18T10:00:00Z", UpdatedAt: "2026-07-20T10:00:00Z", DisplayFinancialStatus: "PAID", CurrencyCode: "INR", Customer: &shopify.Customer{ID: customerID, FirstName: "Test", Phone: "919999999999", Tags: []string{"whatsapp-opt-in"}}}
	order.CurrentTotalPriceSet.ShopMoney.Amount = "999.00"
	line := shopify.LineItem{ID: id + "-line", Title: "Starter Product", Quantity: 1, CurrentQuantity: 1, Product: &shopify.Product{ID: "p-starter", Title: "Starter Product", Handle: "starter-product", Tags: []string{"starter-product"}}, Variant: &shopify.Variant{ID: "v-starter", InventoryQuantity: 10}}
	order.LineItems.Nodes = []shopify.LineItem{line}
	fulfillment := shopify.Fulfillment{ID: id + "-f", Status: "SUCCESS", DeliveredAt: &delivered, UpdatedAt: delivered}
	fulfilledLine := shopify.FulfillmentLine{Quantity: 1}
	fulfilledLine.LineItem.ID = line.ID
	fulfillment.FulfillmentLineItems.Nodes = []shopify.FulfillmentLine{fulfilledLine}
	order.Fulfillments = []shopify.Fulfillment{fulfillment}
	return order
}

func TestDeliveredWorkflowIsDurableAndIdempotent(t *testing.T) {
	processor, database := testProcessor(t)
	defer database.Close()
	body, err := os.ReadFile("../../config/workflows/post_delivery_followup_example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	_, err = database.DB.Exec(`INSERT INTO workflow_definitions(name,version,definition_hash,yaml,active,created_at) VALUES('post_delivery_followup_example',1,'hash',?,1,?)`, string(body), time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		t.Fatal(err)
	}
	order := deliveredExampleOrder("o-1", "c-1")
	if err := processor.upsertOrder(context.Background(), order); err != nil {
		t.Fatal(err)
	}
	if err := processor.upsertOrder(context.Background(), order); err != nil {
		t.Fatal(err)
	}
	var runs, jobs int
	_ = database.DB.QueryRow(`SELECT count(*) FROM workflow_runs`).Scan(&runs)
	_ = database.DB.QueryRow(`SELECT count(*) FROM scheduled_jobs`).Scan(&jobs)
	if runs != 1 || jobs != 4 {
		t.Fatalf("runs=%d jobs=%d", runs, jobs)
	}
}

func TestPartialDeliveryDoesNotStartWorkflow(t *testing.T) {
	processor, database := testProcessor(t)
	defer database.Close()
	body, _ := os.ReadFile("../../config/workflows/post_delivery_followup_example.yaml")
	_, _ = database.DB.Exec(`INSERT INTO workflow_definitions(name,version,definition_hash,yaml,active,created_at) VALUES('post_delivery_followup_example',1,'hash',?,1,?)`, string(body), time.Now().UTC().Format(time.RFC3339Nano))
	order := deliveredExampleOrder("o-2", "c-2")
	second := order.LineItems.Nodes[0]
	second.ID = "second"
	second.Quantity = 1
	second.CurrentQuantity = 1
	order.LineItems.Nodes = append(order.LineItems.Nodes, second)
	if err := processor.upsertOrder(context.Background(), order); err != nil {
		t.Fatal(err)
	}
	var runs int
	_ = database.DB.QueryRow(`SELECT count(*) FROM workflow_runs`).Scan(&runs)
	if runs != 0 {
		t.Fatalf("runs=%d", runs)
	}
}

func TestReturnedDeliveredOrderDoesNotStartWorkflow(t *testing.T) {
	processor, database := testProcessor(t)
	defer database.Close()
	body, _ := os.ReadFile("../../config/workflows/post_delivery_followup_example.yaml")
	_, _ = database.DB.Exec(`INSERT INTO workflow_definitions(name,version,definition_hash,yaml,active,created_at) VALUES('post_delivery_followup_example',1,'hash',?,1,?)`, string(body), time.Now().UTC().Format(time.RFC3339Nano))
	order := deliveredExampleOrder("returned-order", "returned-customer")
	order.Returns.Nodes = []shopify.Return{{Status: "CLOSED"}}
	if err := processor.upsertOrder(context.Background(), order); err != nil {
		t.Fatal(err)
	}
	var runs int
	_ = database.DB.QueryRow(`SELECT count(*) FROM workflow_runs`).Scan(&runs)
	if runs != 0 {
		t.Fatalf("returned order started workflow runs=%d", runs)
	}
	var recorded sql.NullString
	if err := database.DB.QueryRow(`SELECT return_recorded_at FROM orders WHERE shopify_id=?`, order.ID).Scan(&recorded); err != nil || !recorded.Valid {
		t.Fatalf("return persisted=%v err=%v", recorded, err)
	}
}

func TestOutOfOrderStatusesAndOptOut(t *testing.T) {
	processor, database := testProcessor(t)
	defer database.Close()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	phone := "919999999999"
	encrypted, _ := security.Encrypt("pii-key", []byte(phone))
	_, _ = database.DB.Exec(`INSERT INTO customers(id,shopify_id,phone_ciphertext,phone_hash,whatsapp_consent,created_at,updated_at) VALUES(1,'c',?,?, 'opted_in',?,?)`, encrypted, security.KeyedHash("pii-key", phone), now, now)
	_, _ = database.DB.Exec(`INSERT INTO outbound_messages(id,customer_id,template_name,template_language,category,idempotency_key,meta_message_id,state,created_at,updated_at) VALUES('m',1,'t','en_US','MARKETING','key','wamid.1','accepted',?,?)`, now, now)
	read := `{"object":"whatsapp_business_account","entry":[{"id":"w","changes":[{"field":"messages","value":{"statuses":[{"id":"wamid.1","status":"read","timestamp":"1784600000","recipient_id":"919999999999"}]}}]}]}`
	delivered := strings.Replace(read, `"read"`, `"delivered"`, 1)
	if err := processor.processMetaWebhook(context.Background(), []byte(read)); err != nil {
		t.Fatal(err)
	}
	if err := processor.processMetaWebhook(context.Background(), []byte(delivered)); err != nil {
		t.Fatal(err)
	}
	var deliveredAt, readAt, state string
	if err := database.DB.QueryRow(`SELECT delivered_at,read_at,state FROM outbound_messages WHERE id='m'`).Scan(&deliveredAt, &readAt, &state); err != nil {
		t.Fatal(err)
	}
	if state != "read" {
		t.Fatalf("state regressed to %s", state)
	}
	optout := `{"object":"whatsapp_business_account","entry":[{"id":"w","changes":[{"field":"messages","value":{"messages":[{"from":"919999999999","id":"in.1","timestamp":"1784600100","type":"text","text":{"body":"STOP"}}]}}]}]}`
	if err := processor.processMetaWebhook(context.Background(), []byte(optout)); err != nil {
		t.Fatal(err)
	}
	var consent string
	if err := database.DB.QueryRow(`SELECT whatsapp_consent FROM customers WHERE id=1`).Scan(&consent); err != nil {
		t.Fatal(err)
	}
	if consent != "opted_out" {
		t.Fatalf("consent=%s", consent)
	}
	secondOptout := strings.Replace(optout, `"id":"in.1"`, `"id":"in.2"`, 1)
	if err := processor.processMetaWebhook(context.Background(), []byte(secondOptout)); err != nil {
		t.Fatal(err)
	}
	var optoutEvents int
	_ = database.DB.QueryRow(`SELECT count(*) FROM audit_log WHERE action='customer.opt_out'`).Scan(&optoutEvents)
	if optoutEvents != 1 {
		t.Fatalf("repeated STOP opt-out events=%d", optoutEvents)
	}
}

func TestMetaInvalidRecipientSuppressesCustomerAndCancelsWork(t *testing.T) {
	processor, database := testProcessor(t)
	defer database.Close()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, _ = database.DB.Exec(`INSERT INTO customers(id,shopify_id,whatsapp_consent,created_at,updated_at) VALUES(1,'c','opted_in',?,?)`, now, now)
	_, _ = database.DB.Exec(`INSERT INTO workflow_runs(id,workflow_name,workflow_version,customer_id,trigger_type,trigger_id,state,started_at) VALUES('run','flow',1,1,'order_delivered','order','active',?)`, now)
	_, _ = database.DB.Exec(`INSERT INTO scheduled_jobs(id,workflow_run_id,step_id,idempotency_key,kind,payload,scheduled_at,available_at,state,created_at,updated_at) VALUES('job','run','step','key','send_whatsapp',x'7b7d',?,?, 'scheduled',?,?)`, now, now, now, now)
	_, _ = database.DB.Exec(`INSERT INTO scheduled_jobs(id,step_id,idempotency_key,kind,payload,scheduled_at,available_at,state,created_at,updated_at) VALUES('campaign-job','campaign','campaign-key','send_whatsapp',?, ?,?, 'scheduled',?,?)`, []byte(`{"customer_id":1,"campaign_id":"campaign"}`), now, now, now, now)
	_, _ = database.DB.Exec(`INSERT INTO outbound_messages(id,job_id,customer_id,workflow_run_id,template_name,template_language,category,idempotency_key,meta_message_id,state,created_at,updated_at) VALUES('message','job',1,'run','template','en_US','MARKETING','message-key','wamid.invalid','accepted',?,?)`, now, now)
	payload := `{"object":"whatsapp_business_account","entry":[{"id":"w","changes":[{"field":"messages","value":{"statuses":[{"id":"wamid.invalid","status":"failed","timestamp":"1784600000","errors":[{"code":131026,"title":"Undeliverable","message":"recipient unavailable"}]}]}}]}]}`
	if err := processor.processMetaWebhook(context.Background(), []byte(payload)); err != nil {
		t.Fatal(err)
	}
	var invalid int
	var runState, jobState, campaignJobState string
	_ = database.DB.QueryRow(`SELECT invalid_number FROM customers WHERE id=1`).Scan(&invalid)
	_ = database.DB.QueryRow(`SELECT state FROM workflow_runs WHERE id='run'`).Scan(&runState)
	_ = database.DB.QueryRow(`SELECT state FROM scheduled_jobs WHERE id='job'`).Scan(&jobState)
	_ = database.DB.QueryRow(`SELECT state FROM scheduled_jobs WHERE id='campaign-job'`).Scan(&campaignJobState)
	if invalid != 1 || runState != "cancelled" || jobState != "cancelled" || campaignJobState != "cancelled" {
		t.Fatalf("invalid=%d run=%s job=%s campaign_job=%s", invalid, runState, jobState, campaignJobState)
	}
}

func TestMetaWebhookForDifferentPhoneNumberIsIgnored(t *testing.T) {
	processor, database := testProcessor(t)
	defer database.Close()
	processor.Config.MetaPhoneNumberID = "configured-phone"
	now := time.Now().UTC().Format(time.RFC3339Nano)
	phone := "919999999999"
	_, _ = database.DB.Exec(`INSERT INTO customers(id,shopify_id,phone_hash,whatsapp_consent,created_at,updated_at) VALUES(1,'c',?,'opted_in',?,?)`, security.KeyedHash("pii-key", phone), now, now)
	payload := `{"object":"whatsapp_business_account","entry":[{"id":"w","changes":[{"field":"messages","value":{"metadata":{"phone_number_id":"another-phone"},"messages":[{"from":"919999999999","id":"in.other","timestamp":"1784600100","type":"text","text":{"body":"STOP"}}]}}]}]}`
	if err := processor.processMetaWebhook(context.Background(), []byte(payload)); err != nil {
		t.Fatal(err)
	}
	var consent string
	_ = database.DB.QueryRow(`SELECT whatsapp_consent FROM customers WHERE id=1`).Scan(&consent)
	if consent != "opted_in" {
		t.Fatalf("cross-phone webhook changed consent to %s", consent)
	}
}

func TestKnownMetaStatusSurvivesPhoneProfileSwitch(t *testing.T) {
	processor, database := testProcessor(t)
	defer database.Close()
	processor.Config.MetaPhoneNumberID = "production-phone"
	processor.Config.MetaTestPhoneNumberID = "test-phone"
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, _ = database.DB.Exec(`INSERT INTO customers(id,shopify_id,whatsapp_consent,created_at,updated_at) VALUES(1,'c','opted_in',?,?)`, now, now)
	_, _ = database.DB.Exec(`INSERT INTO outbound_messages(id,customer_id,template_name,template_language,category,idempotency_key,meta_message_id,state,created_at,updated_at) VALUES('m',1,'t','en_US','MARKETING','key','wamid.test','delivered',?,?)`, now, now)
	payload := `{"object":"whatsapp_business_account","entry":[{"id":"w","changes":[{"field":"messages","value":{"metadata":{"phone_number_id":"test-phone"},"statuses":[{"id":"wamid.test","status":"read","timestamp":"1784600000","recipient_id":"919999999999"}]}}]}]}`
	if err := processor.processMetaWebhook(context.Background(), []byte(payload)); err != nil {
		t.Fatal(err)
	}
	var readAt sql.NullString
	var state string
	if err := database.DB.QueryRow(`SELECT read_at,state FROM outbound_messages WHERE id='m'`).Scan(&readAt, &state); err != nil {
		t.Fatal(err)
	}
	if !readAt.Valid || state != "read" {
		t.Fatalf("read_at=%v state=%q", readAt, state)
	}
}

func TestInboundMetaWebhookAcceptsConfiguredTestPhone(t *testing.T) {
	processor, database := testProcessor(t)
	defer database.Close()
	processor.Config.MetaPhoneNumberID = "production-phone"
	processor.Config.MetaTestPhoneNumberID = "test-phone"
	now := time.Now().UTC().Format(time.RFC3339Nano)
	phone := "919999999999"
	_, _ = database.DB.Exec(`INSERT INTO customers(id,shopify_id,phone_hash,whatsapp_consent,created_at,updated_at) VALUES(1,'c',?,'opted_in',?,?)`, security.KeyedHash("pii-key", phone), now, now)
	payload := `{"object":"whatsapp_business_account","entry":[{"id":"w","changes":[{"field":"messages","value":{"metadata":{"phone_number_id":"test-phone"},"messages":[{"from":"919999999999","id":"in.test","timestamp":"1784600100","type":"text","text":{"body":"STOP"}}]}}]}]}`
	if err := processor.processMetaWebhook(context.Background(), []byte(payload)); err != nil {
		t.Fatal(err)
	}
	var consent string
	if err := database.DB.QueryRow(`SELECT whatsapp_consent FROM customers WHERE id=1`).Scan(&consent); err != nil {
		t.Fatal(err)
	}
	if consent != "opted_out" {
		t.Fatalf("consent=%q", consent)
	}
}

func TestShopifyCustomerWebhookQueuesDirectReconciliation(t *testing.T) {
	processor, database := testProcessor(t)
	defer database.Close()
	event := store.WebhookEvent{Provider: "shopify", EventID: "customer-event", Topic: "customers_whats_app_marketing_consent/update", Payload: []byte(`{"customer_id":123,"whats_app_marketing_consent":{"state":"subscribed"}}`)}
	if err := processor.processShopifyWebhook(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	var kind, payload string
	if err := database.DB.QueryRow(`SELECT kind,payload FROM scheduled_jobs`).Scan(&kind, &payload); err != nil {
		t.Fatal(err)
	}
	if kind != "shopify_sync_customer" || !strings.Contains(payload, "gid://shopify/Customer/123") {
		t.Fatalf("kind=%q payload=%q", kind, payload)
	}
}

func TestShopifyWhatsAppConsentIsAppliedAndStaleUpdateIgnored(t *testing.T) {
	processor, database := testProcessor(t)
	defer database.Close()
	decode := func(state, updated string) shopify.Customer {
		var customer shopify.Customer
		body := fmt.Sprintf(`{"id":"gid://shopify/Customer/1","firstName":"Test","defaultPhoneNumber":{"phoneNumber":"+919999999999","whatsAppMarketingConsent":{"state":%q,"updatedAt":%q}}}`, state, updated)
		if err := json.Unmarshal([]byte(body), &customer); err != nil {
			t.Fatal(err)
		}
		return customer
	}
	if err := processor.upsertCustomer(context.Background(), decode("SUBSCRIBED", "2026-07-22T12:00:00Z")); err != nil {
		t.Fatal(err)
	}
	if err := processor.upsertCustomer(context.Background(), decode("UNSUBSCRIBED", "2026-07-21T12:00:00Z")); err != nil {
		t.Fatal(err)
	}
	var consent string
	var suppressed sql.NullString
	if err := database.DB.QueryRow(`SELECT whatsapp_consent,suppressed_at FROM customers WHERE shopify_id='gid://shopify/Customer/1'`).Scan(&consent, &suppressed); err != nil {
		t.Fatal(err)
	}
	if consent != "opted_in" || suppressed.Valid {
		t.Fatalf("consent=%q suppressed=%v", consent, suppressed)
	}
}

func TestNativePendingConsentCannotFallBackToOptInTag(t *testing.T) {
	processor, database := testProcessor(t)
	defer database.Close()
	var customer shopify.Customer
	body := `{"id":"gid://shopify/Customer/2","firstName":"Test","tags":["whatsapp-opt-in"],"defaultPhoneNumber":{"phoneNumber":"+919999999999","whatsAppMarketingConsent":{"state":"PENDING","updatedAt":"2026-07-22T12:00:00Z"}}}`
	if err := json.Unmarshal([]byte(body), &customer); err != nil {
		t.Fatal(err)
	}
	if err := processor.upsertCustomer(context.Background(), customer); err != nil {
		t.Fatal(err)
	}
	var consent string
	if err := database.DB.QueryRow(`SELECT whatsapp_consent FROM customers WHERE shopify_id='gid://shopify/Customer/2'`).Scan(&consent); err != nil {
		t.Fatal(err)
	}
	if consent != "not_opted_in" {
		t.Fatalf("native pending consent became %q", consent)
	}
}

func TestShopifyOptOutTransitionIsAuditedExactlyOnce(t *testing.T) {
	processor, database := testProcessor(t)
	defer database.Close()
	decode := func(state, updated string) shopify.Customer {
		var customer shopify.Customer
		body := fmt.Sprintf(`{"id":"gid://shopify/Customer/3","defaultPhoneNumber":{"phoneNumber":"+919999999999","whatsAppMarketingConsent":{"state":%q,"updatedAt":%q}}}`, state, updated)
		if err := json.Unmarshal([]byte(body), &customer); err != nil {
			t.Fatal(err)
		}
		return customer
	}
	if err := processor.upsertCustomer(context.Background(), decode("SUBSCRIBED", "2026-07-20T12:00:00Z")); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := processor.upsertCustomer(context.Background(), decode("UNSUBSCRIBED", "2026-07-22T12:00:00Z")); err != nil {
			t.Fatal(err)
		}
	}
	var events int
	_ = database.DB.QueryRow(`SELECT count(*) FROM audit_log WHERE action='customer.opt_out'`).Scan(&events)
	if events != 1 {
		t.Fatalf("Shopify opt-out events=%d", events)
	}
}

func TestWorkflowTemplateParametersRenderInMetaOrder(t *testing.T) {
	processor, database := testProcessor(t)
	defer database.Close()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	first, _ := security.Encrypt("pii-key", []byte("Asha"))
	last, _ := security.Encrypt("pii-key", []byte("Test"))
	_, _ = database.DB.Exec(`INSERT INTO customers(id,shopify_id,created_at,updated_at) VALUES(1,'customer',?,?)`, now, now)
	_, _ = database.DB.Exec(`INSERT INTO orders(shopify_id,customer_id,order_number,created_at,updated_at) VALUES('order',1,'#123',?,?)`, now, now)
	_, _ = database.DB.Exec(`INSERT INTO order_lines(shopify_id,order_id,title,quantity,current_quantity) VALUES('line','order','Starter Product',1,1)`)
	_, _ = database.DB.Exec(`INSERT INTO workflow_runs(id,workflow_name,workflow_version,customer_id,trigger_type,trigger_id,state,started_at) VALUES('run','flow',1,1,'order_delivered','order','active',?)`, now)
	payload := sendPayload{RunID: "run", Params: map[string]string{"header.1": "literal:Order update", "body.1": "customer.first_name", "body.2": "order.number", "body.3": "order.first_product_title"}}
	components, fingerprint, err := processor.renderTemplateParameters(context.Background(), payload, first, last)
	if err != nil {
		t.Fatal(err)
	}
	if len(components) != 2 || components[0].Type != "header" || components[1].Type != "body" {
		t.Fatalf("components=%+v", components)
	}
	got := components[1].Parameters
	if len(got) != 3 || got[0].Text != "Asha" || got[1].Text != "#123" || got[2].Text != "Starter Product" || fingerprint == "" {
		t.Fatalf("body=%+v fingerprint_empty=%v", got, fingerprint == "")
	}
}

func TestWorkflowImageHeaderRendersBeforeBodyParameters(t *testing.T) {
	processor, database := testProcessor(t)
	defer database.Close()
	payload := sendPayload{
		HeaderImageURL: "https://cdn.example.com/header.webp",
		Params:         map[string]string{"body.1": "literal:750"},
	}
	components, fingerprint, err := processor.renderTemplateParameters(context.Background(), payload, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(components) != 2 || components[0].Type != "header" || components[1].Type != "body" {
		t.Fatalf("components=%+v", components)
	}
	image := components[0].Parameters[0].Image
	if image == nil || image.Link != payload.HeaderImageURL || components[0].Parameters[0].Type != "image" {
		t.Fatalf("image header=%+v", components[0])
	}
	if components[1].Parameters[0].Text != "750" || fingerprint == "" {
		t.Fatalf("body=%+v fingerprint_empty=%v", components[1], fingerprint == "")
	}
}

func TestStepConditionStopsCustomerWhoAlreadyConverted(t *testing.T) {
	processor, database := testProcessor(t)
	defer database.Close()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, _ = database.DB.Exec(`INSERT INTO customers(id,shopify_id,created_at,updated_at) VALUES(1,'customer',?,?)`, now, now)
	_, _ = database.DB.Exec(`INSERT INTO products(shopify_id,title,handle,tags_json,updated_at) VALUES('product','Upgraded Product','upgraded-product','["upgraded"]',?)`, now)
	_, _ = database.DB.Exec(`INSERT INTO orders(shopify_id,customer_id,order_number,created_at,updated_at) VALUES('conversion',1,'#200',?,?)`, now, now)
	_, _ = database.DB.Exec(`INSERT INTO order_lines(shopify_id,order_id,product_id,title,quantity,current_quantity) VALUES('line','conversion','product','Upgraded Product',1,1)`)
	payload := sendPayload{CustomerID: 1, Conditions: workflow.Conditions{CustomerHasNotPurchased: &workflow.ProductCondition{ProductTags: []string{"upgraded"}}}}
	eligible, reason, err := processor.evaluateStepConditions(context.Background(), payload)
	if err != nil {
		t.Fatal(err)
	}
	if eligible || reason == "" {
		t.Fatalf("eligible=%v reason=%q", eligible, reason)
	}
}

func TestProviderErrorsAreRedactedBeforeLogging(t *testing.T) {
	prefix := "shp" + "ss_"
	value := safeLogError(fmt.Errorf("recipient +91 99999 99999 token %sabcdefghijklmnopqrstuvwxyz failed", prefix))
	if strings.Contains(value, "99999") || strings.Contains(value, prefix) {
		t.Fatalf("unsafe log value=%q", value)
	}
}

func TestPurchaseAttributionUsesPreOrderMessageAndIsRemovedOnRefund(t *testing.T) {
	processor, database := testProcessor(t)
	defer database.Close()
	processor.Config.AttributionWindow = 7 * 24 * time.Hour
	now := time.Now().UTC()
	stamp := now.Format(time.RFC3339Nano)
	_, _ = database.DB.Exec(`INSERT INTO customers(id,shopify_id,created_at,updated_at) VALUES(1,'customer',?,?)`, stamp, stamp)
	accepted := now.Add(-time.Hour).Format(time.RFC3339Nano)
	_, _ = database.DB.Exec(`INSERT INTO outbound_messages(id,customer_id,campaign_id,template_name,template_language,category,idempotency_key,state,attempted_at,accepted_at,created_at,updated_at) VALUES('message',1,'campaign','template','en_US','MARKETING','key','accepted',?,?,?,?)`, accepted, accepted, accepted, accepted)
	order := deliveredExampleOrder("attributed-order", "customer")
	order.ProcessedAt = now.Format(time.RFC3339)
	order.UpdatedAt = order.ProcessedAt
	if err := processor.upsertOrder(context.Background(), order); err != nil {
		t.Fatal(err)
	}
	var campaign string
	if err := database.DB.QueryRow(`SELECT campaign_id FROM conversions WHERE order_id=?`, order.ID).Scan(&campaign); err != nil {
		t.Fatal(err)
	}
	if campaign != "campaign" {
		t.Fatalf("campaign=%q", campaign)
	}
	order.Refunds = []shopify.Refund{{CreatedAt: now.Add(time.Minute).Format(time.RFC3339)}}
	if err := processor.upsertOrder(context.Background(), order); err != nil {
		t.Fatal(err)
	}
	var conversions int
	_ = database.DB.QueryRow(`SELECT count(*) FROM conversions WHERE order_id=?`, order.ID).Scan(&conversions)
	if conversions != 0 {
		t.Fatalf("refunded conversion count=%d", conversions)
	}
}

func TestBackInStockTransitionStartsDurableWorkflow(t *testing.T) {
	processor, database := testProcessor(t)
	defer database.Close()
	body, _ := os.ReadFile("../../config/workflows/back_in_stock_example.yaml")
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, _ = database.DB.Exec(`INSERT INTO workflow_definitions(name,version,definition_hash,yaml,active,created_at) VALUES('back_in_stock_example',1,'hash',?,1,?)`, string(body), now)
	_, _ = database.DB.Exec(`INSERT INTO customers(id,shopify_id,whatsapp_consent,created_at,updated_at) VALUES(1,'customer','opted_in',?,?)`, now, now)
	_, _ = database.DB.Exec(`INSERT INTO products(shopify_id,title,handle,tags_json,updated_at) VALUES('product','Example Product','example-product','[]',?)`, now)
	_, _ = database.DB.Exec(`INSERT INTO variants(shopify_id,product_id,title,inventory_item_id,inventory_quantity,updated_at) VALUES('gid://shopify/ProductVariant/1','product','Default','gid://shopify/InventoryItem/1',0,?)`, now)
	_, _ = database.DB.Exec(`INSERT INTO orders(shopify_id,customer_id,created_at,updated_at) VALUES('order',1,?,?)`, now, now)
	_, _ = database.DB.Exec(`INSERT INTO order_lines(shopify_id,order_id,product_id,title,quantity,current_quantity) VALUES('line','order','product','Example Product',1,1)`)
	var item shopify.InventoryItem
	fixture := `{"id":"gid://shopify/InventoryItem/1","variant":{"id":"gid://shopify/ProductVariant/1","title":"Default","inventoryQuantity":5,"product":{"id":"product","title":"Example Product","handle":"example-product","status":"ACTIVE","tags":[]}},"inventoryLevels":{"nodes":[{"id":"level","updatedAt":"2026-07-22T12:00:00Z","location":{"id":"location"},"quantities":[{"name":"available","quantity":5}]}]}}`
	if err := json.Unmarshal([]byte(fixture), &item); err != nil {
		t.Fatal(err)
	}
	if err := processor.upsertInventory(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	var runs, jobs int
	_ = database.DB.QueryRow(`SELECT count(*) FROM workflow_runs WHERE trigger_type='inventory_back_in_stock'`).Scan(&runs)
	_ = database.DB.QueryRow(`SELECT count(*) FROM scheduled_jobs WHERE kind='send_whatsapp'`).Scan(&jobs)
	if runs != 1 || jobs != 1 {
		t.Fatalf("runs=%d jobs=%d", runs, jobs)
	}
}

func TestReconciliationUsesHistoricalThenOverlappingWatermark(t *testing.T) {
	processor, database := testProcessor(t)
	defer database.Close()
	processor.Config.InitialSyncLookback = 2 * 365 * 24 * time.Hour
	processor.Config.ReconcileOverlap = 24 * time.Hour
	now := time.Date(2026, 7, 22, 12, 30, 0, 0, time.UTC)
	if err := processor.enqueueReconciliation(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	var initial []byte
	if err := database.DB.QueryRow(`SELECT payload FROM scheduled_jobs`).Scan(&initial); err != nil {
		t.Fatal(err)
	}
	var first map[string]string
	if err := json.Unmarshal(initial, &first); err != nil {
		t.Fatal(err)
	}
	if first["mode"] != "initial" || first["since"] != now.Add(-2*365*24*time.Hour).Format(time.RFC3339Nano) {
		t.Fatalf("initial payload=%v", first)
	}
	if _, err := database.DB.Exec(`DELETE FROM scheduled_jobs`); err != nil {
		t.Fatal(err)
	}
	watermark := now.Add(-6 * time.Hour)
	if err := database.SetSyncWatermark(context.Background(), "shopify", "catalog", watermark); err != nil {
		t.Fatal(err)
	}
	later := now.Add(2 * time.Hour)
	if err := processor.enqueueReconciliation(context.Background(), later); err != nil {
		t.Fatal(err)
	}
	var incremental []byte
	if err := database.DB.QueryRow(`SELECT payload FROM scheduled_jobs`).Scan(&incremental); err != nil {
		t.Fatal(err)
	}
	var second map[string]string
	if err := json.Unmarshal(incremental, &second); err != nil {
		t.Fatal(err)
	}
	if second["mode"] != "incremental" || second["since"] != watermark.Add(-24*time.Hour).Format(time.RFC3339Nano) {
		t.Fatalf("incremental payload=%v", second)
	}
}

func TestWorkflowDefinitionsAreImmutableByVersion(t *testing.T) {
	_, database := testProcessor(t)
	defer database.Close()
	body, _ := os.ReadFile("../../config/workflows/post_delivery_followup_example.yaml")
	loaded, err := workflow.Parse("test", body)
	if err != nil {
		t.Fatal(err)
	}
	if err := persistWorkflowDefinition(context.Background(), database.DB, loaded); err != nil {
		t.Fatal(err)
	}
	changed := loaded
	changed.Hash = "different"
	if err := persistWorkflowDefinition(context.Background(), database.DB, changed); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("same-version edit accepted: %v", err)
	}
}

func TestShopifyCustomerPrivacyRedactionDeletesAssociatedData(t *testing.T) {
	processor, database := testProcessor(t)
	defer database.Close()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, _ = database.DB.Exec(`INSERT INTO customers(id,shopify_id,whatsapp_consent,created_at,updated_at) VALUES(1,'gid://shopify/Customer/123','opted_in',?,?)`, now, now)
	_, _ = database.DB.Exec(`INSERT INTO orders(shopify_id,customer_id,created_at,updated_at) VALUES('gid://shopify/Order/456',1,?,?)`, now, now)
	_, _ = database.DB.Exec(`INSERT INTO workflow_runs(id,workflow_name,workflow_version,customer_id,trigger_type,trigger_id,state,started_at) VALUES('run','flow',1,1,'order','order','active',?)`, now)
	_, _ = database.DB.Exec(`INSERT INTO scheduled_jobs(id,workflow_run_id,step_id,idempotency_key,kind,payload,scheduled_at,available_at,state,created_at,updated_at) VALUES('job','run','step','key','send_whatsapp',x'7b7d',?,?, 'scheduled',?,?)`, now, now, now, now)
	event := store.WebhookEvent{Topic: "customers/redact", Payload: []byte(`{"shop_domain":"example.myshopify.com","customer":{"id":123},"orders_to_redact":[456]}`)}
	if err := processor.processShopifyWebhook(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"customers", "orders", "workflow_runs", "scheduled_jobs"} {
		var count int
		if err := database.DB.QueryRow(`SELECT count(*) FROM ` + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("table=%s count=%d err=%v", table, count, err)
		}
	}
	var objectID string
	if err := database.DB.QueryRow(`SELECT object_id FROM audit_log WHERE action='shopify.customer_redacted'`).Scan(&objectID); err != nil || objectID == "gid://shopify/Customer/123" {
		t.Fatalf("unsafe audit object=%q err=%v", objectID, err)
	}
}

func TestShopifyShopRedactionPurgesDataAndDeactivatesWorkflows(t *testing.T) {
	processor, database := testProcessor(t)
	defer database.Close()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, _ = database.DB.Exec(`INSERT INTO customers(id,shopify_id,created_at,updated_at) VALUES(1,'customer',?,?)`, now, now)
	_, _ = database.DB.Exec(`INSERT INTO products(shopify_id,title,updated_at) VALUES('product','Product',?)`, now)
	_, _ = database.DB.Exec(`INSERT INTO workflow_definitions(name,version,definition_hash,yaml,active,created_at) VALUES('flow',1,'hash','name: flow',1,?)`, now)
	event := store.WebhookEvent{Topic: "shop/redact", Payload: []byte(`{"shop_domain":"example.myshopify.com"}`)}
	if err := processor.processShopifyWebhook(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	var customers, products, active int
	_ = database.DB.QueryRow(`SELECT count(*) FROM customers`).Scan(&customers)
	_ = database.DB.QueryRow(`SELECT count(*) FROM products`).Scan(&products)
	_ = database.DB.QueryRow(`SELECT count(*) FROM workflow_definitions WHERE active=1`).Scan(&active)
	if customers != 0 || products != 0 || active != 0 {
		t.Fatalf("customers=%d products=%d active=%d", customers, products, active)
	}
}

func TestShopifyAppUninstallCancelsJobsAndDeactivatesWorkflows(t *testing.T) {
	processor, database := testProcessor(t)
	defer database.Close()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, _ = database.DB.Exec(`INSERT INTO customers(id,shopify_id,created_at,updated_at) VALUES(1,'customer',?,?)`, now, now)
	_, _ = database.DB.Exec(`INSERT INTO workflow_definitions(name,version,definition_hash,yaml,active,created_at) VALUES('flow',1,'hash','name: flow',1,?)`, now)
	_, _ = database.DB.Exec(`INSERT INTO workflow_runs(id,workflow_name,workflow_version,customer_id,trigger_type,trigger_id,state,started_at) VALUES('run','flow',1,1,'order','order','active',?)`, now)
	_, _ = database.DB.Exec(`INSERT INTO scheduled_jobs(id,workflow_run_id,step_id,idempotency_key,kind,payload,scheduled_at,available_at,state,created_at,updated_at) VALUES('job','run','step','key','send_whatsapp',x'7b7d',?,?, 'scheduled',?,?)`, now, now, now, now)
	if err := processor.processShopifyWebhook(context.Background(), store.WebhookEvent{Topic: "app/uninstalled", Payload: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	var active int
	var runState, jobState string
	_ = database.DB.QueryRow(`SELECT count(*) FROM workflow_definitions WHERE active=1`).Scan(&active)
	_ = database.DB.QueryRow(`SELECT state FROM workflow_runs WHERE id='run'`).Scan(&runState)
	_ = database.DB.QueryRow(`SELECT state FROM scheduled_jobs WHERE id='job'`).Scan(&jobState)
	if active != 0 || runState != "cancelled" || jobState != "cancelled" {
		t.Fatalf("active=%d run=%s job=%s", active, runState, jobState)
	}
}

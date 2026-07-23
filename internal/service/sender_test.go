package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nikunj-taneja/seedhibaat/internal/config"
	"github.com/nikunj-taneja/seedhibaat/internal/meta"
	"github.com/nikunj-taneja/seedhibaat/internal/security"
	"github.com/nikunj-taneja/seedhibaat/internal/store"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func senderFixture(t *testing.T, client *meta.Client) (*Processor, *store.Store, store.Job) {
	t.Helper()
	database, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	phone, _ := security.Encrypt("pii-key", []byte("919999999999"))
	first, _ := security.Encrypt("pii-key", []byte("Test"))
	last, _ := security.Encrypt("pii-key", []byte("Customer"))
	_, _ = database.DB.Exec(`INSERT INTO customers(id,shopify_id,phone_ciphertext,first_name_ciphertext,last_name_ciphertext,whatsapp_consent,created_at,updated_at) VALUES(1,'customer',?,?,?,'opted_in',?,?)`, phone, first, last, now, now)
	payload, _ := json.Marshal(sendPayload{CustomerID: 1, Template: "template", Language: "en_US", Category: "MARKETING", FrequencyMessages: 4, FrequencyWindow: "24h"})
	job := store.Job{ID: "job", StepID: "step", Kind: "send_whatsapp", Payload: payload}
	if _, err := database.EnqueueJob(context.Background(), job, "send-key", time.Now()); err != nil {
		t.Fatal(err)
	}
	processor := &Processor{Config: config.Config{PIIHashKey: "pii-key", LinkSigningKey: "link-key", OutboundSendingEnabled: true}, Store: database, Meta: client, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	return processor, database, job
}

func TestPermanentMetaFailureIsRecordedAndInvalidRecipientSuppressed(t *testing.T) {
	client := meta.NewClient("v1", "token", "waba", "phone")
	client.HTTP = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusBadRequest, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":{"message":"recipient is unavailable","code":131026}}`)), Request: request}, nil
	})}
	processor, database, job := senderFixture(t, client)
	defer database.Close()
	if err := processor.sendWhatsApp(context.Background(), job); err == nil {
		t.Fatal("permanent Meta rejection returned no error")
	}
	var state, code string
	var invalid int
	_ = database.DB.QueryRow(`SELECT state,failure_code FROM outbound_messages`).Scan(&state, &code)
	_ = database.DB.QueryRow(`SELECT invalid_number FROM customers WHERE id=1`).Scan(&invalid)
	if state != "failed" || code != "131026" || invalid != 1 {
		t.Fatalf("state=%s code=%s invalid=%d", state, code, invalid)
	}
}

func TestNetworkAmbiguityIsRecordedUnknownAndNeverAutomaticallyRetried(t *testing.T) {
	client := meta.NewClient("v1", "token", "waba", "phone")
	client.HTTP = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("connection reset after write")
	})}
	processor, database, job := senderFixture(t, client)
	defer database.Close()
	err := processor.sendWhatsApp(context.Background(), job)
	var permanent *noRetryError
	if !errors.As(err, &permanent) {
		t.Fatalf("error=%v", err)
	}
	var state string
	_ = database.DB.QueryRow(`SELECT state FROM outbound_messages`).Scan(&state)
	if state != "unknown" {
		t.Fatalf("state=%s", state)
	}
}

func TestFrequencyCapReservationIsAtomicAcrossConcurrentJobs(t *testing.T) {
	var calls atomic.Int64
	client := meta.NewClient("v1", "token", "waba", "phone")
	client.HTTP = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		id := calls.Add(1)
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(fmt.Sprintf(`{"messages":[{"id":"wamid.%d"}]}`, id))), Request: request}, nil
	})}
	processor, database, first := senderFixture(t, client)
	defer database.Close()
	var payload sendPayload
	if err := json.Unmarshal(first.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	payload.FrequencyMessages = 1
	first.Payload, _ = json.Marshal(payload)
	_, _ = database.DB.Exec(`UPDATE scheduled_jobs SET payload=? WHERE id='job'`, first.Payload)
	secondPayload, _ := json.Marshal(payload)
	second := store.Job{ID: "job-2", StepID: "step", Kind: "send_whatsapp", Payload: secondPayload}
	if _, err := database.EnqueueJob(context.Background(), second, "send-key-2", time.Now()); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errorsSeen := make(chan error, 2)
	for _, job := range []store.Job{first, second} {
		wg.Add(1)
		go func(job store.Job) {
			defer wg.Done()
			errorsSeen <- processor.sendWhatsApp(context.Background(), job)
		}(job)
	}
	wg.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Fatal(err)
		}
	}
	var messages, suppressed int
	_ = database.DB.QueryRow(`SELECT count(*) FROM outbound_messages`).Scan(&messages)
	_ = database.DB.QueryRow(`SELECT count(*) FROM scheduled_jobs WHERE state='suppressed'`).Scan(&suppressed)
	if messages != 1 || suppressed != 1 || calls.Load() != 1 {
		t.Fatalf("messages=%d suppressed=%d calls=%d", messages, suppressed, calls.Load())
	}
}

func TestCancelledCampaignIsRecheckedImmediatelyBeforeSend(t *testing.T) {
	var calls atomic.Int64
	client := meta.NewClient("v1", "token", "waba", "phone")
	client.HTTP = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"messages":[{"id":"wamid.1"}]}`)), Request: request}, nil
	})}
	processor, database, job := senderFixture(t, client)
	defer database.Close()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, _ = database.DB.Exec(`INSERT INTO campaigns(id,name,segment_json,exclusions_json,template_name,template_language,state,created_at) VALUES('cancelled','test','{}','{}','template','en_US','cancelled',?)`, now)
	var payload sendPayload
	_ = json.Unmarshal(job.Payload, &payload)
	payload.CampaignID = "cancelled"
	job.Payload, _ = json.Marshal(payload)
	_, _ = database.DB.Exec(`UPDATE scheduled_jobs SET payload=? WHERE id=?`, job.Payload, job.ID)
	if err := processor.sendWhatsApp(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	var state string
	_ = database.DB.QueryRow(`SELECT state FROM scheduled_jobs WHERE id=?`, job.ID).Scan(&state)
	if state != "suppressed" || calls.Load() != 0 {
		t.Fatalf("state=%s provider_calls=%d", state, calls.Load())
	}
}

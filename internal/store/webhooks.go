package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

type WebhookEvent struct {
	Provider string
	EventID  string
	Topic    string
	Payload  []byte
	Attempts int
}

func (s *Store) RecordWebhook(ctx context.Context, event WebhookEvent, receivedAt time.Time) (bool, error) {
	stamp := receivedAt.UTC().Format(time.RFC3339Nano)
	result, err := s.DB.ExecContext(ctx, `INSERT INTO webhook_events(provider,event_id,topic,received_at,available_at,payload) VALUES(?,?,?,?,?,?) ON CONFLICT(provider,event_id) DO NOTHING`, event.Provider, event.EventID, event.Topic, stamp, stamp, event.Payload)
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	return count == 1, err
}

func (s *Store) ClaimWebhook(ctx context.Context) (WebhookEvent, bool, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return WebhookEvent{}, false, err
	}
	defer tx.Rollback()
	var event WebhookEvent
	err = tx.QueryRowContext(ctx, `SELECT provider,event_id,topic,payload,attempts FROM webhook_events WHERE status IN ('pending','retry') AND available_at<=? ORDER BY available_at,received_at LIMIT 1`, time.Now().UTC().Format(time.RFC3339Nano)).Scan(&event.Provider, &event.EventID, &event.Topic, &event.Payload, &event.Attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return WebhookEvent{}, false, nil
	}
	if err != nil {
		return WebhookEvent{}, false, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE webhook_events SET status='processing',attempts=attempts+1 WHERE provider=? AND event_id=? AND status IN ('pending','retry')`, event.Provider, event.EventID)
	if err != nil {
		return WebhookEvent{}, false, err
	}
	count, err := result.RowsAffected()
	if err != nil || count != 1 {
		return WebhookEvent{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return WebhookEvent{}, false, err
	}
	event.Attempts++
	return event, true, nil
}

func (s *Store) FinishWebhook(ctx context.Context, event WebhookEvent, processingErr error) error {
	if processingErr == nil {
		_, err := s.DB.ExecContext(ctx, `UPDATE webhook_events SET status='processed',error=NULL WHERE provider=? AND event_id=?`, event.Provider, event.EventID)
		return err
	}
	status := "retry"
	if event.Attempts >= 8 {
		status = "failed"
	}
	backoff := time.Duration(1<<min(event.Attempts, 10)) * time.Second
	_, err := s.DB.ExecContext(ctx, `UPDATE webhook_events SET status=?,available_at=?,error=? WHERE provider=? AND event_id=?`, status, time.Now().Add(backoff).UTC().Format(time.RFC3339Nano), truncateError(processingErr), event.Provider, event.EventID)
	return err
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

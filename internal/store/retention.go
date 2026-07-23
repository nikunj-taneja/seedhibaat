package store

import (
	"context"
	"time"
)

type RetentionResult struct {
	RepliesScrubbed         int64
	WebhookPayloadsScrubbed int64
	LinksDeleted            int64
	CustomersScrubbed       int64
}

func (s *Store) ApplyRetention(ctx context.Context, cutoff time.Time) (RetentionResult, error) {
	stamp := cutoff.UTC().Format(time.RFC3339Nano)
	var result RetentionResult
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	query := func(statement string, args ...any) (int64, error) {
		outcome, err := tx.ExecContext(ctx, statement, args...)
		if err != nil {
			return 0, err
		}
		return outcome.RowsAffected()
	}
	if result.RepliesScrubbed, err = query(`UPDATE replies SET body_ciphertext=NULL WHERE received_at<? AND body_ciphertext IS NOT NULL`, stamp); err != nil {
		return result, err
	}
	if result.WebhookPayloadsScrubbed, err = query(`UPDATE webhook_events SET payload=x'' WHERE received_at<? AND status='processed' AND length(payload)>0`, stamp); err != nil {
		return result, err
	}
	if result.LinksDeleted, err = query(`DELETE FROM tracked_links WHERE expires_at<?`, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return result, err
	}
	if result.CustomersScrubbed, err = query(`UPDATE customers SET phone_ciphertext=NULL,first_name_ciphertext=NULL,last_name_ciphertext=NULL WHERE updated_at<? AND NOT EXISTS(SELECT 1 FROM orders o WHERE o.customer_id=customers.id AND o.updated_at>=?) AND NOT EXISTS(SELECT 1 FROM outbound_messages m WHERE m.customer_id=customers.id AND m.created_at>=?) AND NOT EXISTS(SELECT 1 FROM workflow_runs w WHERE w.customer_id=customers.id AND w.state IN ('active','paused'))`, stamp, stamp, stamp); err != nil {
		return result, err
	}
	if err := tx.Commit(); err != nil {
		return result, err
	}
	_, _ = s.DB.ExecContext(ctx, `PRAGMA optimize`)
	return result, nil
}

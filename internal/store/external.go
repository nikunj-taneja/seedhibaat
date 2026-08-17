package store

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"time"
)

// ExternalMessage is a send performed outside the daemon's queue — today only
// the operator CLI — that still has to land in outbound_messages so status
// webhooks, reporting, and revenue attribution can find it.
type ExternalMessage struct {
	PhoneHash            string
	Template             string
	Language             string
	Category             string
	IdempotencyKey       string
	ParameterFingerprint string
	MetaMessageID        string
	Status               string
	FailureCode          string
	FailureReason        string
	AttemptedAt          string
}

type ExternalMessageResult struct {
	MessageID  string
	CustomerID int64
	Created    bool
	Upgraded   bool
	// Unresolved reports a recipient with no customer row. The recorder must
	// never invent one: customers.phone_hash is UNIQUE while the Shopify
	// upsert infers ON CONFLICT(shopify_id) only, so a phone-only row makes
	// SQLite abort that customer's next sync and lose their orders.
	Unresolved bool
}

// RecordExternalMessage is idempotent on IdempotencyKey so the CLI can replay
// its whole ledger safely. A send that failed before Meta accepted it and was
// then repaired and resent is upgraded in place, so the successful attempt's
// meta_message_id is kept and its status webhooks still resolve.
func (s *Store) RecordExternalMessage(ctx context.Context, message ExternalMessage) (ExternalMessageResult, error) {
	var result ExternalMessageResult
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()

	var existingID, existingState string
	var existingMetaID sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT id,state,meta_message_id FROM outbound_messages WHERE idempotency_key=?`, message.IdempotencyKey).Scan(&existingID, &existingState, &existingMetaID)
	if err == nil {
		result.MessageID = existingID
		if existingState == "failed" && !existingMetaID.Valid && message.Status == "accepted" {
			if _, err := tx.ExecContext(ctx, `UPDATE outbound_messages SET state='accepted',meta_message_id=?,accepted_at=?,failed_at=NULL,failure_code=NULL,failure_reason=NULL,updated_at=? WHERE id=?`,
				message.MetaMessageID, message.AttemptedAt, time.Now().UTC().Format(time.RFC3339Nano), existingID); err != nil {
				return result, err
			}
			result.Upgraded = true
		}
		return result, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return result, err
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	var customerID int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM customers WHERE phone_hash=?`, message.PhoneHash).Scan(&customerID)
	if errors.Is(err, sql.ErrNoRows) {
		result.Unresolved = true
		return result, tx.Commit()
	} else if err != nil {
		return result, err
	}
	result.CustomerID = customerID

	state := "accepted"
	var accepted, failed any
	if message.Status == "failed" {
		state = "failed"
		failed = message.AttemptedAt
	} else {
		accepted = message.AttemptedAt
	}
	messageID := NewID("msg")
	// created_at carries the real send time, not the recording time: every
	// reporting window keys on it, so a replayed ledger must land on the day
	// the message actually went out.
	insert, err := tx.ExecContext(ctx, `INSERT INTO outbound_messages
		(id,customer_id,template_name,template_language,category,parameter_fingerprint,idempotency_key,meta_message_id,state,source,attempted_at,accepted_at,failed_at,failure_code,failure_reason,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,'cli',?,?,?,?,?,?,?) ON CONFLICT(idempotency_key) DO NOTHING`,
		messageID, customerID, message.Template, message.Language, message.Category,
		nullableText(message.ParameterFingerprint), message.IdempotencyKey, nullableText(message.MetaMessageID),
		state, message.AttemptedAt, accepted, failed,
		nullableText(message.FailureCode), nullableText(message.FailureReason), message.AttemptedAt, now)
	if err != nil {
		return result, err
	}
	// A concurrent recorder may have won the race; report it as already
	// recorded rather than as an error.
	if written, err := insert.RowsAffected(); err != nil {
		return result, err
	} else if written == 0 {
		if err := tx.QueryRowContext(ctx, `SELECT id FROM outbound_messages WHERE idempotency_key=?`, message.IdempotencyKey).Scan(&result.MessageID); err != nil {
			return result, err
		}
		return result, tx.Commit()
	}
	result.MessageID = messageID
	result.Created = true
	return result, tx.Commit()
}

// SuppressForMetaFailure applies the consent and suppression consequences of a
// Meta delivery rejection: a recipient Meta reports as opted out or
// undeliverable must stop receiving from every path, not only the one that
// discovered it.
//
// It has two callers today because the Python CLI still sends directly to Meta
// alongside the daemon. That is temporary — the CLI is being reduced to a thin
// client over the daemon — and until then the rule cannot live in the sender.
func (s *Store) SuppressForMetaFailure(ctx context.Context, customerID int64, code int) (bool, error) {
	if code != 131026 && code != 131050 {
		return false, nil
	}
	consent, reason, invalid := "opted_in", "Meta reported an undeliverable recipient", 1
	if code == 131050 {
		consent, reason, invalid = "opted_out", "Meta marketing opt-out", 0
	}
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	query := `UPDATE customers SET whatsapp_consent=?,invalid_number=CASE WHEN ?=1 THEN 1 ELSE invalid_number END,suppressed_at=coalesce(suppressed_at,?),suppression_reason=?,updated_at=? WHERE id=?`
	if code == 131050 {
		query += ` AND whatsapp_consent<>'opted_out'`
	}
	result, err := s.DB.ExecContext(ctx, query, consent, invalid, stamp, reason, stamp, customerID)
	if err != nil {
		return false, err
	}
	changed, _ := result.RowsAffected()
	if code == 131050 && changed == 1 {
		if err := s.Audit(ctx, "meta", "customer.opt_out", "customer", strconv.FormatInt(customerID, 10), `{"source":"meta_error_131050"}`); err != nil {
			return false, err
		}
	}
	if err := s.CancelCustomerWork(ctx, customerID, reason); err != nil {
		return false, err
	}
	return changed == 1, nil
}

func nullableText(value string) any {
	if value == "" {
		return nil
	}
	return value
}

package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// ExternalMessage is a send performed outside the daemon's queue — today only
// the operator CLI — that still has to land in outbound_messages so status
// webhooks, reporting, and revenue attribution can find it.
type ExternalMessage struct {
	PhoneHash            string
	PhoneCiphertext      []byte
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
	MessageID       string
	Created         bool
	CustomerCreated bool
}

// RecordExternalMessage is idempotent on IdempotencyKey so the CLI can replay
// its whole ledger safely. encryptPhone is called only when the recipient is
// unknown, to avoid encrypting on the common path.
func (s *Store) RecordExternalMessage(ctx context.Context, message ExternalMessage, encryptPhone func() ([]byte, error)) (ExternalMessageResult, error) {
	var result ExternalMessageResult
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()

	var existingID string
	err = tx.QueryRowContext(ctx, `SELECT id FROM outbound_messages WHERE idempotency_key=?`, message.IdempotencyKey).Scan(&existingID)
	if err == nil {
		result.MessageID = existingID
		return result, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return result, err
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	var customerID int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM customers WHERE phone_hash=?`, message.PhoneHash).Scan(&customerID)
	if errors.Is(err, sql.ErrNoRows) {
		// A recipient with no customer row still has to be traceable, so the
		// row is created with unknown consent rather than dropping the send.
		ciphertext := message.PhoneCiphertext
		if ciphertext == nil && encryptPhone != nil {
			if ciphertext, err = encryptPhone(); err != nil {
				return result, err
			}
		}
		insert, err := tx.ExecContext(ctx, `INSERT INTO customers(phone_ciphertext,phone_hash,whatsapp_consent,created_at,updated_at) VALUES(?,?,'unknown',?,?)`, ciphertext, message.PhoneHash, now, now)
		if err != nil {
			return result, err
		}
		if customerID, err = insert.LastInsertId(); err != nil {
			return result, err
		}
		result.CustomerCreated = true
	} else if err != nil {
		return result, err
	}

	state := "accepted"
	var accepted, failed any
	if message.Status == "failed" {
		state = "failed"
		failed = message.AttemptedAt
	} else {
		accepted = message.AttemptedAt
	}
	messageID := NewID("msg")
	_, err = tx.ExecContext(ctx, `INSERT INTO outbound_messages
		(id,customer_id,template_name,template_language,category,parameter_fingerprint,idempotency_key,meta_message_id,state,source,attempted_at,accepted_at,failed_at,failure_code,failure_reason,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,'cli',?,?,?,?,?,?,?)`,
		messageID, customerID, message.Template, message.Language, message.Category,
		nullableText(message.ParameterFingerprint), message.IdempotencyKey, nullableText(message.MetaMessageID),
		state, message.AttemptedAt, accepted, failed,
		nullableText(message.FailureCode), nullableText(message.FailureReason), now, now)
	if err != nil {
		return result, err
	}
	result.MessageID = messageID
	result.Created = true
	return result, tx.Commit()
}

func nullableText(value string) any {
	if value == "" {
		return nil
	}
	return value
}

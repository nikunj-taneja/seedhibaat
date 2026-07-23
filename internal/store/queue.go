package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"regexp"
	"time"
)

type Job struct {
	ID            string
	WorkflowRunID sql.NullString
	StepID        string
	Kind          string
	Payload       []byte
	Attempts      int
	MaxAttempts   int
}

func NewID(prefix string) string {
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		panic(err)
	}
	return prefix + "_" + hex.EncodeToString(random)
}

func (s *Store) EnqueueJob(ctx context.Context, job Job, idempotencyKey string, at time.Time) (bool, error) {
	if job.ID == "" {
		job.ID = NewID("job")
	}
	if job.MaxAttempts == 0 {
		job.MaxAttempts = 8
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	result, err := s.DB.ExecContext(ctx, `
		INSERT INTO scheduled_jobs(id, workflow_run_id, step_id, idempotency_key, kind, payload, scheduled_at, available_at, max_attempts, created_at, updated_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(idempotency_key) DO NOTHING`, job.ID, nullableNullString(job.WorkflowRunID), job.StepID, idempotencyKey, job.Kind, job.Payload, at.UTC().Format(time.RFC3339Nano), at.UTC().Format(time.RFC3339Nano), job.MaxAttempts, now, now)
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	return count == 1, err
}

func (s *Store) ClaimJob(ctx context.Context, workerID string, now time.Time) (Job, bool, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Job{}, false, err
	}
	defer tx.Rollback()
	var job Job
	err = tx.QueryRowContext(ctx, `
		SELECT id, workflow_run_id, step_id, kind, payload, attempts, max_attempts
		FROM scheduled_jobs
		WHERE state IN ('scheduled', 'retry') AND available_at <= ?
		ORDER BY available_at, created_at LIMIT 1`, now.UTC().Format(time.RFC3339Nano)).Scan(&job.ID, &job.WorkflowRunID, &job.StepID, &job.Kind, &job.Payload, &job.Attempts, &job.MaxAttempts)
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, false, nil
	}
	if err != nil {
		return Job{}, false, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE scheduled_jobs SET state='running', locked_at=?, locked_by=?, attempts=attempts+1, updated_at=? WHERE id=? AND state IN ('scheduled','retry')`, now.UTC().Format(time.RFC3339Nano), workerID, now.UTC().Format(time.RFC3339Nano), job.ID)
	if err != nil {
		return Job{}, false, err
	}
	count, err := result.RowsAffected()
	if err != nil || count != 1 {
		return Job{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return Job{}, false, err
	}
	job.Attempts++
	return job, true, nil
}

func (s *Store) CompleteJob(ctx context.Context, id string, now time.Time) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE scheduled_jobs SET state='completed', completed_at=?, locked_at=NULL, locked_by=NULL, updated_at=? WHERE id=? AND state='running'`, now.UTC().Format(time.RFC3339Nano), now.UTC().Format(time.RFC3339Nano), id)
	return err
}

func (s *Store) DeferJob(ctx context.Context, id string, until time.Time, reason string) error {
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	result, err := s.DB.ExecContext(ctx, `UPDATE scheduled_jobs SET state='scheduled',available_at=?,locked_at=NULL,locked_by=NULL,last_error=?,updated_at=? WHERE id=? AND state='running'`, until.UTC().Format(time.RFC3339Nano), reason, stamp, id)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return errors.New("job is not running")
	}
	return nil
}

func (s *Store) FailJob(ctx context.Context, job Job, cause error, now time.Time) error {
	state := "retry"
	if job.Attempts >= job.MaxAttempts {
		state = "failed"
	}
	backoff := time.Duration(math.Min(math.Pow(2, float64(job.Attempts)), 3600)) * time.Second
	available := now.Add(backoff)
	_, err := s.DB.ExecContext(ctx, `UPDATE scheduled_jobs SET state=?, available_at=?, locked_at=NULL, locked_by=NULL, last_error=?, updated_at=? WHERE id=?`, state, available.UTC().Format(time.RFC3339Nano), truncateError(cause), now.UTC().Format(time.RFC3339Nano), job.ID)
	return err
}

func (s *Store) FailJobPermanently(ctx context.Context, job Job, cause error, now time.Time) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE scheduled_jobs SET state='failed',locked_at=NULL,locked_by=NULL,last_error=?,updated_at=? WHERE id=?`, truncateError(cause), now.UTC().Format(time.RFC3339Nano), job.ID)
	return err
}

func (s *Store) RecoverStaleJobs(ctx context.Context, before time.Time) (int64, error) {
	result, err := s.DB.ExecContext(ctx, `UPDATE scheduled_jobs SET state='retry', locked_at=NULL, locked_by=NULL, last_error='worker lease expired', available_at=?, updated_at=? WHERE state='running' AND locked_at < ?`, time.Now().UTC().Format(time.RFC3339Nano), time.Now().UTC().Format(time.RFC3339Nano), before.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func (s *Store) PauseWorkflow(ctx context.Context, runID, reason string) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE workflow_runs SET state='paused', cancellation_reason=? WHERE id=? AND state='active'`, reason, runID)
	if err != nil {
		return err
	}
	if changed, err := result.RowsAffected(); err != nil || changed != 1 {
		return errors.New("workflow run is not active")
	}
	if _, err := tx.ExecContext(ctx, `UPDATE scheduled_jobs SET state='paused', updated_at=? WHERE workflow_run_id=? AND state IN ('scheduled','retry')`, now, runID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ResumeWorkflow(ctx context.Context, runID string) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE workflow_runs SET state='active', cancellation_reason=NULL WHERE id=? AND state='paused'`, runID)
	if err != nil {
		return err
	}
	if changed, err := result.RowsAffected(); err != nil || changed != 1 {
		return errors.New("workflow run is not paused")
	}
	if _, err := tx.ExecContext(ctx, `UPDATE scheduled_jobs SET state='scheduled', available_at=CASE WHEN available_at < ? THEN ? ELSE available_at END, updated_at=? WHERE workflow_run_id=? AND state='paused'`, now, now, now, runID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) CancelWorkflow(ctx context.Context, runID, reason string) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE workflow_runs SET state='cancelled',cancelled_at=?,cancellation_reason=? WHERE id=? AND state IN ('active','paused')`, now, reason, runID)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return errors.New("workflow run is not active or paused")
	}
	if _, err := tx.ExecContext(ctx, `UPDATE scheduled_jobs SET state='cancelled',last_error=?,locked_at=NULL,locked_by=NULL,completed_at=?,updated_at=? WHERE workflow_run_id=? AND state IN ('scheduled','retry','paused')`, reason, now, now, runID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ReplayFailedJob(ctx context.Context, jobID string, now time.Time) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var state string
	if err := tx.QueryRowContext(ctx, `SELECT state FROM scheduled_jobs WHERE id=?`, jobID).Scan(&state); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errors.New("job not found")
		}
		return err
	}
	if state != "failed" {
		return fmt.Errorf("only failed jobs can be replayed; current state is %s", state)
	}
	var messageState sql.NullString
	var metaID sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT state,meta_message_id FROM outbound_messages WHERE job_id=?`, jobID).Scan(&messageState, &metaID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if metaID.Valid || (messageState.Valid && (messageState.String == "accepted" || messageState.String == "sent" || messageState.String == "delivered" || messageState.String == "read" || messageState.String == "unknown")) {
		return errors.New("job may already have been accepted; replay is blocked to prevent a duplicate send")
	}
	stamp := now.UTC().Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, `UPDATE scheduled_jobs SET state='scheduled',attempts=0,available_at=?,scheduled_at=?,locked_at=NULL,locked_by=NULL,last_error=NULL,completed_at=NULL,updated_at=? WHERE id=? AND state='failed'`, stamp, stamp, stamp, jobID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) CancelCustomerWork(ctx context.Context, customerID int64, reason string) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE workflow_runs SET state='cancelled', cancelled_at=?, cancellation_reason=? WHERE customer_id=? AND state IN ('active','paused')`, now, reason, customerID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE scheduled_jobs SET state='cancelled', last_error=?, updated_at=? WHERE workflow_run_id IN (SELECT id FROM workflow_runs WHERE customer_id=?) AND state IN ('scheduled','retry','paused')`, reason, now, customerID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE scheduled_jobs SET state='cancelled',last_error=?,completed_at=?,updated_at=? WHERE kind='send_whatsapp' AND CAST(json_extract(payload,'$.customer_id') AS INTEGER)=? AND state IN ('scheduled','retry','paused')`, reason, now, now, customerID); err != nil {
		return err
	}
	return tx.Commit()
}

func nullableNullString(value sql.NullString) any {
	if !value.Valid || value.String == "" {
		return nil
	}
	return value.String
}

func truncateError(err error) string {
	if err == nil {
		return "unknown error"
	}
	message := persistedPhonePattern.ReplaceAllString(err.Error(), "[redacted-number]")
	message = persistedTokenPattern.ReplaceAllString(message, "[redacted-secret]")
	if len(message) > 1000 {
		return message[:1000]
	}
	return message
}

var (
	persistedPhonePattern = regexp.MustCompile(`\+?\d[\d\s().-]{8,}\d`)
	persistedTokenPattern = regexp.MustCompile(`\b(?:EA|shpss_|shpat_|shpca_)[A-Za-z0-9_-]{20,}\b`)
)

func (j Job) Validate() error {
	if j.Kind == "" || j.StepID == "" || len(j.Payload) == 0 {
		return fmt.Errorf("job kind, step ID, and payload are required")
	}
	return nil
}

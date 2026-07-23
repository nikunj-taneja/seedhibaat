package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

func (s *Store) SyncWatermark(ctx context.Context, source, resource string) (time.Time, bool, error) {
	var value sql.NullString
	err := s.DB.QueryRowContext(ctx, `SELECT watermark FROM sync_cursors WHERE source=? AND resource=?`, source, resource).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) || !value.Valid || value.String == "" {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	parsed, err := time.Parse(time.RFC3339Nano, value.String)
	if err != nil {
		return time.Time{}, false, err
	}
	return parsed, true, nil
}

func (s *Store) SetSyncWatermark(ctx context.Context, source, resource string, watermark time.Time) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := s.DB.ExecContext(ctx, `INSERT INTO sync_cursors(source,resource,watermark,updated_at) VALUES(?,?,?,?)
		ON CONFLICT(source,resource) DO UPDATE SET watermark=excluded.watermark,updated_at=excluded.updated_at`, source, resource, watermark.UTC().Format(time.RFC3339Nano), now)
	return err
}

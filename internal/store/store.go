package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

type Store struct {
	DB *sql.DB
}

func Open(ctx context.Context, path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("database path is empty")
	}
	if path != ":memory:" {
		path = filepath.Clean(path)
	}
	dsn := path + "?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetConnMaxLifetime(0)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	store := &Store{DB: db}
	if err := store.Migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) Close() error { return s.DB.Close() }

func (s *Store) Migrate(ctx context.Context) error {
	entries, err := fs.Glob(migrationFS, "migrations/*.sql")
	if err != nil {
		return fmt.Errorf("list migrations: %w", err)
	}
	sort.Strings(entries)
	for _, entry := range entries {
		base := filepath.Base(entry)
		versionText, _, ok := strings.Cut(base, "_")
		if !ok {
			return fmt.Errorf("invalid migration filename %q", base)
		}
		version, err := strconv.Atoi(versionText)
		if err != nil {
			return fmt.Errorf("invalid migration version %q: %w", base, err)
		}
		var exists int
		err = s.DB.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type='table' AND name='schema_migrations'").Scan(&exists)
		if err != nil {
			return err
		}
		if exists > 0 {
			var applied int
			if err := s.DB.QueryRowContext(ctx, "SELECT count(*) FROM schema_migrations WHERE version = ?", version).Scan(&applied); err != nil {
				return err
			}
			if applied > 0 {
				continue
			}
		}
		body, err := migrationFS.ReadFile(entry)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", base, err)
		}
		tx, err := s.DB.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, string(body)); err != nil {
			tx.Rollback()
			return fmt.Errorf("apply migration %s: %w", base, err)
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO schema_migrations(version, applied_at) VALUES(?, ?)", version, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			tx.Rollback()
			return fmt.Errorf("record migration %s: %w", base, err)
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) IntegrityCheck(ctx context.Context) error {
	var result string
	if err := s.DB.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&result); err != nil {
		return err
	}
	if result != "ok" {
		return fmt.Errorf("sqlite integrity check: %s", result)
	}
	return nil
}

func (s *Store) Audit(ctx context.Context, actor, action, objectType, objectID, detailsJSON string) error {
	if detailsJSON == "" {
		detailsJSON = "{}"
	}
	_, err := s.DB.ExecContext(ctx, `INSERT INTO audit_log(occurred_at, actor, action, object_type, object_id, details_json) VALUES(?, ?, ?, ?, ?, ?)`, time.Now().UTC().Format(time.RFC3339Nano), actor, action, objectType, nullable(objectID), detailsJSON)
	return err
}

func nullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}

package backup

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/nikunj-taneja/seedhibaat/internal/store"
)

func TestEncryptedBackupRestore(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	source, err := store.Open(ctx, filepath.Join(directory, "source.db"))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = source.DB.Exec(`INSERT INTO audit_log(occurred_at,actor,action,object_type,details_json) VALUES('now','test','created','test','{}')`)
	backupPath, err := Create(ctx, source, filepath.Join(directory, "backups"), "backup-key", time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	source.Close()
	restorePath := filepath.Join(directory, "restored.db")
	if err := Restore(ctx, backupPath, restorePath, "backup-key"); err != nil {
		t.Fatal(err)
	}
	restored, err := store.Open(ctx, restorePath)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	var count int
	if err := restored.DB.QueryRow(`SELECT count(*) FROM audit_log`).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	if err := Restore(ctx, backupPath, filepath.Join(directory, "wrong.db"), "wrong-key"); err == nil {
		t.Fatal("wrong backup key accepted")
	}
}

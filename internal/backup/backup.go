package backup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nikunj-taneja/seedhibaat/internal/security"
	"github.com/nikunj-taneja/seedhibaat/internal/store"
)

var header = []byte("SEEDHIBAAT-BACKUP-V1\n")

func Create(ctx context.Context, database *store.Store, directory, key string, now time.Time) (string, error) {
	if key == "" {
		return "", errors.New("backup key is empty")
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		return "", err
	}
	snapshot, err := os.CreateTemp(directory, ".snapshot-*.db")
	if err != nil {
		return "", err
	}
	snapshotPath := snapshot.Name()
	snapshot.Close()
	_ = os.Remove(snapshotPath)
	defer os.Remove(snapshotPath)
	escaped := strings.ReplaceAll(snapshotPath, "'", "''")
	if _, err := database.DB.ExecContext(ctx, "VACUUM INTO '"+escaped+"'"); err != nil {
		return "", fmt.Errorf("create SQLite snapshot: %w", err)
	}
	plaintext, err := os.ReadFile(snapshotPath)
	if err != nil {
		return "", err
	}
	ciphertext, err := security.Encrypt(key, plaintext)
	if err != nil {
		return "", err
	}
	destination := filepath.Join(directory, "seedhibaat-"+now.UTC().Format("20060102T150405Z")+".db.enc")
	temporary, err := os.CreateTemp(directory, ".backup-*.tmp")
	if err != nil {
		return "", err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0600); err != nil {
		temporary.Close()
		return "", err
	}
	if _, err := temporary.Write(append(header, ciphertext...)); err != nil {
		temporary.Close()
		return "", err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return "", err
	}
	if err := temporary.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(temporaryPath, destination); err != nil {
		return "", err
	}
	return destination, nil
}

func Restore(ctx context.Context, input, output, key string) error {
	if key == "" {
		return errors.New("backup key is empty")
	}
	if _, err := os.Stat(output); err == nil {
		return errors.New("restore target already exists")
	}
	body, err := os.ReadFile(input)
	if err != nil {
		return err
	}
	if len(body) <= len(header) || string(body[:len(header)]) != string(header) {
		return errors.New("invalid backup header")
	}
	plaintext, err := security.Decrypt(key, body[len(header):])
	if err != nil {
		return fmt.Errorf("decrypt backup: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(output), 0700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(output), ".restore-*.db")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(plaintext); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	restored, err := store.Open(ctx, temporaryPath)
	if err != nil {
		return fmt.Errorf("open restored database: %w", err)
	}
	if err := restored.IntegrityCheck(ctx); err != nil {
		restored.Close()
		return err
	}
	if err := restored.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, output)
}

func Prune(directory string, olderThan time.Time) (int, error) {
	entries, err := os.ReadDir(directory)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "seedhibaat-") || !strings.HasSuffix(entry.Name(), ".db.enc") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return removed, err
		}
		if info.ModTime().Before(olderThan) {
			if err := os.Remove(filepath.Join(directory, entry.Name())); err != nil {
				return removed, err
			}
			removed++
		}
	}
	return removed, nil
}

package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/wendi/pulseguard/internal/config"
)

// newMigratedDB opens a fresh temp-file SQLite DB and runs all migrations.
// Each test gets its own filesystem path so concurrency is safe.
func newMigratedDB(t *testing.T) *sql.DB {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "test.db")
	db, err := Open(config.Database{Path: p, BusyTimeout: config.Duration(5 * time.Second)})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := Migrate(context.Background(), db, nil); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return db
}

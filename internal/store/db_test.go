package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/wendi/pulseguard/internal/config"
)

// openTestDB opens a temp-file SQLite DB. We avoid :memory: because
// some integration tests share connections and modernc.org/sqlite gives
// each connection in the pool a private memory database.
func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "test.db")
	db, err := Open(config.Database{Path: p, BusyTimeout: config.Duration(5 * time.Second)})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestOpen_AppliesPragmas(t *testing.T) {
	db := openTestDB(t)

	var mode string
	if err := db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatalf("journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Fatalf("journal_mode = %q want wal", mode)
	}

	var fk int
	if err := db.QueryRow("PRAGMA foreign_keys").Scan(&fk); err != nil {
		t.Fatalf("foreign_keys: %v", err)
	}
	if fk != 1 {
		t.Fatalf("foreign_keys = %d want 1", fk)
	}
}

func TestMigrate_AllTablesPresent(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	want := []string{
		"bots",
		"channels",
		"dead_letters",
		"dedup_keys",
		"invite_codes",
		"push_logs",
		"push_outbox",
		"rate_buckets",
		"sessions",
		"templates",
		"tenants",
	}
	got := readTables(t, db)
	if len(got) != len(want) {
		t.Fatalf("got tables=%v\nwant %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("table[%d]=%q want %q\nfull got=%v", i, got[i], want[i], got)
		}
	}
}

func TestMigrate_Idempotent(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("first Migrate: %v", err)
	}
	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}

	var versions int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&versions); err != nil {
		t.Fatalf("count migrations: %v", err)
	}
	if versions != 1 {
		t.Fatalf("schema_migrations rows = %d want 1", versions)
	}
}

func TestMigrate_RecordsVersion(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	var v int
	var appliedAt int64
	if err := db.QueryRow(`SELECT version, applied_at FROM schema_migrations`).Scan(&v, &appliedAt); err != nil {
		t.Fatalf("select migration row: %v", err)
	}
	if v != 1 {
		t.Fatalf("version = %d want 1", v)
	}
	if appliedAt <= 0 {
		t.Fatalf("applied_at = %d want >0", appliedAt)
	}
}

func readTables(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`
		SELECT name FROM sqlite_master
		 WHERE type='table'
		   AND name NOT LIKE 'sqlite_%'
		   AND name <> 'schema_migrations'
		 ORDER BY name`)
	if err != nil {
		t.Fatalf("select sqlite_master: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

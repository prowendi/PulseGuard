package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/wendi/pulseguard/internal/config"
	"github.com/wendi/pulseguard/internal/domain"
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
	if err := Migrate(ctx, db, nil); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	want := []string{
		"bots",
		"channel_templates",
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
	if err := Migrate(ctx, db, nil); err != nil {
		t.Fatalf("first Migrate: %v", err)
	}
	if err := Migrate(ctx, db, nil); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}

	// The migrations directory grows over time; assert the on-disk count
	// equals whatever migrations/*.sql ships in the binary, so this test
	// keeps tracking the canonical "applied == bundled" invariant without
	// being hard-coded to a specific version count.
	wantVersions := countBundledMigrations(t)

	var versions int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&versions); err != nil {
		t.Fatalf("count migrations: %v", err)
	}
	if versions != wantVersions {
		t.Fatalf("schema_migrations rows = %d want %d (matching bundled migrations)", versions, wantVersions)
	}
}

func TestMigrate_RecordsVersion(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	clk := &domain.FakeClock{T: time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)}
	if err := Migrate(ctx, db, clk); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	// applied_at on the very first migration must equal the FakeClock now.
	var appliedAt int64
	if err := db.QueryRow(`SELECT applied_at FROM schema_migrations WHERE version = 1`).Scan(&appliedAt); err != nil {
		t.Fatalf("select migration row: %v", err)
	}
	wantMs := clk.Now().UnixMilli()
	if appliedAt != wantMs {
		t.Fatalf("applied_at = %d want %d (FakeClock = %s)", appliedAt, wantMs, clk.Now())
	}
}

// countBundledMigrations reads the embedded migrations/ directory and
// returns the number of *.sql files shipping in the binary. Used so
// migration-count assertions stay in sync with the migration pack.
func countBundledMigrations(t *testing.T) int {
	t.Helper()
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		t.Fatalf("read migrations dir: %v", err)
	}
	n := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if filepath.Ext(e.Name()) == ".sql" {
			n++
		}
	}
	return n
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

package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/wendi/pulseguard/internal/domain"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Migrate applies every migrations/NNNN_*.sql script not yet recorded in
// the schema_migrations table. Calls are idempotent. The supplied clock
// stamps applied_at on each new migration so tests can replay migrations
// against a deterministic FakeClock and so the project's "all timestamps
// flow through domain.Clock" invariant is preserved (spec §6).
func Migrate(ctx context.Context, db *sql.DB, clock domain.Clock) error {
	return MigrateFS(ctx, db, migrationsFS, "migrations", clock)
}

// MigrateFS allows tests to substitute a custom embed.FS or sub-FS.
func MigrateFS(ctx context.Context, db *sql.DB, src fs.FS, dir string, clock domain.Clock) error {
	if clock == nil {
		clock = domain.RealClock()
	}
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
		  version    INTEGER PRIMARY KEY,
		  applied_at INTEGER NOT NULL
		)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	applied, err := loadApplied(ctx, db)
	if err != nil {
		return err
	}

	files, err := collectMigrationFiles(src, dir)
	if err != nil {
		return err
	}

	for _, f := range files {
		if applied[f.version] {
			continue
		}
		body, err := fs.ReadFile(src, dir+"/"+f.name)
		if err != nil {
			return fmt.Errorf("read %s: %w", f.name, err)
		}
		if err := runMigration(ctx, db, clock, f.version, string(body)); err != nil {
			return fmt.Errorf("migration %d (%s): %w", f.version, f.name, err)
		}
	}
	return nil
}

func loadApplied(ctx context.Context, db *sql.DB) (map[int]bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("select schema_migrations: %w", err)
	}
	defer rows.Close()
	out := map[int]bool{}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out[v] = true
	}
	return out, rows.Err()
}

type migrationFile struct {
	version int
	name    string
}

func collectMigrationFiles(src fs.FS, dir string) ([]migrationFile, error) {
	entries, err := fs.ReadDir(src, dir)
	if err != nil {
		return nil, fmt.Errorf("read dir %s: %w", dir, err)
	}
	var out []migrationFile
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		v, err := parseVersion(e.Name())
		if err != nil {
			return nil, err
		}
		out = append(out, migrationFile{version: v, name: e.Name()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}

func parseVersion(name string) (int, error) {
	// expected layout: NNNN_<label>.sql
	prefix := name
	if idx := strings.IndexByte(name, '_'); idx > 0 {
		prefix = name[:idx]
	}
	var v int
	if _, err := fmt.Sscanf(prefix, "%d", &v); err != nil {
		return 0, fmt.Errorf("migration %q: cannot parse version: %w", name, err)
	}
	return v, nil
}

func runMigration(ctx context.Context, db *sql.DB, clock domain.Clock, version int, body string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, body); err != nil {
		return fmt.Errorf("exec ddl: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations(version, applied_at) VALUES(?, ?)`,
		version, clock.Now().UnixMilli()); err != nil {
		return fmt.Errorf("record migration: %w", err)
	}
	return tx.Commit()
}

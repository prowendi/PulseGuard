package store

import (
	"context"
	"strings"
	"testing"
)

// seedCommandRowForGuardTest puts a single row in `commands` so the
// SEC-2 guard sees non-empty data. FKs are deliberately disabled for
// this synthetic seed — we don't need a valid tenant/bot graph just
// to exercise the count check.
func seedCommandRowForGuardTest(t *testing.T) func() error {
	t.Helper()
	db := newMigratedDB(t)
	if _, err := db.Exec(`PRAGMA foreign_keys = OFF`); err != nil {
		t.Fatalf("fk off: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO commands(tenant_id,bot_id,name,description,code,enabled,created_at,updated_at)
		  VALUES(1,1,'/seeded','','x',1,1,1);
	`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return func() error {
		return guardDestructiveMigration(context.Background(), db, 11)
	}
}

// TestGuardDestructiveMigration_AllowsEmptyTables: greenfield databases
// hit the destructive migration with no rows, so the guard MUST allow
// it. Without this case the upgrade path for fresh installations
// would be broken.
func TestGuardDestructiveMigration_AllowsEmptyTables(t *testing.T) {
	db := newMigratedDB(t)
	if _, err := db.Exec(`DELETE FROM commands; DELETE FROM subscribers;`); err != nil {
		t.Fatalf("wipe: %v", err)
	}
	if err := guardDestructiveMigration(context.Background(), db, 11); err != nil {
		t.Fatalf("empty tables should pass: %v", err)
	}
}

// TestGuardDestructiveMigration_BlocksOnDataWithoutEnv: SEC-2 core.
// commands has rows AND env unset → refuse with a message that points
// the operator at the env-var escape hatch.
func TestGuardDestructiveMigration_BlocksOnDataWithoutEnv(t *testing.T) {
	t.Setenv("PULSEGUARD_DEV_RESET", "")
	run := seedCommandRowForGuardTest(t)
	err := run()
	if err == nil {
		t.Fatalf("guard should refuse when commands has rows and env is unset")
	}
	if !strings.Contains(err.Error(), "PULSEGUARD_DEV_RESET") {
		t.Fatalf("guard error should mention env var, got: %v", err)
	}
}

// TestGuardDestructiveMigration_OptInWithEnv: operator who knows what
// they're doing can opt in by setting PULSEGUARD_DEV_RESET=1.
func TestGuardDestructiveMigration_OptInWithEnv(t *testing.T) {
	t.Setenv("PULSEGUARD_DEV_RESET", "1")
	run := seedCommandRowForGuardTest(t)
	if err := run(); err != nil {
		t.Fatalf("explicit opt-in should pass, got: %v", err)
	}
}

// TestGuardDestructiveMigration_OtherVersionsUnaffected: only the
// versions enumerated in guardDestructiveMigration get the special
// treatment; ordinary migrations must not be slowed down or blocked.
func TestGuardDestructiveMigration_OtherVersionsUnaffected(t *testing.T) {
	db := newMigratedDB(t)
	for _, v := range []int{1, 5, 10, 99} {
		if err := guardDestructiveMigration(context.Background(), db, v); err != nil {
			t.Fatalf("version %d should not be guarded: %v", v, err)
		}
	}
}

package bootstrap

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/wendi/pulseguard/internal/auth"
	"github.com/wendi/pulseguard/internal/config"
	"github.com/wendi/pulseguard/internal/domain"
	"github.com/wendi/pulseguard/internal/store"
)

func newTenantRepo(t *testing.T) (*store.TenantRepo, *domain.FakeClock) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "bootstrap.db")
	db, err := store.Open(config.Database{
		Path:        dbPath,
		BusyTimeout: config.Duration(5 * time.Second),
	})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(context.Background(), db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	clk := &domain.FakeClock{T: time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)}
	return store.NewTenantRepo(db, clk), clk
}

func TestEnsureInitialAdmin_CreatesAdminOnEmptyDB(t *testing.T) {
	tenants, clk := newTenantRepo(t)
	cfg := config.Bootstrap{
		InitialAdminEmail:    "admin@example.com",
		InitialAdminPassword: "s3cret-pass",
	}
	sec := config.Security{BcryptCost: 4}

	err := EnsureInitialAdmin(context.Background(),
		BootstrapRepos{Tenants: tenants}, cfg, sec, clk)
	if err != nil {
		t.Fatalf("EnsureInitialAdmin: %v", err)
	}

	admin, err := tenants.GetByEmail(context.Background(), "admin@example.com")
	if err != nil {
		t.Fatalf("GetByEmail: %v", err)
	}
	if admin.Role != domain.RoleAdmin {
		t.Fatalf("role = %q, want admin", admin.Role)
	}
	if admin.Status != domain.TenantActive {
		t.Fatalf("status = %q, want active", admin.Status)
	}
	if err := auth.CompareHashAndPassword([]byte(admin.PasswordHash), "s3cret-pass"); err != nil {
		t.Fatalf("password hash does not match: %v", err)
	}
	if admin.ID == 0 {
		t.Fatal("admin id not populated")
	}
}

func TestEnsureInitialAdmin_NoOpOnNonEmptyDB(t *testing.T) {
	tenants, clk := newTenantRepo(t)

	// Pre-seed a user tenant so CountActive>0.
	existing := &domain.Tenant{
		Email:        "user@example.com",
		PasswordHash: "doesnotmatter",
		Role:         domain.RoleUser,
		Status:       domain.TenantActive,
	}
	if err := tenants.Insert(context.Background(), existing); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	cfg := config.Bootstrap{
		InitialAdminEmail:    "admin@example.com",
		InitialAdminPassword: "s3cret-pass",
	}
	sec := config.Security{BcryptCost: 4}

	if err := EnsureInitialAdmin(context.Background(),
		BootstrapRepos{Tenants: tenants}, cfg, sec, clk); err != nil {
		t.Fatalf("EnsureInitialAdmin: %v", err)
	}

	// Admin should NOT have been created.
	if _, err := tenants.GetByEmail(context.Background(), "admin@example.com"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("expected ErrNotFound for admin, got %v", err)
	}
}

func TestEnsureInitialAdmin_FailsWithoutEmail(t *testing.T) {
	tenants, clk := newTenantRepo(t)
	cfg := config.Bootstrap{
		InitialAdminEmail:    "",
		InitialAdminPassword: "s3cret",
	}
	sec := config.Security{BcryptCost: 4}

	err := EnsureInitialAdmin(context.Background(),
		BootstrapRepos{Tenants: tenants}, cfg, sec, clk)
	if err == nil {
		t.Fatal("expected error when initial admin email missing")
	}
}

func TestEnsureInitialAdmin_FailsWithoutPassword(t *testing.T) {
	tenants, clk := newTenantRepo(t)
	cfg := config.Bootstrap{
		InitialAdminEmail:    "admin@example.com",
		InitialAdminPassword: "",
	}
	sec := config.Security{BcryptCost: 4}

	err := EnsureInitialAdmin(context.Background(),
		BootstrapRepos{Tenants: tenants}, cfg, sec, clk)
	if err == nil {
		t.Fatal("expected error when initial admin password missing")
	}
}

func TestEnsureInitialAdmin_IdempotentSecondCall(t *testing.T) {
	tenants, clk := newTenantRepo(t)
	cfg := config.Bootstrap{
		InitialAdminEmail:    "admin@example.com",
		InitialAdminPassword: "s3cret-pass",
	}
	sec := config.Security{BcryptCost: 4}

	if err := EnsureInitialAdmin(context.Background(),
		BootstrapRepos{Tenants: tenants}, cfg, sec, clk); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if err := EnsureInitialAdmin(context.Background(),
		BootstrapRepos{Tenants: tenants}, cfg, sec, clk); err != nil {
		t.Fatalf("second call: %v", err)
	}

	n, err := tenants.CountActive(context.Background())
	if err != nil {
		t.Fatalf("CountActive: %v", err)
	}
	if n != 1 {
		t.Fatalf("active tenant count = %d, want 1", n)
	}
}

func TestEnsureInitialAdmin_NilTenantsRepo(t *testing.T) {
	clk := &domain.FakeClock{T: time.Now()}
	err := EnsureInitialAdmin(context.Background(), BootstrapRepos{},
		config.Bootstrap{InitialAdminEmail: "a@b", InitialAdminPassword: "p"},
		config.Security{BcryptCost: 4}, clk)
	if err == nil {
		t.Fatal("expected error when Tenants nil")
	}
}

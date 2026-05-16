package auth

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/wendi/pulseguard/internal/config"
	"github.com/wendi/pulseguard/internal/domain"
	"github.com/wendi/pulseguard/internal/store"
)

// testFixture wires the auth service with the real SQLite repos so the
// transactional invariants (unique email, invite consume race) get exercised.
type testFixture struct {
	svc      *Service
	tenants  *store.TenantRepo
	invites  *store.InviteRepo
	sessions *store.SessionRepo
	clock    *domain.FakeClock
}

func newFixture(t *testing.T) *testFixture {
	t.Helper()
	dir := t.TempDir()
	dbcfg := config.Database{Path: filepath.Join(dir, "test.db"), BusyTimeout: config.Duration(5 * time.Second)}
	db, err := store.Open(dbcfg)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	clk := &domain.FakeClock{T: time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)}
	if err := store.Migrate(context.Background(), db, clk); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	tenants := store.NewTenantRepo(db, clk)
	invites := store.NewInviteRepo(db, clk)
	sessions := store.NewSessionRepo(db, clk)
	cfg := config.Security{
		MasterKeyB64: "",
		SessionTTL:   config.Duration(14 * 24 * time.Hour),
		CookieSecure: true,
		BcryptCost:   4, // minimum legal cost for fast tests
	}
	svc := New(tenants, invites, sessions, cfg, clk)
	return &testFixture{svc: svc, tenants: tenants, invites: invites, sessions: sessions, clock: clk}
}

// seedAdminAndInvite returns (adminID, inviteCode) ready for registration.
func (f *testFixture) seedAdminAndInvite(t *testing.T, expiresIn time.Duration) (int64, string) {
	t.Helper()
	admin := &domain.Tenant{
		Email:        "admin@example.com",
		PasswordHash: "x",
		Role:         domain.RoleAdmin,
		Status:       domain.TenantActive,
	}
	if err := f.tenants.Insert(context.Background(), admin); err != nil {
		t.Fatalf("seed admin: %v", err)
	}
	var inv *domain.InviteCode
	var err error
	if expiresIn > 0 {
		inv, err = f.svc.GenerateInvite(context.Background(), admin.ID, expiresIn)
	} else {
		inv, err = f.svc.GenerateInvite(context.Background(), admin.ID, 0)
	}
	if err != nil {
		t.Fatalf("GenerateInvite: %v", err)
	}
	return admin.ID, inv.Code
}

func TestRegisterSuccess(t *testing.T) {
	f := newFixture(t)
	_, code := f.seedAdminAndInvite(t, time.Hour)

	ctx := context.Background()
	tenant, sess, err := f.svc.Register(ctx, "user@example.com", "hunter2", code)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if tenant.ID == 0 {
		t.Fatalf("tenant id is zero")
	}
	if tenant.Email != "user@example.com" {
		t.Fatalf("email = %q", tenant.Email)
	}
	if tenant.Role != domain.RoleUser {
		t.Fatalf("role = %q", tenant.Role)
	}
	if sess.ID == "" || sess.TenantID != tenant.ID {
		t.Fatalf("bad session: %+v", sess)
	}
	if !sess.ExpiresAt.After(f.clock.Now()) {
		t.Fatalf("session not in future: %v vs %v", sess.ExpiresAt, f.clock.Now())
	}

	// Invite must be consumed
	inv, err := f.invites.Lock(ctx, code)
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}
	if inv.UsedAt == nil {
		t.Fatalf("invite not consumed")
	}
	if inv.UsedBy == nil || *inv.UsedBy != tenant.ID {
		t.Fatalf("invite used_by wrong: %v", inv.UsedBy)
	}

	// Session round-trip
	gotTenant, gotSess, err := f.svc.SessionFromID(ctx, sess.ID)
	if err != nil {
		t.Fatalf("SessionFromID: %v", err)
	}
	if gotTenant.ID != tenant.ID || gotSess.ID != sess.ID {
		t.Fatalf("roundtrip mismatch")
	}
}

func TestRegisterDuplicateEmail(t *testing.T) {
	f := newFixture(t)
	_, code1 := f.seedAdminAndInvite(t, time.Hour)

	ctx := context.Background()
	if _, _, err := f.svc.Register(ctx, "dup@example.com", "pw", code1); err != nil {
		t.Fatalf("first register: %v", err)
	}

	// Issue a second code (different one) and try with same email
	adminTenant, err := f.tenants.GetByEmail(ctx, "admin@example.com")
	if err != nil {
		t.Fatalf("get admin: %v", err)
	}
	inv2, err := f.svc.GenerateInvite(ctx, adminTenant.ID, time.Hour)
	if err != nil {
		t.Fatalf("GenerateInvite 2: %v", err)
	}

	_, _, err = f.svc.Register(ctx, "dup@example.com", "pw", inv2.Code)
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("expected ErrConflict, got %v", err)
	}
}

func TestRegisterInviteCases(t *testing.T) {
	t.Run("unknown_code", func(t *testing.T) {
		f := newFixture(t)
		_, _, err := f.svc.Register(context.Background(), "a@example.com", "pw", "no-such")
		if !errors.Is(err, domain.ErrInviteInvalid) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("expired", func(t *testing.T) {
		f := newFixture(t)
		_, code := f.seedAdminAndInvite(t, time.Hour)
		f.clock.Advance(2 * time.Hour)
		_, _, err := f.svc.Register(context.Background(), "a@example.com", "pw", code)
		if !errors.Is(err, domain.ErrInviteInvalid) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("already_used", func(t *testing.T) {
		f := newFixture(t)
		_, code := f.seedAdminAndInvite(t, time.Hour)
		if _, _, err := f.svc.Register(context.Background(), "first@example.com", "pw", code); err != nil {
			t.Fatalf("first register: %v", err)
		}
		_, _, err := f.svc.Register(context.Background(), "second@example.com", "pw", code)
		if !errors.Is(err, domain.ErrInviteInvalid) {
			t.Fatalf("got %v", err)
		}
	})
}

func TestLoginSuccess(t *testing.T) {
	f := newFixture(t)
	_, code := f.seedAdminAndInvite(t, time.Hour)
	ctx := context.Background()
	tenant, _, err := f.svc.Register(ctx, "u@example.com", "hunter2", code)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	sess, err := f.svc.Login(ctx, "u@example.com", "hunter2")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if sess.TenantID != tenant.ID {
		t.Fatalf("tenant mismatch")
	}
}

func TestLoginBadPassword(t *testing.T) {
	f := newFixture(t)
	_, code := f.seedAdminAndInvite(t, time.Hour)
	ctx := context.Background()
	if _, _, err := f.svc.Register(ctx, "u@example.com", "right", code); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, err := f.svc.Login(ctx, "u@example.com", "wrong"); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("got %v", err)
	}
}

func TestLoginUnknownEmail(t *testing.T) {
	f := newFixture(t)
	if _, err := f.svc.Login(context.Background(), "nobody@example.com", "x"); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("got %v", err)
	}
}

func TestSessionFromIDExpired(t *testing.T) {
	f := newFixture(t)
	_, code := f.seedAdminAndInvite(t, time.Hour)
	ctx := context.Background()
	_, sess, err := f.svc.Register(ctx, "u@example.com", "pw", code)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	// Jump past TTL
	f.clock.Advance(15 * 24 * time.Hour)
	if _, _, err := f.svc.SessionFromID(ctx, sess.ID); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("got %v", err)
	}
}

func TestSessionFromIDUnknown(t *testing.T) {
	f := newFixture(t)
	if _, _, err := f.svc.SessionFromID(context.Background(), "bogus"); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("got %v", err)
	}
}

func TestLogoutDeletesSession(t *testing.T) {
	f := newFixture(t)
	_, code := f.seedAdminAndInvite(t, time.Hour)
	ctx := context.Background()
	_, sess, err := f.svc.Register(ctx, "u@example.com", "pw", code)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := f.svc.Logout(ctx, sess.ID); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if _, _, err := f.svc.SessionFromID(ctx, sess.ID); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("got %v after logout", err)
	}
}

func TestGenerateInviteShape(t *testing.T) {
	f := newFixture(t)
	admin := &domain.Tenant{Email: "boss@example.com", PasswordHash: "x", Role: domain.RoleAdmin}
	if err := f.tenants.Insert(context.Background(), admin); err != nil {
		t.Fatalf("seed admin: %v", err)
	}
	inv, err := f.svc.GenerateInvite(context.Background(), admin.ID, time.Hour)
	if err != nil {
		t.Fatalf("GenerateInvite: %v", err)
	}
	if len(inv.Code) != 32 {
		t.Fatalf("code length = %d, want 32", len(inv.Code))
	}
	if inv.CreatedBy != admin.ID {
		t.Fatalf("created_by mismatch")
	}
	if inv.ExpiresAt == nil {
		t.Fatalf("expires_at nil")
	}
}

func TestGenerateInviteNoTTL(t *testing.T) {
	f := newFixture(t)
	admin := &domain.Tenant{Email: "boss@example.com", PasswordHash: "x", Role: domain.RoleAdmin}
	if err := f.tenants.Insert(context.Background(), admin); err != nil {
		t.Fatalf("seed admin: %v", err)
	}
	inv, err := f.svc.GenerateInvite(context.Background(), admin.ID, 0)
	if err != nil {
		t.Fatalf("GenerateInvite: %v", err)
	}
	if inv.ExpiresAt != nil {
		t.Fatalf("expected nil expiry, got %v", inv.ExpiresAt)
	}
}

func TestHashPasswordRoundtrip(t *testing.T) {
	h, err := HashPassword("hunter2", 4)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if err := CompareHashAndPassword(h, "hunter2"); err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if err := CompareHashAndPassword(h, "wrong"); err == nil {
		t.Fatalf("expected mismatch error")
	}
}

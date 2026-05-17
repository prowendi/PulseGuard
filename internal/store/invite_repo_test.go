package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wendi/pulseguard/internal/domain"
)

// inviteSetup inserts an admin tenant + clock and returns repos.
func inviteSetup(t *testing.T) (*TenantRepo, *InviteRepo, *domain.Tenant, *domain.FakeClock) {
	t.Helper()
	db := newMigratedDB(t)
	clk := &domain.FakeClock{T: time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)}
	tr := NewTenantRepo(db, clk)
	ir := NewInviteRepo(db, clk)
	admin := makeTenant("admin@x.com")
	admin.Role = domain.RoleAdmin
	if err := tr.Insert(context.Background(), admin); err != nil {
		t.Fatalf("seed admin: %v", err)
	}
	return tr, ir, admin, clk
}

func TestInviteRepo_InsertAndLock(t *testing.T) {
	_, ir, admin, _ := inviteSetup(t)
	ctx := context.Background()

	inv := &domain.InviteCode{Code: "ABC123", CreatedBy: admin.ID}
	if err := ir.Insert(ctx, inv); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if inv.CreatedAt.IsZero() {
		t.Fatalf("CreatedAt unset")
	}

	got, err := ir.Lock(ctx, "ABC123")
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}
	if got.CreatedBy != admin.ID {
		t.Fatalf("CreatedBy = %d want %d", got.CreatedBy, admin.ID)
	}
	if got.UsedAt != nil {
		t.Fatalf("UsedAt should be nil")
	}
}

func TestInviteRepo_Lock_Missing(t *testing.T) {
	_, ir, _, _ := inviteSetup(t)
	_, err := ir.Lock(context.Background(), "missing")
	if !errors.Is(err, domain.ErrInviteInvalid) {
		t.Fatalf("Lock missing = %v want ErrInviteInvalid", err)
	}
}

func TestInviteRepo_Consume(t *testing.T) {
	tr, ir, admin, clk := inviteSetup(t)
	ctx := context.Background()

	if err := ir.Insert(ctx, &domain.InviteCode{Code: "X1", CreatedBy: admin.ID}); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	user := makeTenant("user@x.com")
	if err := tr.Insert(ctx, user); err != nil {
		t.Fatalf("Insert user: %v", err)
	}

	clk.Advance(time.Minute)
	if err := ir.Consume(ctx, "X1", user.ID); err != nil {
		t.Fatalf("Consume: %v", err)
	}

	got, err := ir.Lock(ctx, "X1")
	if err != nil {
		t.Fatalf("re-Lock: %v", err)
	}
	if got.UsedAt == nil {
		t.Fatalf("UsedAt missing after consume")
	}
	if got.UsedBy == nil || *got.UsedBy != user.ID {
		t.Fatalf("UsedBy = %v want %d", got.UsedBy, user.ID)
	}

	// Second consume must reject.
	err = ir.Consume(ctx, "X1", user.ID)
	if !errors.Is(err, domain.ErrInviteInvalid) {
		t.Fatalf("double-consume = %v want ErrInviteInvalid", err)
	}
}

func TestInviteRepo_Consume_Expired(t *testing.T) {
	tr, ir, admin, clk := inviteSetup(t)
	ctx := context.Background()

	past := clk.T.Add(-time.Hour)
	if err := ir.Insert(ctx, &domain.InviteCode{
		Code: "EXP", CreatedBy: admin.ID, ExpiresAt: &past,
	}); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	u := makeTenant("u@x.com")
	if err := tr.Insert(ctx, u); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	err := ir.Consume(ctx, "EXP", u.ID)
	if !errors.Is(err, domain.ErrInviteInvalid) {
		t.Fatalf("expired consume = %v want ErrInviteInvalid", err)
	}
}

func TestInviteRepo_ListByCreator(t *testing.T) {
	_, ir, admin, _ := inviteSetup(t)
	ctx := context.Background()
	for _, c := range []string{"A1", "A2", "A3"} {
		if err := ir.Insert(ctx, &domain.InviteCode{Code: c, CreatedBy: admin.ID}); err != nil {
			t.Fatalf("Insert %s: %v", c, err)
		}
	}
	got, err := ir.ListByCreator(ctx, admin.ID)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len = %d want 3", len(got))
	}
}

func TestInviteRepo_Insert_RejectsEmptyCode(t *testing.T) {
	_, ir, admin, _ := inviteSetup(t)
	err := ir.Insert(context.Background(), &domain.InviteCode{CreatedBy: admin.ID})
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("empty code = %v want ErrValidation", err)
	}
}

func TestInviteRepo_Delete(t *testing.T) {
	tr, ir, admin, _ := inviteSetup(t)
	ctx := context.Background()

	// Setup a second admin to prove cross-admin isolation.
	other := makeTenant("other@x.com")
	other.Role = domain.RoleAdmin
	if err := tr.Insert(ctx, other); err != nil {
		t.Fatalf("seed other admin: %v", err)
	}

	// 1. Delete unused invite owned by admin.
	inv := &domain.InviteCode{Code: "DELME-1", CreatedBy: admin.ID}
	if err := ir.Insert(ctx, inv); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if err := ir.Delete(ctx, "DELME-1", admin.ID); err != nil {
		t.Fatalf("Delete unused: %v", err)
	}
	if _, err := ir.Lock(ctx, "DELME-1"); !errors.Is(err, domain.ErrInviteInvalid) {
		t.Fatalf("post-delete Lock = %v want ErrInviteInvalid", err)
	}

	// 2. Delete already-consumed invite should refuse with ErrInviteInvalid.
	consumer := makeTenant("consumer@x.com")
	if err := tr.Insert(ctx, consumer); err != nil {
		t.Fatalf("seed consumer: %v", err)
	}
	used := &domain.InviteCode{Code: "USED-2", CreatedBy: admin.ID}
	if err := ir.Insert(ctx, used); err != nil {
		t.Fatalf("Insert used: %v", err)
	}
	if err := ir.Consume(ctx, "USED-2", consumer.ID); err != nil {
		t.Fatalf("Consume: %v", err)
	}
	if err := ir.Delete(ctx, "USED-2", admin.ID); !errors.Is(err, domain.ErrInviteInvalid) {
		t.Fatalf("Delete used = %v want ErrInviteInvalid", err)
	}

	// 3. Delete invite owned by another admin must report NotFound.
	other1 := &domain.InviteCode{Code: "OWNED-3", CreatedBy: other.ID}
	if err := ir.Insert(ctx, other1); err != nil {
		t.Fatalf("Insert other-owned: %v", err)
	}
	if err := ir.Delete(ctx, "OWNED-3", admin.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("cross-admin Delete = %v want ErrNotFound", err)
	}

	// 4. Delete missing code returns NotFound.
	if err := ir.Delete(ctx, "NOPE", admin.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing Delete = %v want ErrNotFound", err)
	}
}

func TestInviteRepo_CountByCreatorSince(t *testing.T) {
	tr, ir, admin, clk := inviteSetup(t)
	ctx := context.Background()

	// Pre-window: invite at clk - 25h. windowStart = today UTC midnight
	// computed from clk.T = 2026-05-17 12:00 UTC, so windowStart =
	// 2026-05-17 00:00 UTC.
	windowStart := time.Date(clk.T.Year(), clk.T.Month(), clk.T.Day(), 0, 0, 0, 0, time.UTC)

	// Insert a pre-window invite (clock at 25h before midnight).
	old := clk.T
	clk.T = windowStart.Add(-25 * time.Hour)
	if err := ir.Insert(ctx, &domain.InviteCode{Code: "OLD-1", CreatedBy: admin.ID}); err != nil {
		t.Fatalf("insert old: %v", err)
	}
	clk.T = old

	// Insert 3 in-window invites at the configured clock (12:00 UTC).
	for _, c := range []string{"NEW-1", "NEW-2", "NEW-3"} {
		if err := ir.Insert(ctx, &domain.InviteCode{Code: c, CreatedBy: admin.ID}); err != nil {
			t.Fatalf("insert %s: %v", c, err)
		}
	}

	got, err := ir.CountByCreatorSince(ctx, admin.ID, windowStart)
	if err != nil {
		t.Fatalf("CountByCreatorSince: %v", err)
	}
	if got != 3 {
		t.Fatalf("count = %d want 3 (older invite must be excluded)", got)
	}

	// A second admin's invites must not bleed in.
	other := makeTenant("other-admin@x.com")
	other.Role = domain.RoleAdmin
	if err := tr.Insert(ctx, other); err != nil {
		t.Fatalf("seed other: %v", err)
	}
	if err := ir.Insert(ctx, &domain.InviteCode{Code: "OTHER-1", CreatedBy: other.ID}); err != nil {
		t.Fatalf("insert other: %v", err)
	}
	got, err = ir.CountByCreatorSince(ctx, admin.ID, windowStart)
	if err != nil {
		t.Fatalf("CountByCreatorSince after other: %v", err)
	}
	if got != 3 {
		t.Fatalf("count = %d want 3 (other admin must not bleed in)", got)
	}
}

func TestInviteRepo_CountByCreatorSince_RejectsZeroAdmin(t *testing.T) {
	_, ir, _, _ := inviteSetup(t)
	_, err := ir.CountByCreatorSince(context.Background(), 0, time.Now())
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("zero admin err = %v want ErrValidation", err)
	}
}

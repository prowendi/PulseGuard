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

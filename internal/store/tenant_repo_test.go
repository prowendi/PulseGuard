package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wendi/pulseguard/internal/domain"
)

func fixedClock(t time.Time) domain.Clock { return &domain.FakeClock{T: t} }

func makeTenant(email string) *domain.Tenant {
	return &domain.Tenant{
		Email:        email,
		PasswordHash: "$2a$10$abcdefghijklmnopqrstuvxyz",
		DisplayName:  "Test " + email,
	}
}

func TestTenantRepo_InsertAndGet(t *testing.T) {
	db := newMigratedDB(t)
	ck := fixedClock(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	repo := NewTenantRepo(db, ck)
	ctx := context.Background()

	tn := makeTenant("a@b.com")
	if err := repo.Insert(ctx, tn); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if tn.ID == 0 {
		t.Fatalf("Insert did not populate ID")
	}
	if tn.Role != domain.RoleUser {
		t.Fatalf("default role = %q", tn.Role)
	}
	if tn.Status != domain.TenantActive {
		t.Fatalf("default status = %q", tn.Status)
	}

	got, err := repo.GetByID(ctx, tn.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Email != "a@b.com" {
		t.Fatalf("GetByID email = %q", got.Email)
	}

	got2, err := repo.GetByEmail(ctx, "a@b.com")
	if err != nil {
		t.Fatalf("GetByEmail: %v", err)
	}
	if got2.ID != tn.ID {
		t.Fatalf("GetByEmail id = %d want %d", got2.ID, tn.ID)
	}
}

func TestTenantRepo_GetByEmail_NotFound(t *testing.T) {
	db := newMigratedDB(t)
	repo := NewTenantRepo(db, fixedClock(time.Now()))
	_, err := repo.GetByEmail(context.Background(), "nope@x.com")
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("GetByEmail = %v, want ErrNotFound", err)
	}
}

func TestTenantRepo_GetByID_NotFound(t *testing.T) {
	db := newMigratedDB(t)
	repo := NewTenantRepo(db, fixedClock(time.Now()))
	_, err := repo.GetByID(context.Background(), 9999)
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("GetByID = %v, want ErrNotFound", err)
	}
}

func TestTenantRepo_UniqueEmail(t *testing.T) {
	db := newMigratedDB(t)
	repo := NewTenantRepo(db, fixedClock(time.Now()))
	ctx := context.Background()
	if err := repo.Insert(ctx, makeTenant("x@y.com")); err != nil {
		t.Fatalf("Insert#1: %v", err)
	}
	err := repo.Insert(ctx, makeTenant("x@y.com"))
	if err == nil {
		t.Fatalf("duplicate email should fail")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "unique") {
		t.Fatalf("duplicate error wrong: %v", err)
	}
}

func TestTenantRepo_CountActive(t *testing.T) {
	db := newMigratedDB(t)
	repo := NewTenantRepo(db, fixedClock(time.Now()))
	ctx := context.Background()
	for i, e := range []string{"a@x.com", "b@x.com", "c@x.com"} {
		tn := makeTenant(e)
		if i == 2 {
			tn.Status = domain.TenantDisabled
		}
		if err := repo.Insert(ctx, tn); err != nil {
			t.Fatalf("Insert %s: %v", e, err)
		}
	}
	got, err := repo.CountActive(ctx)
	if err != nil {
		t.Fatalf("CountActive: %v", err)
	}
	if got != 2 {
		t.Fatalf("CountActive = %d want 2", got)
	}
}

func TestTenantRepo_RejectsEmptyEmail(t *testing.T) {
	db := newMigratedDB(t)
	repo := NewTenantRepo(db, fixedClock(time.Now()))
	err := repo.Insert(context.Background(), &domain.Tenant{PasswordHash: "x"})
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("Insert empty email = %v want ErrValidation", err)
	}
}

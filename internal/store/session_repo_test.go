package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prowendi/PulseGuard/internal/domain"
)

func sessionSetup(t *testing.T) (*SessionRepo, *domain.Tenant, *domain.FakeClock) {
	t.Helper()
	db := newMigratedDB(t)
	clk := &domain.FakeClock{T: time.Date(2026, 5, 17, 0, 0, 0, 0, time.UTC)}
	tr := NewTenantRepo(db, clk)
	tn := makeTenant("session@x.com")
	if err := tr.Insert(context.Background(), tn); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	return NewSessionRepo(db, clk), tn, clk
}

func TestSessionRepo_InsertAndGet(t *testing.T) {
	sr, tn, clk := sessionSetup(t)
	ctx := context.Background()
	s := &domain.Session{
		ID:        "abc123",
		TenantID:  tn.ID,
		ExpiresAt: clk.T.Add(14 * 24 * time.Hour),
	}
	if err := sr.Insert(ctx, s); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if s.CreatedAt.IsZero() {
		t.Fatalf("CreatedAt unset")
	}

	got, err := sr.GetByID(ctx, "abc123")
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.TenantID != tn.ID {
		t.Fatalf("TenantID = %d want %d", got.TenantID, tn.ID)
	}
	if !got.ExpiresAt.Equal(s.ExpiresAt) {
		t.Fatalf("ExpiresAt = %v want %v", got.ExpiresAt, s.ExpiresAt)
	}
}

func TestSessionRepo_GetByID_NotFound(t *testing.T) {
	sr, _, _ := sessionSetup(t)
	_, err := sr.GetByID(context.Background(), "nope")
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("err = %v want ErrNotFound", err)
	}
}

func TestSessionRepo_Delete(t *testing.T) {
	sr, tn, clk := sessionSetup(t)
	ctx := context.Background()
	s := &domain.Session{ID: "todelete", TenantID: tn.ID, ExpiresAt: clk.T.Add(time.Hour)}
	if err := sr.Insert(ctx, s); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if err := sr.Delete(ctx, s.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	_, err := sr.GetByID(ctx, s.ID)
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("after delete err = %v", err)
	}
	// idempotent
	if err := sr.Delete(ctx, "nonexistent"); err != nil {
		t.Fatalf("Delete missing: %v", err)
	}
}

func TestSessionRepo_PurgeExpired(t *testing.T) {
	sr, tn, clk := sessionSetup(t)
	ctx := context.Background()

	old := &domain.Session{ID: "old", TenantID: tn.ID, ExpiresAt: clk.T.Add(-time.Hour)}
	fresh := &domain.Session{ID: "fresh", TenantID: tn.ID, ExpiresAt: clk.T.Add(time.Hour)}
	if err := sr.Insert(ctx, old); err != nil {
		t.Fatalf("seed old: %v", err)
	}
	if err := sr.Insert(ctx, fresh); err != nil {
		t.Fatalf("seed fresh: %v", err)
	}
	clk.Advance(0) // exercise the clock to make sure we still purge correctly

	n, err := sr.PurgeExpired(ctx, clk.T)
	if err != nil {
		t.Fatalf("PurgeExpired: %v", err)
	}
	if n != 1 {
		t.Fatalf("purged = %d want 1", n)
	}
	if _, err := sr.GetByID(ctx, "old"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("old still present: %v", err)
	}
	if _, err := sr.GetByID(ctx, "fresh"); err != nil {
		t.Fatalf("fresh missing: %v", err)
	}
}

func TestSessionRepo_RejectsEmptyID(t *testing.T) {
	sr, tn, clk := sessionSetup(t)
	err := sr.Insert(context.Background(), &domain.Session{
		TenantID: tn.ID, ExpiresAt: clk.T.Add(time.Hour),
	})
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("empty id err = %v want ErrValidation", err)
	}
}

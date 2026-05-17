package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wendi/pulseguard/internal/domain"
)

// TestSilenceRepo_InsertMatchDelete is the V7-3 happy-path: insert a
// silence with prefix "db01", confirm Match returns true for an alert
// fingerprint that begins with that prefix and false for one that
// doesn't, then delete and confirm Match goes back to false.
func TestSilenceRepo_InsertMatchDelete(t *testing.T) {
	f := newResourceFixture(t)
	repo := NewSilenceRepo(f.db, f.clk)
	now := f.clk.Now()
	s := &domain.Silence{
		TenantID:  f.tenant.ID,
		Pattern:   "db01",
		CreatedBy: "@alice",
		ExpiresAt: now.Add(time.Hour),
	}
	if err := repo.Insert(context.Background(), s); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if s.ID == 0 {
		t.Fatalf("Insert did not assign ID")
	}

	// Prefix match: "db01-cpu-high" begins with "db01".
	if hit, err := repo.Match(context.Background(), f.tenant.ID, "db01-cpu-high", now); err != nil || !hit {
		t.Fatalf("Match hit: %v %v", hit, err)
	}
	// Non-prefix: "web01" does not begin with "db01".
	if hit, err := repo.Match(context.Background(), f.tenant.ID, "web01-cpu", now); err != nil || hit {
		t.Fatalf("Match miss: %v %v", hit, err)
	}

	if err := repo.Delete(context.Background(), f.tenant.ID, s.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if hit, err := repo.Match(context.Background(), f.tenant.ID, "db01-cpu-high", now); err != nil || hit {
		t.Fatalf("Match after delete: %v %v", hit, err)
	}
}

// TestSilenceRepo_MatchSkipsExpired proves the expires_at gate: a row
// whose ExpiresAt is in the past must NOT match, even if its pattern
// is a perfect prefix. This is what gives /silence its time-windowed
// semantics — no operator needs to manually delete after the window.
func TestSilenceRepo_MatchSkipsExpired(t *testing.T) {
	f := newResourceFixture(t)
	repo := NewSilenceRepo(f.db, f.clk)
	now := f.clk.Now()
	// Insert a silence that expired one minute ago.
	if err := repo.Insert(context.Background(), &domain.Silence{
		TenantID:  f.tenant.ID,
		Pattern:   "db01",
		CreatedBy: "@a",
		ExpiresAt: now.Add(-time.Minute),
	}); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if hit, err := repo.Match(context.Background(), f.tenant.ID, "db01-cpu", now); err != nil || hit {
		t.Fatalf("expired silence must NOT match (hit=%v err=%v)", hit, err)
	}
}

// TestSilenceRepo_TenantIsolation makes sure one tenant's silence
// cannot suppress another tenant's alerts even when the fingerprints
// would otherwise match.
func TestSilenceRepo_TenantIsolation(t *testing.T) {
	f := newResourceFixture(t)
	repo := NewSilenceRepo(f.db, f.clk)
	now := f.clk.Now()
	if err := repo.Insert(context.Background(), &domain.Silence{
		TenantID: f.tenant.ID, Pattern: "db01",
		CreatedBy: "@a", ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	other := makeTenant("other@x.com")
	if err := f.tenants.Insert(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	if hit, err := repo.Match(context.Background(), other.ID, "db01-cpu", now); err != nil || hit {
		t.Fatalf("cross-tenant silence leak: %v %v", hit, err)
	}
}

// TestSilenceRepo_ListActiveOrderAndFiltering: ListActive returns only
// non-expired rows, ordered by ExpiresAt ASC.
func TestSilenceRepo_ListActiveOrderAndFiltering(t *testing.T) {
	f := newResourceFixture(t)
	repo := NewSilenceRepo(f.db, f.clk)
	now := f.clk.Now()
	// Three rows: A (already expired), B (lifts in 1h), C (lifts in 5m).
	rows := []*domain.Silence{
		{TenantID: f.tenant.ID, Pattern: "expired", CreatedBy: "@a", ExpiresAt: now.Add(-time.Minute)},
		{TenantID: f.tenant.ID, Pattern: "later", CreatedBy: "@a", ExpiresAt: now.Add(time.Hour)},
		{TenantID: f.tenant.ID, Pattern: "soon", CreatedBy: "@a", ExpiresAt: now.Add(5 * time.Minute)},
	}
	for _, r := range rows {
		if err := repo.Insert(context.Background(), r); err != nil {
			t.Fatal(err)
		}
	}
	list, err := repo.ListActive(context.Background(), f.tenant.ID, now)
	if err != nil {
		t.Fatalf("ListActive: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("expected 2 active (expired excluded), got %d", len(list))
	}
	if list[0].Pattern != "soon" || list[1].Pattern != "later" {
		t.Fatalf("order wrong: %v", []string{list[0].Pattern, list[1].Pattern})
	}
}

// TestSilenceRepo_DeleteByPattern: removes only rows whose pattern is
// an EXACT match (after TrimSpace); other patterns stay put.
func TestSilenceRepo_DeleteByPattern(t *testing.T) {
	f := newResourceFixture(t)
	repo := NewSilenceRepo(f.db, f.clk)
	now := f.clk.Now()
	for _, p := range []string{"db01", "db01", "web02"} {
		if err := repo.Insert(context.Background(), &domain.Silence{
			TenantID: f.tenant.ID, Pattern: p,
			CreatedBy: "@a", ExpiresAt: now.Add(time.Hour),
		}); err != nil {
			t.Fatal(err)
		}
	}
	n, err := repo.DeleteByPattern(context.Background(), f.tenant.ID, "db01")
	if err != nil {
		t.Fatalf("DeleteByPattern: %v", err)
	}
	if n != 2 {
		t.Fatalf("rows affected = %d, want 2", n)
	}
	remaining, err := repo.ListActive(context.Background(), f.tenant.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 1 || remaining[0].Pattern != "web02" {
		t.Fatalf("after DeleteByPattern db01: remaining=%+v", remaining)
	}
}

// TestSilenceRepo_DeleteByPattern_Empty: blank input is a no-op (does
// NOT mass-delete every silence in the tenant).
func TestSilenceRepo_DeleteByPattern_Empty(t *testing.T) {
	f := newResourceFixture(t)
	repo := NewSilenceRepo(f.db, f.clk)
	now := f.clk.Now()
	if err := repo.Insert(context.Background(), &domain.Silence{
		TenantID: f.tenant.ID, Pattern: "anything",
		CreatedBy: "@a", ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	n, err := repo.DeleteByPattern(context.Background(), f.tenant.ID, "   ")
	if err != nil || n != 0 {
		t.Fatalf("blank pattern must be no-op (n=%d err=%v)", n, err)
	}
}

// TestSilenceRepo_Delete_NotFound: missing id surfaces ErrNotFound so
// the Telegram listener can render a typo-friendly reply.
func TestSilenceRepo_Delete_NotFound(t *testing.T) {
	f := newResourceFixture(t)
	repo := NewSilenceRepo(f.db, f.clk)
	if err := repo.Delete(context.Background(), f.tenant.ID, 999); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// TestSilenceRepo_Insert_ValidationErrors guards every reject branch.
func TestSilenceRepo_Insert_ValidationErrors(t *testing.T) {
	f := newResourceFixture(t)
	repo := NewSilenceRepo(f.db, f.clk)
	now := f.clk.Now()
	cases := map[string]*domain.Silence{
		"zero tenant": {TenantID: 0, Pattern: "x", CreatedBy: "@a", ExpiresAt: now.Add(time.Hour)},
		"empty pat":   {TenantID: f.tenant.ID, Pattern: " ", CreatedBy: "@a", ExpiresAt: now.Add(time.Hour)},
		"empty by":    {TenantID: f.tenant.ID, Pattern: "x", CreatedBy: "", ExpiresAt: now.Add(time.Hour)},
		"zero exp":    {TenantID: f.tenant.ID, Pattern: "x", CreatedBy: "@a", ExpiresAt: time.Time{}},
	}
	for name, s := range cases {
		t.Run(name, func(t *testing.T) {
			if err := repo.Insert(context.Background(), s); err == nil {
				t.Fatalf("expected validation error for %s", name)
			}
		})
	}
}

// TestSilenceRepo_Match_EmptyInputs: zero tenant or empty fingerprint
// short-circuits to false without hitting SQL.
func TestSilenceRepo_Match_EmptyInputs(t *testing.T) {
	f := newResourceFixture(t)
	repo := NewSilenceRepo(f.db, f.clk)
	now := f.clk.Now()
	if hit, err := repo.Match(context.Background(), 0, "x", now); err != nil || hit {
		t.Fatalf("zero tenant: %v %v", hit, err)
	}
	if hit, err := repo.Match(context.Background(), f.tenant.ID, "  ", now); err != nil || hit {
		t.Fatalf("blank fp: %v %v", hit, err)
	}
}

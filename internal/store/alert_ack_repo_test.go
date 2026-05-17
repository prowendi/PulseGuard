package store

import (
	"context"
	"errors"
	"testing"

	"github.com/wendi/pulseguard/internal/domain"
)

// ─── AlertAckRepo ────────────────────────────────────────────────────

func TestAlertAckRepo_InsertAssignsID(t *testing.T) {
	f := newResourceFixture(t)
	bot := f.makeBot(t, "alpha", "1:secret")
	repo := NewAlertAckRepo(f.db, f.clk)
	a := &domain.AlertAck{
		TenantID:    f.tenant.ID,
		Fingerprint: "fp-1",
		AckedBy:     "@alice",
		BotID:       bot.ID,
		ChatID:      "100",
	}
	if err := repo.Insert(context.Background(), a); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if a.ID == 0 {
		t.Fatalf("Insert did not assign ID")
	}
	if a.AckedAt.IsZero() {
		t.Fatalf("AckedAt not set")
	}
}

func TestAlertAckRepo_InsertDuplicateReturnsErrAlreadyAcked(t *testing.T) {
	f := newResourceFixture(t)
	bot := f.makeBot(t, "alpha", "1:secret")
	repo := NewAlertAckRepo(f.db, f.clk)
	first := &domain.AlertAck{
		TenantID: f.tenant.ID, Fingerprint: "fp-dup",
		AckedBy: "@a", BotID: bot.ID, ChatID: "1",
	}
	if err := repo.Insert(context.Background(), first); err != nil {
		t.Fatalf("Insert 1: %v", err)
	}
	second := &domain.AlertAck{
		TenantID: f.tenant.ID, Fingerprint: "fp-dup",
		AckedBy: "@b", BotID: bot.ID, ChatID: "2",
	}
	err := repo.Insert(context.Background(), second)
	if !errors.Is(err, ErrAlreadyAcked) {
		t.Fatalf("err = %v, want ErrAlreadyAcked", err)
	}
}

func TestAlertAckRepo_GetByFingerprint(t *testing.T) {
	f := newResourceFixture(t)
	bot := f.makeBot(t, "alpha", "1:secret")
	repo := NewAlertAckRepo(f.db, f.clk)
	a := &domain.AlertAck{
		TenantID: f.tenant.ID, Fingerprint: "fp-get",
		AckedBy: "@a", BotID: bot.ID, ChatID: "1",
	}
	if err := repo.Insert(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	got, err := repo.GetByFingerprint(context.Background(), f.tenant.ID, "fp-get")
	if err != nil {
		t.Fatalf("GetByFingerprint: %v", err)
	}
	if got.ID != a.ID || got.AckedBy != "@a" {
		t.Fatalf("mismatch: %+v", got)
	}
	if _, err := repo.GetByFingerprint(context.Background(), f.tenant.ID, "missing"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing err = %v, want ErrNotFound", err)
	}
}

func TestAlertAckRepo_GetByFingerprint_TenantIsolation(t *testing.T) {
	f := newResourceFixture(t)
	bot := f.makeBot(t, "alpha", "1:secret")
	repo := NewAlertAckRepo(f.db, f.clk)
	if err := repo.Insert(context.Background(), &domain.AlertAck{
		TenantID: f.tenant.ID, Fingerprint: "x",
		AckedBy: "@a", BotID: bot.ID, ChatID: "1",
	}); err != nil {
		t.Fatal(err)
	}
	tn2 := makeTenant("other@x.com")
	if err := f.tenants.Insert(context.Background(), tn2); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.GetByFingerprint(context.Background(), tn2.ID, "x"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("cross-tenant err = %v, want ErrNotFound", err)
	}
}

func TestAlertAckRepo_ListByTenantOrder(t *testing.T) {
	f := newResourceFixture(t)
	bot := f.makeBot(t, "alpha", "1:secret")
	repo := NewAlertAckRepo(f.db, f.clk)
	// Three rows, advancing clock so acked_at differs.
	for _, fp := range []string{"fp1", "fp2", "fp3"} {
		if err := repo.Insert(context.Background(), &domain.AlertAck{
			TenantID: f.tenant.ID, Fingerprint: fp,
			AckedBy: "@a", BotID: bot.ID, ChatID: "1",
		}); err != nil {
			t.Fatal(err)
		}
		f.clk.T = f.clk.T.Add(1_000_000_000) // +1s
	}
	rows, err := repo.ListByTenant(context.Background(), f.tenant.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("want 3 rows, got %d", len(rows))
	}
	// Order is newest-first.
	if rows[0].Fingerprint != "fp3" || rows[2].Fingerprint != "fp1" {
		t.Fatalf("order wrong: %s,%s,%s",
			rows[0].Fingerprint, rows[1].Fingerprint, rows[2].Fingerprint)
	}
}

func TestAlertAckRepo_Insert_ValidationErrors(t *testing.T) {
	f := newResourceFixture(t)
	bot := f.makeBot(t, "alpha", "1:secret")
	repo := NewAlertAckRepo(f.db, f.clk)
	cases := map[string]*domain.AlertAck{
		"zero tenant": {TenantID: 0, Fingerprint: "x", BotID: bot.ID, ChatID: "1"},
		"empty fp":    {TenantID: f.tenant.ID, Fingerprint: " ", BotID: bot.ID, ChatID: "1"},
		"zero bot":    {TenantID: f.tenant.ID, Fingerprint: "x", BotID: 0, ChatID: "1"},
		"empty chat":  {TenantID: f.tenant.ID, Fingerprint: "x", BotID: bot.ID, ChatID: ""},
	}
	for name, a := range cases {
		t.Run(name, func(t *testing.T) {
			if err := repo.Insert(context.Background(), a); err == nil {
				t.Fatalf("expected validation error for %s", name)
			}
		})
	}
}

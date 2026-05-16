package pipeline

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wendi/pulseguard/internal/config"
	"github.com/wendi/pulseguard/internal/domain"
	"github.com/wendi/pulseguard/internal/store"
)

func TestFingerprintExplicitKey(t *testing.T) {
	fp := Fingerprint(map[string]any{"a": 1}, "user-supplied")
	if fp != "user-supplied" {
		t.Fatalf("got %q", fp)
	}
}

func TestFingerprintStableForSamePayload(t *testing.T) {
	p1 := map[string]any{"x": 1, "y": "z", "nest": map[string]any{"b": 2, "a": 1}}
	p2 := map[string]any{"y": "z", "nest": map[string]any{"a": 1, "b": 2}, "x": 1}
	fp1 := Fingerprint(p1, "")
	fp2 := Fingerprint(p2, "")
	if fp1 != fp2 {
		t.Fatalf("fingerprints differ: %q vs %q", fp1, fp2)
	}
	if len(fp1) != 64 { // sha256 hex
		t.Fatalf("unexpected fp length %d", len(fp1))
	}
}

func TestFingerprintDifferentPayload(t *testing.T) {
	p1 := map[string]any{"x": 1}
	p2 := map[string]any{"x": 2}
	if Fingerprint(p1, "") == Fingerprint(p2, "") {
		t.Fatalf("expected different fingerprints")
	}
}

func TestFingerprintEmpty(t *testing.T) {
	fp := Fingerprint(nil, "")
	if fp == "" {
		t.Fatalf("expected non-empty fp for empty map")
	}
}

// seedFullStack does a minimal tenant + bot + template + channel chain so
// the FK constraints on push_outbox are satisfied.
func seedFullStack(t *testing.T, dbPath string, dedupWindow int) (*domain.Channel, *store.OutboxRepo, *store.DedupRepo, *domain.FakeClock) {
	t.Helper()
	dbcfg := config.Database{Path: dbPath, BusyTimeout: config.Duration(5 * time.Second)}
	db, err := store.Open(dbcfg)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	clk := &domain.FakeClock{T: time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)}
	if err := store.Migrate(context.Background(), db, clk); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	cipher, err := store.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	tenants := store.NewTenantRepo(db, clk)
	bots := store.NewBotRepo(db, clk, cipher)
	tpls := store.NewTemplateRepo(db, clk)
	chans := store.NewChannelRepo(db, clk)
	outbox := store.NewOutboxRepo(db, clk)
	dedup := store.NewDedupRepo(db)

	tenant := &domain.Tenant{Email: "owner@example.com", PasswordHash: "x", Role: domain.RoleUser}
	if err := tenants.Insert(context.Background(), tenant); err != nil {
		t.Fatalf("insert tenant: %v", err)
	}
	bot := &domain.Bot{TenantID: tenant.ID, Name: "b", BotToken: "TOKEN"}
	if err := bots.Insert(context.Background(), bot); err != nil {
		t.Fatalf("insert bot: %v", err)
	}
	tpl := &domain.Template{TenantID: tenant.ID, Name: "t", ParseMode: domain.ParseMarkdownV2, Body: "hi"}
	if err := tpls.Insert(context.Background(), tpl); err != nil {
		t.Fatalf("insert tpl: %v", err)
	}
	ch := &domain.Channel{
		TenantID: tenant.ID, Name: "ch", PushToken: "tok",
		BotID: bot.ID, ChatID: "12345",
		RatePerMin: 60, DedupWindowS: dedupWindow, Enabled: true,
		Templates: []*domain.ChannelTemplate{{TemplateID: tpl.ID, IsDefault: true}},
	}
	if err := chans.Insert(context.Background(), ch); err != nil {
		t.Fatalf("insert channel: %v", err)
	}
	return ch, outbox, dedup, clk
}

func TestIngestNoDedup(t *testing.T) {
	dir := t.TempDir()
	ch, outbox, dedup, clk := seedFullStack(t, filepath.Join(dir, "t.db"), 0)
	ing := NewIngestor(outbox, NewDedup(dedup, clk), clk)
	id, dropped, err := ing.Ingest(context.Background(), ch, map[string]any{"a": 1}, "")
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if dropped {
		t.Fatalf("should not drop when dedup disabled")
	}
	if id == 0 {
		t.Fatalf("no row inserted")
	}
}

func TestIngestDedupFirstSeen(t *testing.T) {
	dir := t.TempDir()
	ch, outbox, dedup, clk := seedFullStack(t, filepath.Join(dir, "t.db"), 60)
	ing := NewIngestor(outbox, NewDedup(dedup, clk), clk)
	id, dropped, err := ing.Ingest(context.Background(), ch, map[string]any{"x": 1}, "")
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if dropped || id == 0 {
		t.Fatalf("first push must not drop: id=%d dropped=%v", id, dropped)
	}
}

func TestIngestDedupRepeated(t *testing.T) {
	dir := t.TempDir()
	ch, outbox, dedup, clk := seedFullStack(t, filepath.Join(dir, "t.db"), 60)
	ing := NewIngestor(outbox, NewDedup(dedup, clk), clk)

	if _, _, err := ing.Ingest(context.Background(), ch, map[string]any{"x": 1}, ""); err != nil {
		t.Fatalf("first: %v", err)
	}
	id, dropped, err := ing.Ingest(context.Background(), ch, map[string]any{"x": 1}, "")
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if !dropped {
		t.Fatalf("expected drop")
	}
	if id != 0 {
		t.Fatalf("dropped push must not insert outbox row, got id=%d", id)
	}
}

func TestIngestDedupExpiryAllowsRepeat(t *testing.T) {
	dir := t.TempDir()
	ch, outbox, dedup, clk := seedFullStack(t, filepath.Join(dir, "t.db"), 10)
	ing := NewIngestor(outbox, NewDedup(dedup, clk), clk)

	if _, _, err := ing.Ingest(context.Background(), ch, map[string]any{"x": 1}, ""); err != nil {
		t.Fatalf("first: %v", err)
	}
	clk.Advance(30 * time.Second) // beyond 10s window
	id, dropped, err := ing.Ingest(context.Background(), ch, map[string]any{"x": 1}, "")
	if err != nil {
		t.Fatalf("third: %v", err)
	}
	if dropped {
		t.Fatalf("post-expiry should not drop")
	}
	if id == 0 {
		t.Fatalf("expected new insertion")
	}
}

func TestIngestExplicitDedupKey(t *testing.T) {
	dir := t.TempDir()
	ch, outbox, dedup, clk := seedFullStack(t, filepath.Join(dir, "t.db"), 60)
	ing := NewIngestor(outbox, NewDedup(dedup, clk), clk)

	if _, _, err := ing.Ingest(context.Background(), ch, map[string]any{"v": 1}, "cpu_db01"); err != nil {
		t.Fatalf("first: %v", err)
	}
	// Same key, different payload -> still drops because key wins.
	_, dropped, err := ing.Ingest(context.Background(), ch, map[string]any{"v": 999}, "cpu_db01")
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if !dropped {
		t.Fatalf("explicit key should drop")
	}
}

func TestIngestNilChannel(t *testing.T) {
	dir := t.TempDir()
	_, outbox, dedup, clk := seedFullStack(t, filepath.Join(dir, "t.db"), 0)
	ing := NewIngestor(outbox, NewDedup(dedup, clk), clk)
	_, _, err := ing.Ingest(context.Background(), nil, nil, "")
	if err == nil {
		t.Fatalf("expected error for nil channel")
	}
	if !strings.Contains(err.Error(), "channel") {
		t.Fatalf("got %v", err)
	}
}

package store

import (
	"context"
	"errors"
	"testing"

	"github.com/wendi/pulseguard/internal/domain"
)

// ─── CommandRepo ────────────────────────────────────────────────────

func newCommandFixture(t *testing.T) (*resourceFixture, *CommandRepo) {
	t.Helper()
	f := newResourceFixture(t)
	return f, NewCommandRepo(f.db, f.clk)
}

// makeCommand builds a command bound to the supplied bot. Per-bot
// scoping (2026-05): a non-zero BotID is now mandatory for Insert.
func makeCommand(tenantID, botID int64, name string) *domain.Command {
	return &domain.Command{
		TenantID:    tenantID,
		BotID:       botID,
		Name:        name,
		Description: "demo",
		Code:        `def handle(args): return "ok"`,
		Enabled:     true,
	}
}

func TestCommandRepo_InsertAssignsID(t *testing.T) {
	f, repo := newCommandFixture(t)
	bot := f.makeBot(t, "alpha", "1:secret")
	c := makeCommand(f.tenant.ID, bot.ID, "/查询")
	if err := repo.Insert(context.Background(), c); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if c.ID == 0 {
		t.Fatalf("Insert did not assign ID")
	}
	if c.CreatedAt.IsZero() || c.UpdatedAt.IsZero() {
		t.Fatalf("timestamps not set")
	}
}

func TestCommandRepo_InsertUniqueWithinBot(t *testing.T) {
	f, repo := newCommandFixture(t)
	bot := f.makeBot(t, "alpha", "1:secret")
	c1 := makeCommand(f.tenant.ID, bot.ID, "/查询")
	if err := repo.Insert(context.Background(), c1); err != nil {
		t.Fatalf("Insert 1: %v", err)
	}
	c2 := makeCommand(f.tenant.ID, bot.ID, "/查询")
	err := repo.Insert(context.Background(), c2)
	if err == nil {
		t.Fatalf("expected unique constraint error on duplicate (bot, name)")
	}
}

// TestCommandRepo_SameNameDifferentBotsAllowed pins the new per-bot
// scoping: /查询 on bot A and /查询 on bot B coexist without colliding.
func TestCommandRepo_SameNameDifferentBotsAllowed(t *testing.T) {
	f, repo := newCommandFixture(t)
	botA := f.makeBot(t, "alpha", "1:secret")
	botB := f.makeBot(t, "beta", "2:secret")
	if err := repo.Insert(context.Background(), makeCommand(f.tenant.ID, botA.ID, "/查询")); err != nil {
		t.Fatalf("Insert on bot A: %v", err)
	}
	if err := repo.Insert(context.Background(), makeCommand(f.tenant.ID, botB.ID, "/查询")); err != nil {
		t.Fatalf("Insert on bot B (same name): %v", err)
	}
}

// TestCommandRepo_RejectsCrossTenantBot guards the ensureBotOwnership
// check: attaching a command to a bot owned by a different tenant
// must fail loudly even if the FK alone would allow it.
func TestCommandRepo_RejectsCrossTenantBot(t *testing.T) {
	f, repo := newCommandFixture(t)
	tn2 := makeTenant("other@x.com")
	if err := f.tenants.Insert(context.Background(), tn2); err != nil {
		t.Fatalf("seed tenant 2: %v", err)
	}
	otherBot := &domain.Bot{TenantID: tn2.ID, Name: "other", BotToken: "9:secret"}
	if err := f.bots.Insert(context.Background(), otherBot); err != nil {
		t.Fatalf("seed bot for tenant 2: %v", err)
	}
	c := makeCommand(f.tenant.ID, otherBot.ID, "/x")
	err := repo.Insert(context.Background(), c)
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("cross-tenant bot id should reject as ErrValidation, got %v", err)
	}
}

func TestCommandRepo_GetByID(t *testing.T) {
	f, repo := newCommandFixture(t)
	bot := f.makeBot(t, "alpha", "1:secret")
	c := makeCommand(f.tenant.ID, bot.ID, "/echo")
	if err := repo.Insert(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	got, err := repo.GetByID(context.Background(), f.tenant.ID, c.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Name != "/echo" || got.Code != c.Code {
		t.Fatalf("mismatch: %+v", got)
	}
	if got.BotID != bot.ID {
		t.Fatalf("BotID not hydrated: got %d want %d", got.BotID, bot.ID)
	}
}

func TestCommandRepo_GetByID_TenantIsolation(t *testing.T) {
	f, repo := newCommandFixture(t)
	bot := f.makeBot(t, "alpha", "1:secret")
	c := makeCommand(f.tenant.ID, bot.ID, "/echo")
	if err := repo.Insert(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	// Seed a second tenant. Their query must miss.
	tn2 := makeTenant("other@x.com")
	if err := f.tenants.Insert(context.Background(), tn2); err != nil {
		t.Fatalf("seed tenant 2: %v", err)
	}
	if _, err := repo.GetByID(context.Background(), tn2.ID, c.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("expected ErrNotFound across tenants, got %v", err)
	}
}

func TestCommandRepo_GetByTenantAndName(t *testing.T) {
	f, repo := newCommandFixture(t)
	bot := f.makeBot(t, "alpha", "1:secret")
	c := makeCommand(f.tenant.ID, bot.ID, "/查询")
	if err := repo.Insert(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	got, err := repo.GetByTenantAndName(context.Background(), f.tenant.ID, "/查询")
	if err != nil {
		t.Fatalf("GetByTenantAndName: %v", err)
	}
	if got.ID != c.ID {
		t.Fatalf("ID mismatch")
	}
	// missing name
	if _, err := repo.GetByTenantAndName(context.Background(), f.tenant.ID, "/missing"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("expected ErrNotFound for missing name, got %v", err)
	}
}

func TestCommandRepo_GetByBotAndName(t *testing.T) {
	f, repo := newCommandFixture(t)
	bot := f.makeBot(t, "alpha", "1:secret")
	c := makeCommand(f.tenant.ID, bot.ID, "/查询")
	if err := repo.Insert(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	got, err := repo.GetByBotAndName(context.Background(), bot.ID, "/查询")
	if err != nil {
		t.Fatalf("GetByBotAndName: %v", err)
	}
	if got.ID != c.ID {
		t.Fatalf("ID mismatch: %d vs %d", got.ID, c.ID)
	}
}

func TestCommandRepo_GetByBotAndName_DisabledHidden(t *testing.T) {
	f, repo := newCommandFixture(t)
	bot := f.makeBot(t, "alpha", "1:secret")
	c := makeCommand(f.tenant.ID, bot.ID, "/查询")
	c.Enabled = false
	if err := repo.Insert(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	_, err := repo.GetByBotAndName(context.Background(), bot.ID, "/查询")
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("disabled command should be hidden, got err=%v", err)
	}
}

// TestCommandRepo_GetByBotAndName_OnlyOwnBot guards the per-bot
// scoping: a command bound to bot A must NOT be visible when the
// dispatcher resolves through bot B, even within the same tenant.
func TestCommandRepo_GetByBotAndName_OnlyOwnBot(t *testing.T) {
	f, repo := newCommandFixture(t)
	botA := f.makeBot(t, "alpha", "1:secret")
	botB := f.makeBot(t, "beta", "2:secret")
	c := makeCommand(f.tenant.ID, botA.ID, "/查询")
	if err := repo.Insert(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	// Resolving via the sibling bot must miss.
	_, err := repo.GetByBotAndName(context.Background(), botB.ID, "/查询")
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("cross-bot lookup should miss, got err=%v", err)
	}
}

func TestCommandRepo_GetByBotAndName_CrossTenant(t *testing.T) {
	f, repo := newCommandFixture(t)
	bot := f.makeBot(t, "alpha", "1:secret")
	c := makeCommand(f.tenant.ID, bot.ID, "/查询")
	if err := repo.Insert(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	// bot belongs to tenant 2
	tn2 := makeTenant("other@x.com")
	if err := f.tenants.Insert(context.Background(), tn2); err != nil {
		t.Fatal(err)
	}
	bot2 := &domain.Bot{TenantID: tn2.ID, Name: "other", BotToken: "2:secret"}
	if err := f.bots.Insert(context.Background(), bot2); err != nil {
		t.Fatal(err)
	}
	_, err := repo.GetByBotAndName(context.Background(), bot2.ID, "/查询")
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("cross-tenant lookup should miss, got err=%v", err)
	}
}

func TestCommandRepo_Update(t *testing.T) {
	f, repo := newCommandFixture(t)
	bot := f.makeBot(t, "alpha", "1:secret")
	c := makeCommand(f.tenant.ID, bot.ID, "/v1")
	if err := repo.Insert(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	c.Name = "/v2"
	c.Code = `def handle(args): return "v2"`
	c.Enabled = false
	if err := repo.Update(context.Background(), c); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, _ := repo.GetByID(context.Background(), f.tenant.ID, c.ID)
	if got.Name != "/v2" || got.Code != `def handle(args): return "v2"` || got.Enabled {
		t.Fatalf("update mismatch: %+v", got)
	}
}

func TestCommandRepo_Delete(t *testing.T) {
	f, repo := newCommandFixture(t)
	bot := f.makeBot(t, "alpha", "1:secret")
	c := makeCommand(f.tenant.ID, bot.ID, "/k")
	if err := repo.Insert(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if err := repo.Delete(context.Background(), f.tenant.ID, c.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := repo.GetByID(context.Background(), f.tenant.ID, c.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("expected ErrNotFound after delete, got %v", err)
	}
}

func TestCommandRepo_Delete_CrossTenant(t *testing.T) {
	f, repo := newCommandFixture(t)
	bot := f.makeBot(t, "alpha", "1:secret")
	c := makeCommand(f.tenant.ID, bot.ID, "/k")
	if err := repo.Insert(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	err := repo.Delete(context.Background(), 9999, c.ID)
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("delete cross-tenant err = %v, want ErrNotFound", err)
	}
}

func TestCommandRepo_ListByTenant(t *testing.T) {
	f, repo := newCommandFixture(t)
	bot := f.makeBot(t, "alpha", "1:secret")
	for _, n := range []string{"/a", "/b", "/c"} {
		if err := repo.Insert(context.Background(), makeCommand(f.tenant.ID, bot.ID, n)); err != nil {
			t.Fatal(err)
		}
	}
	out, err := repo.ListByTenant(context.Background(), f.tenant.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 3 {
		t.Fatalf("want 3 items, got %d", len(out))
	}
}

func TestCommandRepo_Insert_ValidationErrors(t *testing.T) {
	f, repo := newCommandFixture(t)
	bot := f.makeBot(t, "alpha", "1:secret")
	cases := map[string]*domain.Command{
		"zero tenant": {TenantID: 0, BotID: bot.ID, Name: "/x", Code: "x"},
		"zero bot":    {TenantID: f.tenant.ID, BotID: 0, Name: "/x", Code: "x"},
		"empty name":  {TenantID: f.tenant.ID, BotID: bot.ID, Name: "", Code: "x"},
		"empty code":  {TenantID: f.tenant.ID, BotID: bot.ID, Name: "/x", Code: ""},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if err := repo.Insert(context.Background(), c); err == nil {
				t.Fatalf("expected validation error for %s", name)
			}
		})
	}
}

// ─── SubscriberRepo ─────────────────────────────────────────────────

func TestSubscriberRepo_UpsertNewRow(t *testing.T) {
	f, cmdRepo := newCommandFixture(t)
	bot := f.makeBot(t, "alpha", "1:secret")
	c := makeCommand(f.tenant.ID, bot.ID, "/echo")
	if err := cmdRepo.Insert(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	subRepo := NewSubscriberRepo(f.db, f.clk)
	s := &domain.Subscriber{
		TenantID: f.tenant.ID, CommandID: c.ID, BotID: bot.ID,
		ChatID: "1001", Platform: domain.PlatformTelegram,
	}
	if err := subRepo.Upsert(context.Background(), s); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if s.ID == 0 {
		t.Fatalf("Upsert did not assign id")
	}
	if s.CreatedAt.IsZero() || s.LastSeenAt.IsZero() {
		t.Fatalf("timestamps not set")
	}
}

func TestSubscriberRepo_UpsertIdempotent(t *testing.T) {
	f, cmdRepo := newCommandFixture(t)
	bot := f.makeBot(t, "alpha", "1:secret")
	c := makeCommand(f.tenant.ID, bot.ID, "/echo")
	if err := cmdRepo.Insert(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	subRepo := NewSubscriberRepo(f.db, f.clk)
	first := &domain.Subscriber{
		TenantID: f.tenant.ID, CommandID: c.ID, BotID: bot.ID,
		ChatID: "1001", Platform: domain.PlatformTelegram,
	}
	if err := subRepo.Upsert(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	firstID := first.ID

	// Advance clock so last_seen_at must differ.
	f.clk.T = f.clk.T.Add(60_000_000_000) // +60s

	second := &domain.Subscriber{
		TenantID: f.tenant.ID, CommandID: c.ID, BotID: bot.ID,
		ChatID: "1001", Platform: domain.PlatformTelegram,
	}
	if err := subRepo.Upsert(context.Background(), second); err != nil {
		t.Fatalf("Upsert 2: %v", err)
	}
	if second.ID != firstID {
		t.Fatalf("Upsert created new row (id=%d) instead of reusing %d", second.ID, firstID)
	}
	if !second.LastSeenAt.After(first.LastSeenAt) {
		t.Fatalf("last_seen_at not bumped: first=%v second=%v", first.LastSeenAt, second.LastSeenAt)
	}

	// And there should still be exactly one row.
	rows, err := subRepo.ListByCommand(context.Background(), f.tenant.ID, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 row after idempotent upsert, got %d", len(rows))
	}
}

func TestSubscriberRepo_DefaultPlatform(t *testing.T) {
	f, cmdRepo := newCommandFixture(t)
	bot := f.makeBot(t, "alpha", "1:secret")
	c := makeCommand(f.tenant.ID, bot.ID, "/echo")
	if err := cmdRepo.Insert(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	subRepo := NewSubscriberRepo(f.db, f.clk)
	s := &domain.Subscriber{
		TenantID: f.tenant.ID, CommandID: c.ID, BotID: bot.ID,
		ChatID: "1001",
		// Platform left empty.
	}
	if err := subRepo.Upsert(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if s.Platform != domain.PlatformTelegram {
		t.Fatalf("default platform = %q, want %q", s.Platform, domain.PlatformTelegram)
	}
}

func TestSubscriberRepo_ListByTenant(t *testing.T) {
	f, cmdRepo := newCommandFixture(t)
	bot := f.makeBot(t, "alpha", "1:secret")
	c := makeCommand(f.tenant.ID, bot.ID, "/echo")
	if err := cmdRepo.Insert(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	subRepo := NewSubscriberRepo(f.db, f.clk)
	for _, chat := range []string{"1001", "1002", "1003"} {
		if err := subRepo.Upsert(context.Background(), &domain.Subscriber{
			TenantID: f.tenant.ID, CommandID: c.ID, BotID: bot.ID, ChatID: chat,
		}); err != nil {
			t.Fatal(err)
		}
	}
	out, err := subRepo.ListByTenant(context.Background(), f.tenant.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 3 {
		t.Fatalf("want 3, got %d", len(out))
	}
}

func TestSubscriberRepo_Delete_TenantGuard(t *testing.T) {
	f, cmdRepo := newCommandFixture(t)
	bot := f.makeBot(t, "alpha", "1:secret")
	c := makeCommand(f.tenant.ID, bot.ID, "/echo")
	if err := cmdRepo.Insert(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	subRepo := NewSubscriberRepo(f.db, f.clk)
	s := &domain.Subscriber{
		TenantID: f.tenant.ID, CommandID: c.ID, BotID: bot.ID, ChatID: "1001",
	}
	if err := subRepo.Upsert(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	// cross-tenant delete fails
	if err := subRepo.Delete(context.Background(), 9999, s.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("cross-tenant delete err = %v want ErrNotFound", err)
	}
	// same-tenant delete OK
	if err := subRepo.Delete(context.Background(), f.tenant.ID, s.ID); err != nil {
		t.Fatalf("delete err: %v", err)
	}
}

func TestSubscriberRepo_CommandDeleteCascades(t *testing.T) {
	f, cmdRepo := newCommandFixture(t)
	bot := f.makeBot(t, "alpha", "1:secret")
	c := makeCommand(f.tenant.ID, bot.ID, "/echo")
	if err := cmdRepo.Insert(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	subRepo := NewSubscriberRepo(f.db, f.clk)
	if err := subRepo.Upsert(context.Background(), &domain.Subscriber{
		TenantID: f.tenant.ID, CommandID: c.ID, BotID: bot.ID, ChatID: "1001",
	}); err != nil {
		t.Fatal(err)
	}

	// Delete the command — subscribers should cascade.
	if err := cmdRepo.Delete(context.Background(), f.tenant.ID, c.ID); err != nil {
		t.Fatal(err)
	}
	rows, err := subRepo.ListByTenant(context.Background(), f.tenant.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("subscribers should be cascade-deleted, got %d rows", len(rows))
	}
}

// ─── DeleteByChatAndCommand (V6-4 /unsubscribe path) ──────────────

func TestSubscriberRepo_DeleteByChatAndCommandSlashName(t *testing.T) {
	f, cmdRepo := newCommandFixture(t)
	bot := f.makeBot(t, "alpha", "1:secret")
	c := makeCommand(f.tenant.ID, bot.ID, "/echo")
	if err := cmdRepo.Insert(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	subRepo := NewSubscriberRepo(f.db, f.clk)
	if err := subRepo.Upsert(context.Background(), &domain.Subscriber{
		TenantID: f.tenant.ID, CommandID: c.ID, BotID: bot.ID, ChatID: "9001",
	}); err != nil {
		t.Fatal(err)
	}
	// Caller passes the bare name (no leading slash) — must match
	// against the slash-shaped catalog entry.
	if err := subRepo.DeleteByChatAndCommand(context.Background(), bot.ID, "9001", "echo"); err != nil {
		t.Fatalf("DeleteByChatAndCommand: %v", err)
	}
	rows, _ := subRepo.ListByTenant(context.Background(), f.tenant.ID)
	if len(rows) != 0 {
		t.Fatalf("subscriber not deleted: %d rows", len(rows))
	}
}

func TestSubscriberRepo_DeleteByChatAndCommandBareName(t *testing.T) {
	f, cmdRepo := newCommandFixture(t)
	bot := f.makeBot(t, "alpha", "1:secret")
	c := makeCommand(f.tenant.ID, bot.ID, "查询") // stored without leading slash
	if err := cmdRepo.Insert(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	subRepo := NewSubscriberRepo(f.db, f.clk)
	if err := subRepo.Upsert(context.Background(), &domain.Subscriber{
		TenantID: f.tenant.ID, CommandID: c.ID, BotID: bot.ID, ChatID: "9002",
	}); err != nil {
		t.Fatal(err)
	}
	// Caller passes the slash-prefixed name — must still find the
	// bare-shaped catalog entry.
	if err := subRepo.DeleteByChatAndCommand(context.Background(), bot.ID, "9002", "/查询"); err != nil {
		t.Fatalf("DeleteByChatAndCommand: %v", err)
	}
}

func TestSubscriberRepo_DeleteByChatAndCommandNotFound(t *testing.T) {
	f, _ := newCommandFixture(t)
	bot := f.makeBot(t, "alpha", "1:secret")
	subRepo := NewSubscriberRepo(f.db, f.clk)
	err := subRepo.DeleteByChatAndCommand(context.Background(), bot.ID, "9003", "ghost")
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestSubscriberRepo_DeleteByChatAndCommandCrossBotIgnored(t *testing.T) {
	f, cmdRepo := newCommandFixture(t)
	bot := f.makeBot(t, "alpha", "1:secret")
	c := makeCommand(f.tenant.ID, bot.ID, "/echo")
	if err := cmdRepo.Insert(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	subRepo := NewSubscriberRepo(f.db, f.clk)
	if err := subRepo.Upsert(context.Background(), &domain.Subscriber{
		TenantID: f.tenant.ID, CommandID: c.ID, BotID: bot.ID, ChatID: "9004",
	}); err != nil {
		t.Fatal(err)
	}
	// Use a bot id that does NOT exist — the JOIN drops the
	// subscriber out of scope so the delete must miss.
	if err := subRepo.DeleteByChatAndCommand(context.Background(), 99999, "9004", "echo"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("cross-bot delete err = %v, want ErrNotFound", err)
	}
	rows, _ := subRepo.ListByTenant(context.Background(), f.tenant.ID)
	if len(rows) != 1 {
		t.Fatalf("subscriber should still exist, got %d rows", len(rows))
	}
}

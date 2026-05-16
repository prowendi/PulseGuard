package store

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"strings"
	"testing"
	"time"

	"github.com/wendi/pulseguard/internal/config"
	"github.com/wendi/pulseguard/internal/domain"
)

// resourceFixture sets up tenant + bot + template + channel for tests that
// need the full chain.
type resourceFixture struct {
	db       *sql.DB
	clk      *domain.FakeClock
	cipher   *Cipher
	tenant   *domain.Tenant
	tenants  *TenantRepo
	bots     *BotRepo
	tpls     *TemplateRepo
	channels *ChannelRepo
}

func newResourceFixture(t *testing.T) *resourceFixture {
	t.Helper()
	db := newMigratedDB(t)
	clk := &domain.FakeClock{T: time.Date(2026, 5, 17, 10, 0, 0, 0, time.UTC)}
	cipher, err := NewCipher(makeKeyB64(t))
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	tr := NewTenantRepo(db, clk)
	tn := makeTenant("owner@x.com")
	if err := tr.Insert(context.Background(), tn); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	return &resourceFixture{
		db:       db,
		clk:      clk,
		cipher:   cipher,
		tenant:   tn,
		tenants:  tr,
		bots:     NewBotRepo(db, clk, cipher),
		tpls:     NewTemplateRepo(db, clk),
		channels: NewChannelRepo(db, clk),
	}
}

func (f *resourceFixture) makeBot(t *testing.T, name, token string) *domain.Bot {
	t.Helper()
	b := &domain.Bot{TenantID: f.tenant.ID, Name: name, BotToken: token, Description: "n/a"}
	if err := f.bots.Insert(context.Background(), b); err != nil {
		t.Fatalf("insert bot: %v", err)
	}
	return b
}

func (f *resourceFixture) makeTemplate(t *testing.T, name string) *domain.Template {
	t.Helper()
	tpl := &domain.Template{
		TenantID: f.tenant.ID, Name: name,
		ParseMode: domain.ParseMarkdownV2, Body: "Hello {{.who}}",
	}
	if err := f.tpls.Insert(context.Background(), tpl); err != nil {
		t.Fatalf("insert tpl: %v", err)
	}
	return tpl
}

func (f *resourceFixture) makeChannel(t *testing.T, name, token string, botID, tplID int64) *domain.Channel {
	t.Helper()
	c := &domain.Channel{
		TenantID:     f.tenant.ID,
		Name:         name,
		PushToken:    token,
		BotID:        botID,
		TemplateID:   tplID,
		ChatID:       "-100123",
		RatePerMin:   60,
		DedupWindowS: 0,
		Enabled:      true,
	}
	if err := f.channels.Insert(context.Background(), c); err != nil {
		t.Fatalf("insert channel: %v", err)
	}
	return c
}

// ─── BotRepo ────────────────────────────────────────────────────────

func TestBotRepo_InsertEncrypts(t *testing.T) {
	f := newResourceFixture(t)
	b := f.makeBot(t, "alpha", "12345:secrettoken")

	// raw column must not contain plaintext
	var raw []byte
	if err := f.db.QueryRow(`SELECT bot_token_enc FROM bots WHERE id = ?`, b.ID).Scan(&raw); err != nil {
		t.Fatalf("scan raw: %v", err)
	}
	if strings.Contains(string(raw), "secret") {
		t.Fatalf("plaintext leaked in bot_token_enc")
	}
}

func TestBotRepo_GetByIDDecrypts(t *testing.T) {
	f := newResourceFixture(t)
	b := f.makeBot(t, "alpha", "12345:secrettoken")

	got, err := f.bots.GetByID(context.Background(), f.tenant.ID, b.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.BotToken != "12345:secrettoken" {
		t.Fatalf("token = %q want plaintext", got.BotToken)
	}
}

func TestBotRepo_Update(t *testing.T) {
	f := newResourceFixture(t)
	b := f.makeBot(t, "alpha", "old:token")
	b.Name = "beta"
	b.BotToken = "new:token"
	b.Description = "renamed"
	if err := f.bots.Update(context.Background(), b); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, err := f.bots.GetByID(context.Background(), f.tenant.ID, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "beta" || got.BotToken != "new:token" || got.Description != "renamed" {
		t.Fatalf("update mismatch: %+v", got)
	}
}

func TestBotRepo_Update_WrongTenant(t *testing.T) {
	f := newResourceFixture(t)
	b := f.makeBot(t, "alpha", "tok")
	b.TenantID = 9999
	err := f.bots.Update(context.Background(), b)
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("update wrong tenant err = %v want ErrNotFound", err)
	}
}

func TestBotRepo_Delete(t *testing.T) {
	f := newResourceFixture(t)
	b := f.makeBot(t, "alpha", "tok")
	if err := f.bots.Delete(context.Background(), f.tenant.ID, b.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	_, err := f.bots.GetByID(context.Background(), f.tenant.ID, b.ID)
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("after delete err = %v", err)
	}
	if err := f.bots.Delete(context.Background(), f.tenant.ID, b.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("double-delete err = %v want ErrNotFound", err)
	}
}

func TestBotRepo_ListByTenant(t *testing.T) {
	f := newResourceFixture(t)
	f.makeBot(t, "a", "1:t")
	f.makeBot(t, "b", "2:t")
	f.makeBot(t, "c", "3:t")
	got, err := f.bots.ListByTenant(context.Background(), f.tenant.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("len = %d want 3", len(got))
	}
}

func TestBotRepo_UniqueNameWithinTenant(t *testing.T) {
	f := newResourceFixture(t)
	f.makeBot(t, "alpha", "1:t")
	err := f.bots.Insert(context.Background(), &domain.Bot{
		TenantID: f.tenant.ID, Name: "alpha", BotToken: "2:t",
	})
	if err == nil {
		t.Fatalf("duplicate name should fail")
	}
}

func TestBotRepo_PlatformDefaultTelegram(t *testing.T) {
	f := newResourceFixture(t)
	// Insert with empty Platform should be back-filled to "telegram".
	b := &domain.Bot{TenantID: f.tenant.ID, Name: "default-plat", BotToken: "1:t"}
	if err := f.bots.Insert(context.Background(), b); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if b.Platform != domain.PlatformTelegram {
		t.Fatalf("Platform after insert = %q want telegram", b.Platform)
	}
	got, err := f.bots.GetByID(context.Background(), f.tenant.ID, b.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Platform != domain.PlatformTelegram {
		t.Fatalf("roundtrip Platform = %q want telegram", got.Platform)
	}
}

func TestBotRepo_PlatformExplicitRoundtrip(t *testing.T) {
	f := newResourceFixture(t)
	b := &domain.Bot{
		TenantID: f.tenant.ID, Name: "explicit", BotToken: "1:t",
		Platform: domain.PlatformTelegram,
	}
	if err := f.bots.Insert(context.Background(), b); err != nil {
		t.Fatalf("insert: %v", err)
	}
	got, err := f.bots.GetByID(context.Background(), f.tenant.ID, b.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Platform != domain.PlatformTelegram {
		t.Fatalf("Platform = %q want telegram", got.Platform)
	}
}

func TestBotRepo_PlatformRejectsUnknown(t *testing.T) {
	f := newResourceFixture(t)
	b := &domain.Bot{
		TenantID: f.tenant.ID, Name: "bad", BotToken: "1:t",
		Platform: "discord",
	}
	err := f.bots.Insert(context.Background(), b)
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("Insert unknown platform err = %v want ErrValidation", err)
	}
}

func TestBotRepo_Migrate0002BackfillsExistingRows(t *testing.T) {
	// Simulate a database that only has migration 0001 applied (an
	// "older" install), insert a bot via raw SQL with no platform column
	// awareness, then run Migrate which should add the column with a
	// "telegram" default — leaving the existing row's platform = "telegram".
	dir := t.TempDir()
	dbPath := dir + "/legacy.db"
	db, err := Open(config.Database{Path: dbPath, BusyTimeout: config.Duration(5 * time.Second)})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	clk := &domain.FakeClock{T: time.Date(2026, 5, 17, 10, 0, 0, 0, time.UTC)}

	// Apply only migration 0001 by sub-FS-ing the embedded migrations dir
	// and running the v1 file directly.
	sub, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		t.Fatalf("fs.Sub: %v", err)
	}
	v1Body, err := fs.ReadFile(sub, "0001_init.sql")
	if err != nil {
		t.Fatalf("read v1: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)`); err != nil {
		t.Fatalf("create schema_migrations: %v", err)
	}
	if _, err := db.Exec(string(v1Body)); err != nil {
		t.Fatalf("apply 0001: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO schema_migrations(version, applied_at) VALUES (1, ?)`, clk.Now().UnixMilli()); err != nil {
		t.Fatalf("record 0001: %v", err)
	}

	// Seed a tenant + a bot row through raw SQL (no platform column yet).
	if _, err := db.Exec(`INSERT INTO tenants(email, password_hash, role, status, created_at, updated_at)
		VALUES('legacy@x.com','x','user','active',?,?)`, clk.Now().UnixMilli(), clk.Now().UnixMilli()); err != nil {
		t.Fatalf("insert tenant: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO bots(tenant_id, name, bot_token_enc, description, created_at, updated_at)
		VALUES(1,'legacy-bot',?,'legacy',?,?)`, []byte{0x00, 0x01, 0x02}, clk.Now().UnixMilli(), clk.Now().UnixMilli()); err != nil {
		t.Fatalf("insert legacy bot: %v", err)
	}

	// Now apply remaining migrations (0002 adds platform with default 'telegram').
	if err := Migrate(context.Background(), db, clk); err != nil {
		t.Fatalf("Migrate to head: %v", err)
	}

	var platform string
	if err := db.QueryRow(`SELECT platform FROM bots WHERE name='legacy-bot'`).Scan(&platform); err != nil {
		t.Fatalf("read platform: %v", err)
	}
	if platform != domain.PlatformTelegram {
		t.Fatalf("legacy row platform = %q want telegram", platform)
	}
}

func TestBotRepo_ListAllAcrossTenants(t *testing.T) {
	f := newResourceFixture(t)
	// Seed a second tenant + bots, then assert ListAll returns rows from
	// both. Names must stay unique only within (tenant, name) so the
	// second bot reuses "alpha".
	t2 := makeTenant("second@x.com")
	if err := f.tenants.Insert(context.Background(), t2); err != nil {
		t.Fatalf("insert second tenant: %v", err)
	}
	b1 := f.makeBot(t, "alpha", "1:t")
	b2 := &domain.Bot{TenantID: t2.ID, Name: "alpha", BotToken: "2:t"}
	if err := f.bots.Insert(context.Background(), b2); err != nil {
		t.Fatalf("insert tenant-2 bot: %v", err)
	}
	all, err := f.bots.ListAll(context.Background())
	if err != nil {
		t.Fatalf("ListAll: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("ListAll len = %d want 2", len(all))
	}
	// Tokens decrypted and platform back-filled.
	seen := map[int64]string{}
	for _, b := range all {
		seen[b.ID] = b.Platform
	}
	if seen[b1.ID] != domain.PlatformTelegram || seen[b2.ID] != domain.PlatformTelegram {
		t.Fatalf("platform map = %+v", seen)
	}
}

// ─── TemplateRepo ───────────────────────────────────────────────────

func TestTemplateRepo_CRUD(t *testing.T) {
	f := newResourceFixture(t)
	tpl := f.makeTemplate(t, "alert")
	got, err := f.tpls.GetByID(context.Background(), f.tenant.ID, tpl.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Body != "Hello {{.who}}" {
		t.Fatalf("body = %q", got.Body)
	}

	got.Body = "Goodbye {{.who}}"
	got.ParseMode = domain.ParseHTML
	if err := f.tpls.Update(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	again, err := f.tpls.GetByID(context.Background(), f.tenant.ID, tpl.ID)
	if err != nil {
		t.Fatal(err)
	}
	if again.Body != "Goodbye {{.who}}" || again.ParseMode != domain.ParseHTML {
		t.Fatalf("update mismatch: %+v", again)
	}

	if err := f.tpls.Delete(context.Background(), f.tenant.ID, tpl.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.tpls.GetByID(context.Background(), f.tenant.ID, tpl.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("after delete err = %v", err)
	}
}

func TestTemplateRepo_RejectsInvalidParseMode(t *testing.T) {
	f := newResourceFixture(t)
	err := f.tpls.Insert(context.Background(), &domain.Template{
		TenantID: f.tenant.ID, Name: "x", Body: "y", ParseMode: "Markdown",
	})
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("invalid parse_mode err = %v", err)
	}
}

// ─── ChannelRepo ────────────────────────────────────────────────────

func TestChannelRepo_CRUD(t *testing.T) {
	f := newResourceFixture(t)
	b := f.makeBot(t, "bot", "1:t")
	tpl := f.makeTemplate(t, "tpl")
	c := f.makeChannel(t, "primary", "tokenABC", b.ID, tpl.ID)

	got, err := f.channels.GetByID(context.Background(), f.tenant.ID, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Enabled != true {
		t.Fatalf("Enabled = %v", got.Enabled)
	}
	if got.RatePerMin != 60 {
		t.Fatalf("RatePerMin = %d", got.RatePerMin)
	}

	got.Enabled = false
	got.ChatID = "newchat"
	got.RatePerMin = 30
	got.DedupWindowS = 120
	if err := f.channels.Update(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	again, err := f.channels.GetByID(context.Background(), f.tenant.ID, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if again.Enabled != false || again.ChatID != "newchat" ||
		again.RatePerMin != 30 || again.DedupWindowS != 120 {
		t.Fatalf("update mismatch: %+v", again)
	}
}

func TestChannelRepo_GetByPushToken(t *testing.T) {
	f := newResourceFixture(t)
	b := f.makeBot(t, "bot", "1:t")
	tpl := f.makeTemplate(t, "tpl")
	c := f.makeChannel(t, "primary", "tokenXYZ", b.ID, tpl.ID)

	got, err := f.channels.GetByPushToken(context.Background(), "tokenXYZ")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != c.ID {
		t.Fatalf("id mismatch")
	}

	if _, err := f.channels.GetByPushToken(context.Background(), "missing"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing token err = %v", err)
	}
}

func TestChannelRepo_PushTokenUnique(t *testing.T) {
	f := newResourceFixture(t)
	b := f.makeBot(t, "bot", "1:t")
	tpl := f.makeTemplate(t, "tpl")
	f.makeChannel(t, "a", "shared", b.ID, tpl.ID)
	err := f.channels.Insert(context.Background(), &domain.Channel{
		TenantID: f.tenant.ID, Name: "b", PushToken: "shared",
		BotID: b.ID, TemplateID: tpl.ID, ChatID: "x",
		RatePerMin: 60, DedupWindowS: 0, Enabled: true,
	})
	if err == nil {
		t.Fatalf("duplicate push_token should fail")
	}
}

func TestChannelRepo_Delete(t *testing.T) {
	f := newResourceFixture(t)
	b := f.makeBot(t, "bot", "1:t")
	tpl := f.makeTemplate(t, "tpl")
	c := f.makeChannel(t, "a", "tok", b.ID, tpl.ID)
	if err := f.channels.Delete(context.Background(), f.tenant.ID, c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.channels.GetByID(context.Background(), f.tenant.ID, c.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("after delete err = %v", err)
	}
}

func TestChannelRepo_ListByTenant(t *testing.T) {
	f := newResourceFixture(t)
	b := f.makeBot(t, "bot", "1:t")
	tpl := f.makeTemplate(t, "tpl")
	f.makeChannel(t, "a", "tok1", b.ID, tpl.ID)
	f.makeChannel(t, "b", "tok2", b.ID, tpl.ID)
	got, err := f.channels.ListByTenant(context.Background(), f.tenant.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d want 2", len(got))
	}
}

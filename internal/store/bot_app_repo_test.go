package store

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/wendi/pulseguard/internal/domain"
)

// TestBotRepo_LarkAppRoundtrip verifies the LB1 schema additions plus
// the app_secret AES-GCM encryption and the lark-app:// derived
// BotToken. Inserts a row with the full set of app-bot credentials,
// then reads it back and asserts:
//
//   - Platform / BotKind round-trip exactly
//   - AppID / VerifyToken / EncryptKey round-trip in plaintext
//   - AppSecret round-trips via the cipher
//   - BotToken (derived) parses to the same AppID + secret
//   - The raw app_secret_enc column does NOT contain the plaintext
func TestBotRepo_LarkAppRoundtrip(t *testing.T) {
	f := newResourceFixture(t)
	b := &domain.Bot{
		TenantID:    f.tenant.ID,
		Name:        "lark-app-bot",
		Platform:    domain.PlatformLark,
		BotKind:     domain.BotKindApp,
		Description: "phase B",
		AppID:       "cli_a1b2c3d4e5f6",
		AppSecret:   "verysecret-LB1-value",
		VerifyToken: "verify-tok-xyz",
		EncryptKey:  "encrypt-key-abc",
	}
	if err := f.bots.Insert(context.Background(), b); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if b.BotToken == "" || !strings.HasPrefix(b.BotToken, "lark-app://") {
		t.Fatalf("BotToken on insert = %q want lark-app://... prefix", b.BotToken)
	}

	// Raw column must not contain the plaintext secret.
	var raw []byte
	if err := f.db.QueryRow(`SELECT app_secret_enc FROM bots WHERE id = ?`, b.ID).Scan(&raw); err != nil {
		t.Fatalf("scan raw app_secret_enc: %v", err)
	}
	if strings.Contains(string(raw), "verysecret") {
		t.Fatalf("plaintext leaked in app_secret_enc")
	}

	got, err := f.bots.GetByID(context.Background(), f.tenant.ID, b.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Platform != domain.PlatformLark || got.BotKind != domain.BotKindApp {
		t.Fatalf("platform/kind = %q/%q", got.Platform, got.BotKind)
	}
	if got.AppID != "cli_a1b2c3d4e5f6" {
		t.Fatalf("AppID = %q", got.AppID)
	}
	if got.AppSecret != "verysecret-LB1-value" {
		t.Fatalf("AppSecret = %q (decryption failed?)", got.AppSecret)
	}
	if got.VerifyToken != "verify-tok-xyz" {
		t.Fatalf("VerifyToken = %q", got.VerifyToken)
	}
	if got.EncryptKey != "encrypt-key-abc" {
		t.Fatalf("EncryptKey = %q", got.EncryptKey)
	}
	if !strings.HasPrefix(got.BotToken, "lark-app://cli_a1b2c3d4e5f6") {
		t.Fatalf("derived BotToken = %q", got.BotToken)
	}

	// BotToken must parse back to the (AppID, secret) pair.
	u, err := url.Parse(got.BotToken)
	if err != nil {
		t.Fatalf("url.Parse(BotToken): %v", err)
	}
	if u.Scheme != "lark-app" {
		t.Fatalf("scheme = %q", u.Scheme)
	}
	if u.Host != "cli_a1b2c3d4e5f6" {
		t.Fatalf("host = %q", u.Host)
	}
	if u.Query().Get("secret") != "verysecret-LB1-value" {
		t.Fatalf("query secret = %q", u.Query().Get("secret"))
	}
}

// TestBotRepo_LarkWebhookKindDefaults guards the schema default: pre-
// Phase-B rows (and freshly inserted webhook rows with BotKind="") get
// BotKindWebhook back on read so the runtime sender_router stays on
// the existing https://open.feishu.cn webhook path.
func TestBotRepo_LarkWebhookKindDefaults(t *testing.T) {
	f := newResourceFixture(t)
	b := &domain.Bot{
		TenantID: f.tenant.ID,
		Name:     "lark-webhook",
		Platform: domain.PlatformLark,
		BotToken: "https://open.feishu.cn/open-apis/bot/v2/hook/abc",
	}
	if err := f.bots.Insert(context.Background(), b); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if b.BotKind != domain.BotKindWebhook {
		t.Fatalf("BotKind after insert = %q want webhook", b.BotKind)
	}
	got, err := f.bots.GetByID(context.Background(), f.tenant.ID, b.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.BotKind != domain.BotKindWebhook {
		t.Fatalf("roundtrip BotKind = %q want webhook", got.BotKind)
	}
	if got.BotToken != b.BotToken {
		t.Fatalf("BotToken changed: %q", got.BotToken)
	}
}

// TestBotRepo_RejectsAppKindOnTelegram guards the validation: BotKind
// "app" only makes sense for lark rows. A telegram+app combination
// would silently route an app secret through the telegram client.
func TestBotRepo_RejectsAppKindOnTelegram(t *testing.T) {
	f := newResourceFixture(t)
	b := &domain.Bot{
		TenantID: f.tenant.ID,
		Name:     "bad-tg-app",
		Platform: domain.PlatformTelegram,
		BotKind:  domain.BotKindApp,
		AppID:    "x",
	}
	err := f.bots.Insert(context.Background(), b)
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("err = %v want ErrValidation", err)
	}
}

// TestBotRepo_RejectsUnknownBotKind pins the kind allow-list.
func TestBotRepo_RejectsUnknownBotKind(t *testing.T) {
	f := newResourceFixture(t)
	b := &domain.Bot{
		TenantID: f.tenant.ID,
		Name:     "bad-kind",
		Platform: domain.PlatformLark,
		BotKind:  "weird",
		BotToken: "https://open.feishu.cn/open-apis/bot/v2/hook/abc",
	}
	err := f.bots.Insert(context.Background(), b)
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("err = %v want ErrValidation", err)
	}
}

// TestBotRepo_LarkAppUpdatePreservesSecret guards the Update preserve-
// existing path: when an operator edits an app bot's name without
// re-typing the secret (AppSecret left empty on the struct), the
// stored secret MUST survive.
func TestBotRepo_LarkAppUpdatePreservesSecret(t *testing.T) {
	f := newResourceFixture(t)
	b := &domain.Bot{
		TenantID:    f.tenant.ID,
		Name:        "lark-app-preserve",
		Platform:    domain.PlatformLark,
		BotKind:     domain.BotKindApp,
		AppID:       "cli_keep_me",
		AppSecret:   "original-secret",
		VerifyToken: "v",
		EncryptKey:  "e",
	}
	if err := f.bots.Insert(context.Background(), b); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	// Pull a fresh copy, blank the secret, change the name, and Update.
	got, err := f.bots.GetByID(context.Background(), f.tenant.ID, b.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	got.Name = "renamed"
	got.AppSecret = "" // simulate "leave blank to keep" UI flow
	if err := f.bots.Update(context.Background(), got); err != nil {
		t.Fatalf("Update: %v", err)
	}
	again, err := f.bots.GetByID(context.Background(), f.tenant.ID, b.ID)
	if err != nil {
		t.Fatalf("GetByID after update: %v", err)
	}
	if again.Name != "renamed" {
		t.Fatalf("name = %q want renamed", again.Name)
	}
	if again.AppSecret != "original-secret" {
		t.Fatalf("AppSecret = %q want preserved", again.AppSecret)
	}
}

// TestBotRepo_LarkAppUpdateRotatesSecret guards the rotation path:
// passing an explicit non-empty AppSecret on Update must overwrite
// the stored value.
func TestBotRepo_LarkAppUpdateRotatesSecret(t *testing.T) {
	f := newResourceFixture(t)
	b := &domain.Bot{
		TenantID:    f.tenant.ID,
		Name:        "lark-app-rotate",
		Platform:    domain.PlatformLark,
		BotKind:     domain.BotKindApp,
		AppID:       "cli_rotate",
		AppSecret:   "old-secret",
		VerifyToken: "v",
		EncryptKey:  "e",
	}
	if err := f.bots.Insert(context.Background(), b); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	got, err := f.bots.GetByID(context.Background(), f.tenant.ID, b.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	got.AppSecret = "new-secret"
	if err := f.bots.Update(context.Background(), got); err != nil {
		t.Fatalf("Update: %v", err)
	}
	again, err := f.bots.GetByID(context.Background(), f.tenant.ID, b.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if again.AppSecret != "new-secret" {
		t.Fatalf("AppSecret = %q want new-secret", again.AppSecret)
	}
}

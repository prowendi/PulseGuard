package web

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/wendi/pulseguard/internal/domain"
)

// TestBotsAPI_CreateLarkAppBot pins the LB7 happy path: an app-mode
// lark bot is created via the JSON API with the full credential set.
// The response surfaces bot_kind=app, app_id, verify_token,
// encrypt_key (encrypt_key is intentionally returned so the UI can
// re-render it on the edit screen without round-tripping the
// secret), and app_secret_set=true. The masked bot_token_last4 is
// empty for app rows.
func TestBotsAPI_CreateLarkAppBot(t *testing.T) {
	h := newTestHarness(t)
	_, _ = h.seedAdmin("admin@example.com", "adminpass", "INV1")
	client, csrf := registerTenantAPI(t, h, "alice@example.com", "alicepass", "INV1")

	body := mustJSON(t, map[string]any{
		"name":         "larkapp",
		"platform":     "lark",
		"bot_kind":     "app",
		"app_id":       "cli_a1b2c3d4e5f6",
		"app_secret":   "sec-LB7",
		"verify_token": "vt-LB7",
		"encrypt_key":  "ek-LB7",
	})
	req, _ := http.NewRequest(http.MethodPost, h.fullURL("/api/v1/bots"), bytes.NewReader(body))
	resp, err := client.Do(withCSRF(req, csrf))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d body=%s", resp.StatusCode, drain(resp))
	}
	var created botView
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	if created.Platform != "lark" || created.BotKind != "app" {
		t.Fatalf("platform/kind = %q/%q", created.Platform, created.BotKind)
	}
	if created.AppID != "cli_a1b2c3d4e5f6" {
		t.Fatalf("AppID = %q", created.AppID)
	}
	// SEC-1: verify_token and encrypt_key are never returned in the
	// clear — only their boolean *_set fields surface. The plaintexts
	// stay server-side once stored so a stolen session can't forge
	// inbound Lark events.
	if !created.VerifyTokenSet {
		t.Fatalf("VerifyTokenSet = false (want true after providing verify_token)")
	}
	if !created.EncryptKeySet {
		t.Fatalf("EncryptKeySet = false (want true after providing encrypt_key)")
	}
	if !created.AppSecretSet {
		t.Fatalf("AppSecretSet = false (want true)")
	}
	if created.BotTokenLast4 != "" {
		t.Fatalf("BotTokenLast4 = %q (want empty for app row)", created.BotTokenLast4)
	}
}

// TestBotsAPI_CreateLarkAppBot_RequiresEncryptKey enforces the
// encrypt_key requirement — it's mandatory because the events
// endpoint relies on it for HMAC verification.
func TestBotsAPI_CreateLarkAppBot_RequiresEncryptKey(t *testing.T) {
	h := newTestHarness(t)
	_, _ = h.seedAdmin("admin@example.com", "adminpass", "INV1")
	client, csrf := registerTenantAPI(t, h, "alice@example.com", "alicepass", "INV1")

	body := mustJSON(t, map[string]any{
		"name":       "noEncrypt",
		"platform":   "lark",
		"bot_kind":   "app",
		"app_id":     "cli_abcdef1234",
		"app_secret": "s",
	})
	req, _ := http.NewRequest(http.MethodPost, h.fullURL("/api/v1/bots"), bytes.NewReader(body))
	resp, err := client.Do(withCSRF(req, csrf))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d want 400", resp.StatusCode)
	}
}

// TestBotsAPI_CreateLarkAppBot_RejectsTelegramKindApp guards the
// invariant that bot_kind=app makes sense only for platform=lark.
func TestBotsAPI_CreateLarkAppBot_RejectsTelegramKindApp(t *testing.T) {
	h := newTestHarness(t)
	_, _ = h.seedAdmin("admin@example.com", "adminpass", "INV1")
	client, csrf := registerTenantAPI(t, h, "alice@example.com", "alicepass", "INV1")

	body := mustJSON(t, map[string]any{
		"name":       "tgapp",
		"platform":   "telegram",
		"bot_kind":   "app",
		"app_id":     "cli_abcdef1234",
		"app_secret": "s",
	})
	req, _ := http.NewRequest(http.MethodPost, h.fullURL("/api/v1/bots"), bytes.NewReader(body))
	resp, err := client.Do(withCSRF(req, csrf))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d want 400", resp.StatusCode)
	}
}

// TestBotsAPI_UpdateLarkAppBot_PreservesSecret guards the "blank
// app_secret = keep" semantics on Update.
func TestBotsAPI_UpdateLarkAppBot_PreservesSecret(t *testing.T) {
	h := newTestHarness(t)
	_, _ = h.seedAdmin("admin@example.com", "adminpass", "INV1")
	client, csrf := registerTenantAPI(t, h, "alice@example.com", "alicepass", "INV1")

	// Create with secret.
	body := mustJSON(t, map[string]any{
		"name":         "rotate-test",
		"platform":     "lark",
		"bot_kind":     "app",
		"app_id":       "cli_preserve123",
		"app_secret":   "original-sec",
		"verify_token": "vt",
		"encrypt_key":  "ek",
	})
	req, _ := http.NewRequest(http.MethodPost, h.fullURL("/api/v1/bots"), bytes.NewReader(body))
	resp, err := client.Do(withCSRF(req, csrf))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	var created botView
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d", resp.StatusCode)
	}

	// Update — change only the name, omit app_secret. Server must
	// preserve the existing secret.
	updBody := mustJSON(t, map[string]any{
		"name":     "rotate-test-renamed",
		"platform": "lark",
		"bot_kind": "app",
		"app_id":   "cli_preserve123",
	})
	req, _ = http.NewRequest(http.MethodPut, h.fullURL("/api/v1/bots/"+strInt64(created.ID)), bytes.NewReader(updBody))
	resp, err = client.Do(withCSRF(req, csrf))
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("update status = %d body=%s", resp.StatusCode, drain(resp))
	}
	var updated botView
	_ = json.NewDecoder(resp.Body).Decode(&updated)
	resp.Body.Close()
	if !updated.AppSecretSet {
		t.Fatalf("AppSecretSet = false after update without secret; secret was lost")
	}
	if updated.Name != "rotate-test-renamed" {
		t.Fatalf("name = %q", updated.Name)
	}

	// Direct repo check to be extra sure the encrypted value survives.
	tID := meTenantID(t, h, client)
	got, err := h.deps.Bots.GetByID(context.Background(), tID, created.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.AppSecret != "original-sec" {
		t.Fatalf("AppSecret = %q want original-sec", got.AppSecret)
	}
}

// TestUIBots_AppDrawerRendersFields renders /ui/bots and asserts the
// LB7 drawer DOM is wired up: bot_kind radios, app_id / app_secret /
// verify_token / encrypt_key inputs are present, plus the
// data-action=lark-kind-changed hooks.
func TestUIBots_AppDrawerRendersFields(t *testing.T) {
	h := newTestHarness(t)
	_, _ = h.seedAdmin("admin@example.com", "adminpass", "INV1")
	client, _ := registerTenantAPI(t, h, "alice@example.com", "alicepass", "INV1")

	resp, err := client.Get(h.fullURL("/ui/bots"))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	bs, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body := string(bs)

	must := []string{
		`name="bot_kind"`,
		`value="webhook"`,
		`value="app"`,
		`name="app_id"`,
		`name="app_secret"`,
		`name="verify_token"`,
		`name="encrypt_key"`,
		`data-action="lark-kind-changed"`,
		`data-action="lark-kind-platform"`,
		`data-scope="new-kind-row"`,
		`data-scope="new-webhook-fields"`,
		`data-scope="new-app-fields"`,
		`data-scope="edit-kind-row"`,
		`data-scope="edit-webhook-fields"`,
		`data-scope="edit-app-fields"`,
	}
	for _, s := range must {
		if !strings.Contains(body, s) {
			t.Errorf("missing drawer feature %q", s)
		}
	}
}

// TestUIBots_CreateAppBotViaForm exercises the HTMX UI POST path for
// creating an app-mode lark bot.
func TestUIBots_CreateAppBotViaForm(t *testing.T) {
	h := newTestHarness(t)
	_, _ = h.seedAdmin("admin@example.com", "adminpass", "INV1")
	client, csrf := registerTenantAPI(t, h, "alice@example.com", "alicepass", "INV1")

	form := url.Values{}
	form.Set("csrf", csrf)
	form.Set("name", "uiapp")
	form.Set("platform", "lark")
	form.Set("bot_kind", "app")
	form.Set("app_id", "cli_uiform1234")
	form.Set("app_secret", "uiform-sec")
	form.Set("verify_token", "vt")
	form.Set("encrypt_key", "ek")
	req, _ := http.NewRequest(http.MethodPost, h.fullURL("/ui/bots"),
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", csrf)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("ui create: %v", err)
	}
	bs, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body := string(bs)
	if !strings.Contains(body, "应用机器人") {
		t.Fatalf("flash missing app-bot hint, body=%s", body[:400])
	}
	// New row should appear in the table.
	if !strings.Contains(body, "uiapp") {
		t.Fatalf("new app bot not rendered")
	}
}

// TestUIBots_AppRowEditButtonCarriesKindAndIDs guards the edit-button
// data attributes for app rows: the prefill needs bot-kind / app-id /
// verify-token / encrypt-key so the drawer hydrates correctly. The
// secret is intentionally NOT round-tripped.
func TestUIBots_AppRowEditButtonCarriesKindAndIDs(t *testing.T) {
	h := newTestHarness(t)
	_, _ = h.seedAdmin("admin@example.com", "adminpass", "INV1")
	client, _ := registerTenantAPI(t, h, "alice@example.com", "alicepass", "INV1")
	tenantID := meTenantID(t, h, client)

	// Seed an app bot via the repo so the table has a row.
	if err := h.deps.Bots.Insert(context.Background(), &domain.Bot{
		TenantID:    tenantID,
		Name:        "appedit",
		Platform:    domain.PlatformLark,
		BotKind:     domain.BotKindApp,
		AppID:       "cli_editme9876",
		AppSecret:   "edit-sec",
		VerifyToken: "edit-vt",
		EncryptKey:  "edit-ek",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	resp, err := client.Get(h.fullURL("/ui/bots"))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	bs, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	body := string(bs)
	must := []string{
		`data-bot-kind="app"`,
		`data-app-id="cli_editme9876"`,
		// SEC-1: only the *_set booleans surface in the DOM; the
		// plaintext verify_token / encrypt_key stay server-side. The
		// edit drawer pre-fills these fields blank, and the backend
		// treats blank-on-update as "keep current".
		`data-verify-token-set="1"`,
		`data-encrypt-key-set="1"`,
	}
	for _, s := range must {
		if !strings.Contains(body, s) {
			t.Errorf("missing edit-row attr %q", s)
		}
	}
	// Secrets MUST NOT be in the DOM (SEC-1 + existing app_secret rule).
	for _, leak := range []string{"edit-sec", "edit-vt", "edit-ek"} {
		if strings.Contains(body, leak) {
			t.Fatalf("plaintext secret %q leaked into HTML", leak)
		}
	}
}

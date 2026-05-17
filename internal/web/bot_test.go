package web

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/prowendi/PulseGuard/internal/domain"
)

// authedAPIClient returns a cookie jar client that has been authenticated
// as the given email/password via the JSON /api/v1/auth/login endpoint.
// The CSRF cookie value is returned alongside so callers can echo it
// into X-CSRF-Token for mutating requests.
func authedAPIClient(t *testing.T, h *testHarness, email, pwd string) (*http.Client, string) {
	t.Helper()
	c := h.newJarClient()
	body := mustJSON(t, map[string]any{"email": email, "password": pwd})
	resp, err := c.Post(h.fullURL("/api/v1/auth/login"), "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login status = %d body=%s", resp.StatusCode, drain(resp))
	}
	resp.Body.Close()
	csrf := jarValue(t, c, h.server.URL, "psg_csrf")
	if csrf == "" {
		t.Fatal("login should issue csrf cookie")
	}
	return c, csrf
}

func registerTenantAPI(t *testing.T, h *testHarness, email, pwd, invite string) (*http.Client, string) {
	t.Helper()
	c := h.newJarClient()
	body := mustJSON(t, map[string]any{
		"email": email, "password": pwd, "invite_code": invite,
	})
	resp, err := c.Post(h.fullURL("/api/v1/auth/register"), "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("register status = %d body=%s", resp.StatusCode, drain(resp))
	}
	resp.Body.Close()
	return c, jarValue(t, c, h.server.URL, "psg_csrf")
}

// withCSRF wraps an http.Request to attach the supplied csrf token + the
// Content-Type header for JSON bodies.
func withCSRF(req *http.Request, csrf string) *http.Request {
	req.Header.Set("X-CSRF-Token", csrf)
	if req.Body != nil && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	return req
}

func TestBotsAPILifecycle(t *testing.T) {
	h := newTestHarness(t)
	_, inv := h.seedAdmin("admin@example.com", "adminpass", "INV1")
	_ = inv
	client, csrf := registerTenantAPI(t, h, "alice@example.com", "alicepass", "INV1")

	// 1. Empty list.
	resp, err := client.Get(h.fullURL("/api/v1/bots"))
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d", resp.StatusCode)
	}
	var listBody map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&listBody)
	resp.Body.Close()
	if items, _ := listBody["items"].([]any); len(items) != 0 {
		t.Fatalf("expected empty list, got %v", items)
	}

	// 2. Create.
	createBody := mustJSON(t, map[string]any{
		"name":        "my-bot",
		"bot_token":   "12345:AAAabcXYZ_-1234",
		"description": "main",
	})
	req, _ := http.NewRequest(http.MethodPost, h.fullURL("/api/v1/bots"), bytes.NewReader(createBody))
	resp, err = client.Do(withCSRF(req, csrf))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d body=%s", resp.StatusCode, drain(resp))
	}
	var created botView
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	if created.ID == 0 {
		t.Fatal("created id is zero")
	}
	if created.BotTokenLast4 != "1234" {
		t.Fatalf("token last4 = %q", created.BotTokenLast4)
	}
	if created.Platform != "telegram" {
		t.Fatalf("platform = %q want telegram", created.Platform)
	}

	// 3. Get by id.
	resp, err = client.Get(h.fullURL("/api/v1/bots/" + strInt64(created.ID)))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get status = %d", resp.StatusCode)
	}
	resp.Body.Close()

	// 4. Update.
	updBody := mustJSON(t, map[string]any{"description": "renamed"})
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
	if updated.Description != "renamed" {
		t.Fatalf("description not updated: %q", updated.Description)
	}

	// 5. Delete.
	req, _ = http.NewRequest(http.MethodDelete, h.fullURL("/api/v1/bots/"+strInt64(created.ID)), nil)
	resp, err = client.Do(withCSRF(req, csrf))
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status = %d", resp.StatusCode)
	}
	resp.Body.Close()

	// 6. Get after delete → 404.
	resp, err = client.Get(h.fullURL("/api/v1/bots/" + strInt64(created.ID)))
	if err != nil {
		t.Fatalf("get after delete: %v", err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("get after delete status = %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestBotsAPIBadToken(t *testing.T) {
	h := newTestHarness(t)
	_, _ = h.seedAdmin("admin@example.com", "adminpass", "INV1")
	client, csrf := registerTenantAPI(t, h, "alice@example.com", "alicepass", "INV1")

	body := mustJSON(t, map[string]any{
		"name":      "bad",
		"bot_token": "not-a-token",
	})
	req, _ := http.NewRequest(http.MethodPost, h.fullURL("/api/v1/bots"), bytes.NewReader(body))
	resp, err := client.Do(withCSRF(req, csrf))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestBotsAPIPlatformExplicitTelegram(t *testing.T) {
	h := newTestHarness(t)
	_, _ = h.seedAdmin("admin@example.com", "adminpass", "INV1")
	client, csrf := registerTenantAPI(t, h, "alice@example.com", "alicepass", "INV1")

	body := mustJSON(t, map[string]any{
		"name":      "platbot",
		"bot_token": "12345:AAAabcXYZ_-1234",
		"platform":  "telegram",
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
	if created.Platform != "telegram" {
		t.Fatalf("platform = %q want telegram", created.Platform)
	}
}

func TestBotsAPIPlatformDefaultsTelegram(t *testing.T) {
	h := newTestHarness(t)
	_, _ = h.seedAdmin("admin@example.com", "adminpass", "INV1")
	client, csrf := registerTenantAPI(t, h, "alice@example.com", "alicepass", "INV1")

	// Omit platform entirely; server should fill in "telegram".
	body := mustJSON(t, map[string]any{
		"name":      "defplat",
		"bot_token": "12345:AAAabcXYZ_-1234",
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
	if created.Platform != "telegram" {
		t.Fatalf("platform = %q want telegram (default)", created.Platform)
	}
}

func TestBotsAPIPlatformUnknownRejected(t *testing.T) {
	h := newTestHarness(t)
	_, _ = h.seedAdmin("admin@example.com", "adminpass", "INV1")
	client, csrf := registerTenantAPI(t, h, "alice@example.com", "alicepass", "INV1")

	body := mustJSON(t, map[string]any{
		"name":      "wrong",
		"bot_token": "12345:AAAabcXYZ_-1234",
		"platform":  "discord",
	})
	req, _ := http.NewRequest(http.MethodPost, h.fullURL("/api/v1/bots"), bytes.NewReader(body))
	resp, err := client.Do(withCSRF(req, csrf))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s want 400", resp.StatusCode, drain(resp))
	}
	resp.Body.Close()
}

func TestBotsAPIRequiresCSRF(t *testing.T) {
	h := newTestHarness(t)
	_, _ = h.seedAdmin("admin@example.com", "adminpass", "INV1")
	client, _ := registerTenantAPI(t, h, "alice@example.com", "alicepass", "INV1")

	body := mustJSON(t, map[string]any{
		"name":      "needs-csrf",
		"bot_token": "12345:Aaa",
	})
	req, _ := http.NewRequest(http.MethodPost, h.fullURL("/api/v1/bots"), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req) // no X-CSRF-Token
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestBotsAPIRejectsCrossTenant(t *testing.T) {
	h := newTestHarness(t)
	_, _ = h.seedAdmin("admin@example.com", "adminpass", "INV1")

	// Seed another tenant's bot directly via repo.
	other := &domain.Tenant{
		Email: "other@example.com", PasswordHash: "x",
		Role: domain.RoleUser, Status: domain.TenantActive,
	}
	_ = h.deps.Tenants.Insert(context.Background(), other)
	otherBot := &domain.Bot{TenantID: other.ID, Name: "x", BotToken: "12345:Z"}
	_ = h.deps.Bots.Insert(context.Background(), otherBot)

	// Now create our own tenant via API and try to fetch other's bot.
	// Need a second invite for the second tenant.
	inv2 := &domain.InviteCode{Code: "INV2", CreatedBy: other.ID}
	_ = h.deps.Invites.Insert(context.Background(), inv2)
	client, _ := registerTenantAPI(t, h, "alice@example.com", "alicepass", "INV2")

	resp, err := client.Get(h.fullURL("/api/v1/bots/" + strInt64(otherBot.ID)))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-tenant get status = %d, expected 404", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestUIBotsRenders(t *testing.T) {
	h := newTestHarness(t)
	_, _ = h.seedAdmin("admin@example.com", "adminpass", "INV1")
	// Register tenant via API, then re-use the same client for UI.
	client, _ := registerTenantAPI(t, h, "alice@example.com", "alicepass", "INV1")

	resp, err := client.Get(h.fullURL("/ui/bots"))
	if err != nil {
		t.Fatalf("get /ui/bots: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	bs, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	body := string(bs)
	if !strings.Contains(body, ">Bots<") {
		t.Fatalf("body missing Bots heading: %s", body[:200])
	}
	if !strings.Contains(body, `action="/ui/bots"`) {
		t.Fatalf("body missing create form")
	}
	// Platform column + select.
	if !strings.Contains(body, ">Platform<") {
		t.Fatalf("table missing Platform header")
	}
	if !strings.Contains(body, `name="platform"`) {
		t.Fatalf("form missing platform select")
	}
	if !strings.Contains(body, `value="telegram"`) {
		t.Fatalf("form missing telegram option")
	}
}

func TestUIBotsCreateShowsFlashHint(t *testing.T) {
	h := newTestHarness(t)
	_, _ = h.seedAdmin("admin@example.com", "adminpass", "INV1")
	client, csrf := registerTenantAPI(t, h, "alice@example.com", "alicepass", "INV1")

	form := url.Values{}
	form.Set("csrf", csrf)
	form.Set("name", "uibot")
	form.Set("bot_token", "12345:AAAabcXYZ_-XX99")
	form.Set("description", "ui-created")
	form.Set("platform", "telegram")

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
		t.Fatalf("status = %d body=%s", resp.StatusCode, string(bs)[:400])
	}
	body := string(bs)
	// Flash hint about /start should be rendered as a flash partial.
	if !strings.Contains(body, "/start") {
		t.Fatalf("missing /start hint, body=%s", body[:400])
	}
	if !strings.Contains(body, "Chat ID") {
		t.Fatalf("missing Chat ID hint, body=%s", body[:400])
	}
	// New row should appear in the table with platform column.
	if !strings.Contains(body, "uibot") {
		t.Fatalf("new bot not rendered in table")
	}
}

func strInt64(n int64) string {
	return strconv.FormatInt(n, 10)
}

// TestBotsAPI_EnableDisableLifecycle exercises the dedicated
// /bots/{id}/enable + /disable endpoints. The created bot starts
// enabled (default-on insert path covered in store tests). After
// disable the view echoes enabled=false; after re-enable it flips
// back. With no BotListeners wired into the harness deps, the
// listener Start/Stop calls become no-ops — they're exercised at the
// platform layer in manager_test.go.
func TestBotsAPI_EnableDisableLifecycle(t *testing.T) {
	h := newTestHarness(t)
	_, _ = h.seedAdmin("admin@example.com", "adminpass", "INV1")
	client, csrf := registerTenantAPI(t, h, "alice@example.com", "alicepass", "INV1")

	// Create a bot. It must come back enabled by default.
	createBody := mustJSON(t, map[string]any{
		"name":      "tog-bot",
		"bot_token": "12345:AAAabcXYZ_-1234",
	})
	req, _ := http.NewRequest(http.MethodPost, h.fullURL("/api/v1/bots"), bytes.NewReader(createBody))
	resp, err := client.Do(withCSRF(req, csrf))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d body=%s", resp.StatusCode, drain(resp))
	}
	var created botView
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	if !created.Enabled {
		t.Fatalf("freshly-created bot enabled = false (want true)")
	}

	// Disable.
	req, _ = http.NewRequest(http.MethodPost, h.fullURL("/api/v1/bots/"+strInt64(created.ID)+"/disable"), nil)
	resp, err = client.Do(withCSRF(req, csrf))
	if err != nil {
		t.Fatalf("disable: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("disable status = %d body=%s", resp.StatusCode, drain(resp))
	}
	var disabled botView
	_ = json.NewDecoder(resp.Body).Decode(&disabled)
	resp.Body.Close()
	if disabled.Enabled {
		t.Fatalf("after /disable enabled = true (want false)")
	}
	if disabled.ID != created.ID {
		t.Fatalf("disable returned wrong id %d want %d", disabled.ID, created.ID)
	}

	// GET reflects the persisted state.
	resp, err = client.Get(h.fullURL("/api/v1/bots/" + strInt64(created.ID)))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	var fetched botView
	_ = json.NewDecoder(resp.Body).Decode(&fetched)
	resp.Body.Close()
	if fetched.Enabled {
		t.Fatalf("GET after disable still reports enabled = true")
	}

	// Re-enable.
	req, _ = http.NewRequest(http.MethodPost, h.fullURL("/api/v1/bots/"+strInt64(created.ID)+"/enable"), nil)
	resp, err = client.Do(withCSRF(req, csrf))
	if err != nil {
		t.Fatalf("enable: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("enable status = %d body=%s", resp.StatusCode, drain(resp))
	}
	var enabled botView
	_ = json.NewDecoder(resp.Body).Decode(&enabled)
	resp.Body.Close()
	if !enabled.Enabled {
		t.Fatalf("after /enable enabled = false (want true)")
	}
}

func TestBotsAPI_DisableRequiresCSRF(t *testing.T) {
	h := newTestHarness(t)
	_, _ = h.seedAdmin("admin@example.com", "adminpass", "INV1")
	client, csrf := registerTenantAPI(t, h, "alice@example.com", "alicepass", "INV1")

	// Seed a bot via API so the row exists.
	body := mustJSON(t, map[string]any{
		"name":      "needs-csrf",
		"bot_token": "12345:AAAabcXYZ_-1234",
	})
	req, _ := http.NewRequest(http.MethodPost, h.fullURL("/api/v1/bots"), bytes.NewReader(body))
	resp, err := client.Do(withCSRF(req, csrf))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	var created botView
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()

	req, _ = http.NewRequest(http.MethodPost, h.fullURL("/api/v1/bots/"+strInt64(created.ID)+"/disable"), nil)
	// Intentionally no X-CSRF-Token.
	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("disable: %v", err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d want 403", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestBotsAPI_DisableUnknownIsNotFound(t *testing.T) {
	h := newTestHarness(t)
	_, _ = h.seedAdmin("admin@example.com", "adminpass", "INV1")
	client, csrf := registerTenantAPI(t, h, "alice@example.com", "alicepass", "INV1")

	req, _ := http.NewRequest(http.MethodPost, h.fullURL("/api/v1/bots/9999/disable"), nil)
	resp, err := client.Do(withCSRF(req, csrf))
	if err != nil {
		t.Fatalf("disable: %v", err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d want 404", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestBotsAPI_DisableCrossTenantIsNotFound(t *testing.T) {
	h := newTestHarness(t)
	_, _ = h.seedAdmin("admin@example.com", "adminpass", "INV1")

	// Seed another tenant's bot directly via repo.
	other := &domain.Tenant{
		Email: "other@example.com", PasswordHash: "x",
		Role: domain.RoleUser, Status: domain.TenantActive,
	}
	_ = h.deps.Tenants.Insert(context.Background(), other)
	otherBot := &domain.Bot{TenantID: other.ID, Name: "x", BotToken: "12345:Z"}
	_ = h.deps.Bots.Insert(context.Background(), otherBot)

	inv2 := &domain.InviteCode{Code: "INV2", CreatedBy: other.ID}
	_ = h.deps.Invites.Insert(context.Background(), inv2)
	client, csrf := registerTenantAPI(t, h, "alice@example.com", "alicepass", "INV2")

	req, _ := http.NewRequest(http.MethodPost, h.fullURL("/api/v1/bots/"+strInt64(otherBot.ID)+"/disable"), nil)
	resp, err := client.Do(withCSRF(req, csrf))
	if err != nil {
		t.Fatalf("disable: %v", err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-tenant disable status = %d want 404", resp.StatusCode)
	}
	resp.Body.Close()

	// And the other tenant's bot stays enabled.
	got, err := h.deps.Bots.GetByID(context.Background(), other.ID, otherBot.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if !got.Enabled {
		t.Fatalf("cross-tenant disable mutated victim row")
	}
}

// TestUIBotsUpdate_DescriptionOnlyKeepsToken proves the "blank
// bot_token = keep current" semantic: a cosmetic edit that only
// touches description must NOT mutate the encrypted token column. We
// read the row back via the repo (not the masked API view) and
// compare full plaintext so a regression that replaces token with the
// empty string would fail loudly.
func TestUIBotsUpdate_DescriptionOnlyKeepsToken(t *testing.T) {
	h := newTestHarness(t)
	_, _ = h.seedAdmin("admin@example.com", "adminpass", "INV1")
	client, csrf := registerTenantAPI(t, h, "alice@example.com", "alicepass", "INV1")
	tenantID := meTenantID(t, h, client)

	bot := &domain.Bot{TenantID: tenantID, Name: "keep-tok", Platform: domain.PlatformTelegram, BotToken: "12345:AAAAoriginalTok"}
	if err := h.deps.Bots.Insert(context.Background(), bot); err != nil {
		t.Fatalf("seed bot: %v", err)
	}

	form := url.Values{}
	form.Set("csrf", csrf)
	form.Set("name", bot.Name)
	form.Set("description", "edited-desc")
	form.Set("platform", "telegram")
	form.Set("bot_token", "") // blank = keep
	req, _ := http.NewRequest(http.MethodPost, h.fullURL("/ui/bots/"+strInt64(bot.ID)+"/update"),
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", csrf)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("update status = %d", resp.StatusCode)
	}

	got, err := h.deps.Bots.GetByID(context.Background(), tenantID, bot.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.BotToken != "12345:AAAAoriginalTok" {
		t.Fatalf("token mutated: %q (want original)", got.BotToken)
	}
	if got.Description != "edited-desc" {
		t.Fatalf("description = %q", got.Description)
	}
}

// TestUIBotsUpdate_NewTokenReplaces proves that supplying a non-empty
// bot_token in the edit form replaces the stored credential and that
// the masked view echoes the new tail (so the user can confirm via
// the table without re-rendering).
func TestUIBotsUpdate_NewTokenReplaces(t *testing.T) {
	h := newTestHarness(t)
	_, _ = h.seedAdmin("admin@example.com", "adminpass", "INV1")
	client, csrf := registerTenantAPI(t, h, "alice@example.com", "alicepass", "INV1")
	tenantID := meTenantID(t, h, client)

	bot := &domain.Bot{TenantID: tenantID, Name: "rotate-tok", Platform: domain.PlatformTelegram, BotToken: "12345:AAAAoldToken"}
	if err := h.deps.Bots.Insert(context.Background(), bot); err != nil {
		t.Fatalf("seed bot: %v", err)
	}

	form := url.Values{}
	form.Set("csrf", csrf)
	form.Set("name", bot.Name)
	form.Set("description", "")
	form.Set("platform", "telegram")
	form.Set("bot_token", "67890:BBBBnewToken9999")
	req, _ := http.NewRequest(http.MethodPost, h.fullURL("/ui/bots/"+strInt64(bot.ID)+"/update"),
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", csrf)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("update status = %d", resp.StatusCode)
	}

	got, err := h.deps.Bots.GetByID(context.Background(), tenantID, bot.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.BotToken != "67890:BBBBnewToken9999" {
		t.Fatalf("token not rotated: %q", got.BotToken)
	}
}

// TestUIBotsUpdate_DisabledBotKeepsListenerDown is the regression
// guard for the "edit a disabled bot must not silently spawn a
// listener" expectation. We disable the bot via the repo, edit the
// description, and assert the bot stays disabled. The platform
// Manager is unwired in the test harness so the listener wouldn't
// actually run regardless — what we are checking here is that the
// uiBotUpdate handler does not flip the Enabled column itself.
func TestUIBotsUpdate_DisabledBotKeepsListenerDown(t *testing.T) {
	h := newTestHarness(t)
	_, _ = h.seedAdmin("admin@example.com", "adminpass", "INV1")
	client, csrf := registerTenantAPI(t, h, "alice@example.com", "alicepass", "INV1")
	tenantID := meTenantID(t, h, client)

	bot := &domain.Bot{TenantID: tenantID, Name: "paused", Platform: domain.PlatformTelegram, BotToken: "12345:AAAAdisabledTok"}
	if err := h.deps.Bots.Insert(context.Background(), bot); err != nil {
		t.Fatalf("seed bot: %v", err)
	}
	if err := h.deps.Bots.SetEnabled(context.Background(), tenantID, bot.ID, false); err != nil {
		t.Fatalf("disable: %v", err)
	}

	form := url.Values{}
	form.Set("csrf", csrf)
	form.Set("name", bot.Name)
	form.Set("description", "renamed-while-disabled")
	form.Set("platform", "telegram")
	form.Set("bot_token", "67890:BBBBfreshRotation9")
	req, _ := http.NewRequest(http.MethodPost, h.fullURL("/ui/bots/"+strInt64(bot.ID)+"/update"),
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", csrf)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("update status = %d", resp.StatusCode)
	}

	got, err := h.deps.Bots.GetByID(context.Background(), tenantID, bot.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Enabled {
		t.Fatalf("update flipped Enabled back to true; want stay false")
	}
	if got.Description != "renamed-while-disabled" {
		t.Fatalf("description not updated: %q", got.Description)
	}
	if got.BotToken != "67890:BBBBfreshRotation9" {
		t.Fatalf("token not updated: %q", got.BotToken)
	}
}

// TestUIBotsUpdate_RejectsMalformedToken guards the validator: a
// bot_token field that is non-blank but ill-formatted must be rejected
// rather than persisted as-is. We expect 200 (the flash partial is
// rendered inline by the handler) with the error message visible.
func TestUIBotsUpdate_RejectsMalformedToken(t *testing.T) {
	h := newTestHarness(t)
	_, _ = h.seedAdmin("admin@example.com", "adminpass", "INV1")
	client, csrf := registerTenantAPI(t, h, "alice@example.com", "alicepass", "INV1")
	tenantID := meTenantID(t, h, client)

	bot := &domain.Bot{TenantID: tenantID, Name: "v", Platform: domain.PlatformTelegram, BotToken: "12345:AAAAvalidTok"}
	if err := h.deps.Bots.Insert(context.Background(), bot); err != nil {
		t.Fatalf("seed bot: %v", err)
	}

	form := url.Values{}
	form.Set("csrf", csrf)
	form.Set("name", bot.Name)
	form.Set("description", "")
	form.Set("platform", "telegram")
	form.Set("bot_token", "not-a-token")
	req, _ := http.NewRequest(http.MethodPost, h.fullURL("/ui/bots/"+strInt64(bot.ID)+"/update"),
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", csrf)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	bs, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if !strings.Contains(string(bs), "bot_token") {
		t.Fatalf("missing validation error: %s", string(bs)[:400])
	}

	// Token unchanged.
	got, err := h.deps.Bots.GetByID(context.Background(), tenantID, bot.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.BotToken != "12345:AAAAvalidTok" {
		t.Fatalf("token mutated despite invalid input: %q", got.BotToken)
	}
}

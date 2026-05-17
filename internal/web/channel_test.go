package web

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/wendi/pulseguard/internal/domain"
)

// seedBotAndTemplate inserts a bot + template owned by tenantID and
// returns their ids; callers use these to satisfy channel FK validation.
func seedBotAndTemplate(t *testing.T, h *testHarness, tenantID int64) (botID int64, tplID int64) {
	t.Helper()
	bot := &domain.Bot{TenantID: tenantID, Name: "bot-x", BotToken: "12345:abc"}
	if err := h.deps.Bots.Insert(context.Background(), bot); err != nil {
		t.Fatalf("insert bot: %v", err)
	}
	tpl := &domain.Template{
		TenantID: tenantID, Name: "tpl-x",
		ParseMode: domain.ParseNone, Body: "hi {{ .name }}",
	}
	if err := h.deps.Templates.Insert(context.Background(), tpl); err != nil {
		t.Fatalf("insert template: %v", err)
	}
	return bot.ID, tpl.ID
}

func TestChannelsAPILifecycle(t *testing.T) {
	h := newTestHarness(t)
	_, _ = h.seedAdmin("admin@example.com", "adminpass", "INV1")
	client, csrf := registerTenantAPI(t, h, "alice@example.com", "alicepass", "INV1")

	// Find alice's tenant id via /me.
	tenantID := meTenantID(t, h, client)
	botID, tplID := seedBotAndTemplate(t, h, tenantID)

	// Create channel.
	body := mustJSON(t, map[string]any{
		"name": "alerts", "bot_id": botID, "template_ids": []int64{tplID}, "default_template_id": tplID,
		"chat_id": "@grp", "rate_per_min": 30, "dedup_window_s": 60,
	})
	req, _ := http.NewRequest(http.MethodPost, h.fullURL("/api/v1/channels"), bytes.NewReader(body))
	resp, err := client.Do(withCSRF(req, csrf))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d body=%s", resp.StatusCode, drain(resp))
	}
	var created channelView
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	if created.PushToken == "" {
		t.Fatal("push_token not generated")
	}
	if !created.Enabled {
		t.Fatal("default enabled should be true")
	}
	originalToken := created.PushToken

	// Rotate token.
	req, _ = http.NewRequest(http.MethodPost, h.fullURL("/api/v1/channels/"+strInt64(created.ID)+"/rotate-token"), nil)
	resp, err = client.Do(withCSRF(req, csrf))
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("rotate status = %d body=%s", resp.StatusCode, drain(resp))
	}
	var rotated channelView
	_ = json.NewDecoder(resp.Body).Decode(&rotated)
	resp.Body.Close()
	if rotated.PushToken == "" || rotated.PushToken == originalToken {
		t.Fatalf("token not rotated: %q vs %q", rotated.PushToken, originalToken)
	}

	// Update enabled=false.
	upd := mustJSON(t, map[string]any{"enabled": false})
	req, _ = http.NewRequest(http.MethodPut, h.fullURL("/api/v1/channels/"+strInt64(created.ID)), bytes.NewReader(upd))
	resp, err = client.Do(withCSRF(req, csrf))
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("update status = %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Push via the new token → expect 410 (disabled).
	pushResp, err := http.Post(h.fullURL("/api/v1/push/"+rotated.PushToken),
		"application/json", strings.NewReader(`{"x":1}`))
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if pushResp.StatusCode != http.StatusGone {
		t.Fatalf("disabled push status = %d", pushResp.StatusCode)
	}
	pushResp.Body.Close()

	// Delete channel.
	req, _ = http.NewRequest(http.MethodDelete, h.fullURL("/api/v1/channels/"+strInt64(created.ID)), nil)
	resp, err = client.Do(withCSRF(req, csrf))
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status = %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestChannelsAPIRejectsCrossTenantFKs(t *testing.T) {
	h := newTestHarness(t)
	_, _ = h.seedAdmin("admin@example.com", "adminpass", "INV1")

	// Other tenant owns these resources.
	other := &domain.Tenant{Email: "other@example.com", PasswordHash: "x", Role: domain.RoleUser, Status: domain.TenantActive}
	_ = h.deps.Tenants.Insert(context.Background(), other)
	otherBotID, otherTplID := seedBotAndTemplate(t, h, other.ID)

	// Alice tries to attach to another tenant's resources.
	inv2 := &domain.InviteCode{Code: "INV2", CreatedBy: other.ID}
	_ = h.deps.Invites.Insert(context.Background(), inv2)
	client, csrf := registerTenantAPI(t, h, "alice@example.com", "alicepass", "INV2")

	body := mustJSON(t, map[string]any{
		"name": "x", "bot_id": otherBotID, "template_ids": []int64{otherTplID}, "default_template_id": otherTplID, "chat_id": "@g",
	})
	req, _ := http.NewRequest(http.MethodPost, h.fullURL("/api/v1/channels"), bytes.NewReader(body))
	resp, err := client.Do(withCSRF(req, csrf))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d (want 400 for cross-tenant FK)", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestUIChannelsRenders(t *testing.T) {
	h := newTestHarness(t)
	_, _ = h.seedAdmin("admin@example.com", "adminpass", "INV1")
	client, _ := registerTenantAPI(t, h, "alice@example.com", "alicepass", "INV1")

	resp, err := client.Get(h.fullURL("/ui/channels"))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	bs, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(bs), "新建通道") {
		t.Fatalf("body missing form: %s", string(bs)[:200])
	}
}

func meTenantID(t *testing.T, h *testHarness, c *http.Client) int64 {
	t.Helper()
	resp, err := c.Get(h.fullURL("/api/v1/me"))
	if err != nil {
		t.Fatalf("me: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("me status = %d", resp.StatusCode)
	}
	var b map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&b)
	resp.Body.Close()
	tenant, _ := b["tenant"].(map[string]any)
	idJSON, _ := tenant["id"].(float64)
	return int64(idJSON)
}

// TestChannelsAPIPersistsConditions creates a channel with two
// template bindings — one carrying a non-empty condition, one without
// — and verifies the API roundtrips the condition string verbatim
// through GET (creation response + subsequent fetch).
func TestChannelsAPIPersistsConditions(t *testing.T) {
	h := newTestHarness(t)
	_, _ = h.seedAdmin("admin@example.com", "adminpass", "INV1")
	client, csrf := registerTenantAPI(t, h, "alice@example.com", "alicepass", "INV1")
	tenantID := meTenantID(t, h, client)
	botID, tpl1 := seedBotAndTemplate(t, h, tenantID)
	// seedBotAndTemplate only creates one template; add a second so we
	// can prove ordering + condition pairing without duplicate-id noise.
	tpl2 := &domain.Template{TenantID: tenantID, Name: "tpl-y", ParseMode: domain.ParseNone, Body: "y"}
	if err := h.deps.Templates.Insert(context.Background(), tpl2); err != nil {
		t.Fatalf("insert second template: %v", err)
	}

	body := mustJSON(t, map[string]any{
		"name": "auto-route", "bot_id": botID, "chat_id": "@g",
		"template_ids":        []int64{tpl1, tpl2.ID},
		"default_template_id": tpl2.ID,
		"conditions":          []string{"level eq critical", ""},
	})
	req, _ := http.NewRequest(http.MethodPost, h.fullURL("/api/v1/channels"), bytes.NewReader(body))
	resp, err := client.Do(withCSRF(req, csrf))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d body=%s", resp.StatusCode, drain(resp))
	}
	var created channelView
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	if len(created.Templates) != 2 {
		t.Fatalf("created.Templates len = %d", len(created.Templates))
	}
	// Bindings come back ordered by sort_order; sort_order is assigned
	// in the same order we listed template_ids.
	if created.Templates[0].TemplateID != tpl1 || created.Templates[0].Condition != "level eq critical" {
		t.Fatalf("binding[0] = %+v want tpl1 + condition", created.Templates[0])
	}
	if created.Templates[1].TemplateID != tpl2.ID || created.Templates[1].Condition != "" || !created.Templates[1].IsDefault {
		t.Fatalf("binding[1] = %+v want tpl2 default + empty condition", created.Templates[1])
	}

	// Independent GET to prove DB persistence (not just response echo).
	req, _ = http.NewRequest(http.MethodGet, h.fullURL("/api/v1/channels/"+strInt64(created.ID)), nil)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get status = %d", resp.StatusCode)
	}
	var fetched channelView
	_ = json.NewDecoder(resp.Body).Decode(&fetched)
	resp.Body.Close()
	if fetched.Templates[0].Condition != "level eq critical" {
		t.Fatalf("fetched binding[0].Condition = %q", fetched.Templates[0].Condition)
	}
}

// TestChannelsAPIRejectsMalformedCondition is the validation guard:
// a condition string that fails to parse must produce a 400 before
// any DB write — silently dropping the typo would let it routes to
// nothing in production.
func TestChannelsAPIRejectsMalformedCondition(t *testing.T) {
	h := newTestHarness(t)
	_, _ = h.seedAdmin("admin@example.com", "adminpass", "INV1")
	client, csrf := registerTenantAPI(t, h, "alice@example.com", "alicepass", "INV1")
	tenantID := meTenantID(t, h, client)
	botID, tplID := seedBotAndTemplate(t, h, tenantID)

	body := mustJSON(t, map[string]any{
		"name": "bad-cond", "bot_id": botID, "chat_id": "@g",
		"template_ids": []int64{tplID},
		"conditions":   []string{"level zz critical"}, // unknown op zz
	})
	req, _ := http.NewRequest(http.MethodPost, h.fullURL("/api/v1/channels"), bytes.NewReader(body))
	resp, err := client.Do(withCSRF(req, csrf))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d want 400; body=%s", resp.StatusCode, drain(resp))
	}
	var env apiError
	_ = json.NewDecoder(resp.Body).Decode(&env)
	if env.Error.Code != "VALIDATION" {
		t.Fatalf("code = %q want VALIDATION", env.Error.Code)
	}
	if !strings.Contains(env.Error.Message, "condition") {
		t.Fatalf("message = %q want mention of condition", env.Error.Message)
	}
}

// TestChannelsAPIUpdateReplacesConditions exercises the PUT path so
// we know an edit can swap conditions alongside template_ids without
// dropping or silently blanking them.
func TestChannelsAPIUpdateReplacesConditions(t *testing.T) {
	h := newTestHarness(t)
	_, _ = h.seedAdmin("admin@example.com", "adminpass", "INV1")
	client, csrf := registerTenantAPI(t, h, "alice@example.com", "alicepass", "INV1")
	tenantID := meTenantID(t, h, client)
	botID, tplID := seedBotAndTemplate(t, h, tenantID)

	body := mustJSON(t, map[string]any{
		"name": "upd", "bot_id": botID, "chat_id": "@g",
		"template_ids": []int64{tplID},
	})
	req, _ := http.NewRequest(http.MethodPost, h.fullURL("/api/v1/channels"), bytes.NewReader(body))
	resp, err := client.Do(withCSRF(req, csrf))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d body=%s", resp.StatusCode, drain(resp))
	}
	var created channelView
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()

	upd := mustJSON(t, map[string]any{
		"template_ids":        []int64{tplID},
		"default_template_id": tplID,
		"conditions":          []string{"host startswith db-"},
	})
	req, _ = http.NewRequest(http.MethodPut, h.fullURL("/api/v1/channels/"+strInt64(created.ID)), bytes.NewReader(upd))
	resp, err = client.Do(withCSRF(req, csrf))
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("update status = %d body=%s", resp.StatusCode, drain(resp))
	}
	var updated channelView
	_ = json.NewDecoder(resp.Body).Decode(&updated)
	resp.Body.Close()
	if len(updated.Templates) != 1 {
		t.Fatalf("templates len = %d", len(updated.Templates))
	}
	if updated.Templates[0].Condition != "host startswith db-" {
		t.Fatalf("Condition = %q after update", updated.Templates[0].Condition)
	}
}

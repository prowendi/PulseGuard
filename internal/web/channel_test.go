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

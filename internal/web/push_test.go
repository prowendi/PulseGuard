package web

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/wendi/pulseguard/internal/domain"
)

// seedTenantBotTemplateChannel builds a minimal end-to-end fixture so
// push tests can hit a real channel.
func seedTenantBotTemplateChannel(t *testing.T, h *testHarness, pushToken string, dedupWindow int, enabled bool) (*domain.Tenant, *domain.Channel) {
	t.Helper()
	tenant := &domain.Tenant{
		Email: "tester@example.com", PasswordHash: "x",
		Role: domain.RoleUser, Status: domain.TenantActive,
	}
	if err := h.deps.Tenants.Insert(context.Background(), tenant); err != nil {
		t.Fatalf("insert tenant: %v", err)
	}
	bot := &domain.Bot{
		TenantID: tenant.ID, Name: "bot-A", BotToken: "12345:abc",
	}
	if err := h.deps.Bots.Insert(context.Background(), bot); err != nil {
		t.Fatalf("insert bot: %v", err)
	}
	tpl := &domain.Template{
		TenantID: tenant.ID, Name: "tpl-A",
		ParseMode: domain.ParseNone, Body: "{{ .title }}",
	}
	if err := h.deps.Templates.Insert(context.Background(), tpl); err != nil {
		t.Fatalf("insert template: %v", err)
	}
	ch := &domain.Channel{
		TenantID: tenant.ID, Name: "ch-A",
		PushToken: pushToken, BotID: bot.ID, TemplateID: tpl.ID,
		ChatID: "chat-123", RatePerMin: 60,
		DedupWindowS: dedupWindow, Enabled: enabled,
	}
	if err := h.deps.Channels.Insert(context.Background(), ch); err != nil {
		t.Fatalf("insert channel: %v", err)
	}
	return tenant, ch
}

func TestPushUnknownTokenReturns404(t *testing.T) {
	h := newTestHarness(t)
	resp, err := http.Post(h.fullURL("/api/v1/push/no-such-token"),
		"application/json", bytes.NewReader([]byte(`{"x":1}`)))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestPushDisabledChannelReturns410(t *testing.T) {
	h := newTestHarness(t)
	seedTenantBotTemplateChannel(t, h, "tok-disabled", 0, false /*enabled*/)
	resp, err := http.Post(h.fullURL("/api/v1/push/tok-disabled"),
		"application/json", bytes.NewReader([]byte(`{"title":"hi"}`)))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	if resp.StatusCode != http.StatusGone {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestPushBadJSONReturns400(t *testing.T) {
	h := newTestHarness(t)
	seedTenantBotTemplateChannel(t, h, "tok-bad", 0, true)
	resp, err := http.Post(h.fullURL("/api/v1/push/tok-bad"),
		"application/json", bytes.NewReader([]byte(`not-json`)))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestPushHappyPathReturns202(t *testing.T) {
	h := newTestHarness(t)
	seedTenantBotTemplateChannel(t, h, "tok-good", 0, true)
	resp, err := http.Post(h.fullURL("/api/v1/push/tok-good"),
		"application/json", bytes.NewReader([]byte(`{"title":"hello"}`)))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d body=%s", resp.StatusCode, drain(resp))
	}
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	resp.Body.Close()
	if body["status"] != "queued" {
		t.Fatalf("status field = %v", body["status"])
	}
	if _, ok := body["push_id"]; !ok {
		t.Fatal("missing push_id")
	}
}

func TestPushDedupDropsSecondHit(t *testing.T) {
	h := newTestHarness(t)
	seedTenantBotTemplateChannel(t, h, "tok-dedup", 60 /*windowSec*/, true)
	body := []byte(`{"title":"alert","dedup_key":"dk1"}`)

	// First push: 202 queued.
	resp, err := http.Post(h.fullURL("/api/v1/push/tok-dedup"),
		"application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("first push: %v", err)
	}
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("first push status = %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Second push within window: 200 dropped=true.
	h.clock.Advance(time.Second)
	resp, err = http.Post(h.fullURL("/api/v1/push/tok-dedup"),
		"application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("second push: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("second push status = %d body=%s", resp.StatusCode, drain(resp))
	}
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if out["dropped"] != true {
		t.Fatalf("dropped flag = %v", out["dropped"])
	}
	if out["reason"] != "dedup" {
		t.Fatalf("reason = %v", out["reason"])
	}
}

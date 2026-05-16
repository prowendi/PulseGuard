package web

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/wendi/pulseguard/internal/domain"
)

// seedLogs inserts n synthetic push_logs for tenantID/channelID with
// alternating sent/failed status so list / filter / pagination paths
// have data to traverse.
func seedLogs(t *testing.T, h *testHarness, tenantID, channelID int64, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		status := domain.LogSent
		if i%2 == 1 {
			status = domain.LogFailed
		}
		msgID := int64(1000 + i)
		var errPtr *string
		if status == domain.LogFailed {
			s := "synthetic error"
			errPtr = &s
		}
		l := &domain.PushLog{
			ChannelID:    channelID,
			TenantID:     tenantID,
			PayloadJSON:  `{"x":1}`,
			RenderedText: "rendered",
			TGMessageID:  &msgID,
			Status:       status,
			Error:        errPtr,
			Attempts:     1,
		}
		if err := h.deps.Logs.Insert(context.Background(), l); err != nil {
			t.Fatalf("seed log %d: %v", i, err)
		}
	}
}

func TestLogsAPIListAndPaginate(t *testing.T) {
	h := newTestHarness(t)
	_, _ = h.seedAdmin("admin@example.com", "adminpass", "INV1")
	client, _ := registerTenantAPI(t, h, "alice@example.com", "alicepass", "INV1")

	tenantID := meTenantID(t, h, client)
	botID, tplID := seedBotAndTemplate(t, h, tenantID)
	ch := &domain.Channel{
		TenantID: tenantID, Name: "ch-logs",
		PushToken: "tok-logs", BotID: botID,
		ChatID: "c1", RatePerMin: 60, Enabled: true,
		Templates: []*domain.ChannelTemplate{{TemplateID: tplID, IsDefault: true}},
	}
	if err := h.deps.Channels.Insert(context.Background(), ch); err != nil {
		t.Fatalf("seed channel: %v", err)
	}
	seedLogs(t, h, tenantID, ch.ID, 5)

	// Default list.
	resp, err := client.Get(h.fullURL("/api/v1/logs"))
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.StatusCode, drain(resp))
	}
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	resp.Body.Close()
	items, _ := body["items"].([]any)
	if len(items) != 5 {
		t.Fatalf("items = %d, want 5", len(items))
	}
	if total, _ := body["total"].(float64); int(total) != 5 {
		t.Fatalf("total = %v", body["total"])
	}

	// per_page=2 should clamp to that page size.
	resp, err = client.Get(h.fullURL("/api/v1/logs?per_page=2&page=1"))
	if err != nil {
		t.Fatalf("paginate: %v", err)
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	resp.Body.Close()
	items, _ = body["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("paginated items = %d", len(items))
	}

	// per_page above max (200) clamps; we just check the cap value
	// echoed in the JSON response.
	resp, _ = client.Get(h.fullURL("/api/v1/logs?per_page=1000"))
	_ = json.NewDecoder(resp.Body).Decode(&body)
	resp.Body.Close()
	if pp, _ := body["per_page"].(float64); int(pp) != 200 {
		t.Fatalf("per_page clamp = %v", body["per_page"])
	}
}

func TestLogsAPIChannelFilter(t *testing.T) {
	h := newTestHarness(t)
	_, _ = h.seedAdmin("admin@example.com", "adminpass", "INV1")
	client, _ := registerTenantAPI(t, h, "alice@example.com", "alicepass", "INV1")
	tenantID := meTenantID(t, h, client)
	botID, tplID := seedBotAndTemplate(t, h, tenantID)

	// Two channels, only logs for chA exist.
	chA := &domain.Channel{
		TenantID: tenantID, Name: "A", PushToken: "tA",
		BotID: botID,
		ChatID: "c1", RatePerMin: 60, Enabled: true,
		Templates: []*domain.ChannelTemplate{{TemplateID: tplID, IsDefault: true}},
	}
	chB := &domain.Channel{
		TenantID: tenantID, Name: "B", PushToken: "tB",
		BotID: botID,
		ChatID: "c2", RatePerMin: 60, Enabled: true,
		Templates: []*domain.ChannelTemplate{{TemplateID: tplID, IsDefault: true}},
	}
	_ = h.deps.Channels.Insert(context.Background(), chA)
	_ = h.deps.Channels.Insert(context.Background(), chB)
	seedLogs(t, h, tenantID, chA.ID, 3)

	// Filter on chB -> expect 0.
	resp, err := client.Get(h.fullURL("/api/v1/logs?channel_id=" + strInt64(chB.ID)))
	if err != nil {
		t.Fatalf("filter: %v", err)
	}
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	resp.Body.Close()
	items, _ := body["items"].([]any)
	if len(items) != 0 {
		t.Fatalf("chB items = %d, want 0", len(items))
	}

	// Filter on chA -> 3.
	resp, _ = client.Get(h.fullURL("/api/v1/logs?channel_id=" + strInt64(chA.ID)))
	_ = json.NewDecoder(resp.Body).Decode(&body)
	resp.Body.Close()
	items, _ = body["items"].([]any)
	if len(items) != 3 {
		t.Fatalf("chA items = %d", len(items))
	}
}

func TestLogsAPIRejectsCrossTenant(t *testing.T) {
	h := newTestHarness(t)
	_, _ = h.seedAdmin("admin@example.com", "adminpass", "INV1")

	other := &domain.Tenant{Email: "o@x", PasswordHash: "x", Role: domain.RoleUser, Status: domain.TenantActive}
	_ = h.deps.Tenants.Insert(context.Background(), other)
	otherBot, otherTpl := seedBotAndTemplate(t, h, other.ID)
	otherCh := &domain.Channel{
		TenantID: other.ID, Name: "X", PushToken: "tOther",
		BotID: otherBot,
		ChatID: "c", RatePerMin: 60, Enabled: true,
		Templates: []*domain.ChannelTemplate{{TemplateID: otherTpl, IsDefault: true}},
	}
	_ = h.deps.Channels.Insert(context.Background(), otherCh)
	seedLogs(t, h, other.ID, otherCh.ID, 4)

	inv2 := &domain.InviteCode{Code: "INV2", CreatedBy: other.ID}
	_ = h.deps.Invites.Insert(context.Background(), inv2)
	client, _ := registerTenantAPI(t, h, "alice@example.com", "alicepass", "INV2")

	// Alice queries logs — should see none.
	resp, _ := client.Get(h.fullURL("/api/v1/logs"))
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	resp.Body.Close()
	if items, _ := body["items"].([]any); len(items) != 0 {
		t.Fatalf("cross-tenant leak: %d items", len(items))
	}
}

func TestUILogsRenders(t *testing.T) {
	h := newTestHarness(t)
	_, _ = h.seedAdmin("admin@example.com", "adminpass", "INV1")
	client, _ := registerTenantAPI(t, h, "alice@example.com", "alicepass", "INV1")

	resp, err := client.Get(h.fullURL("/ui/logs"))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	bs, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(bs), "推送日志") {
		t.Fatalf("body missing heading: %s", string(bs)[:200])
	}
}

package web

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/prowendi/PulseGuard/internal/domain"
)

// seedDLQ inserts n synthetic dead_letters rows for the given channel.
func seedDLQ(t *testing.T, h *testHarness, tenantID, channelID int64, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		dl := &domain.DeadLetter{
			OutboxID:    int64(1000 + i),
			ChannelID:   channelID,
			TenantID:    tenantID,
			PayloadJSON: `{"k":"v"}`,
			LastError:   "synthetic perm",
			Attempts:    6,
		}
		if err := h.deps.DLQ.Insert(context.Background(), dl); err != nil {
			t.Fatalf("seed dlq %d: %v", i, err)
		}
	}
}

func TestDLQAPIListAndReplay(t *testing.T) {
	h := newTestHarness(t)
	_, _ = h.seedAdmin("admin@example.com", "adminpass", "INV1")
	client, csrf := registerTenantAPI(t, h, "alice@example.com", "alicepass", "INV1")
	tenantID := meTenantID(t, h, client)
	botID, tplID := seedBotAndTemplate(t, h, tenantID)
	ch := &domain.Channel{
		TenantID: tenantID, Name: "dlq-ch", PushToken: "tDLQ",
		BotID: botID, ChatID: "c", RatePerMin: 60, Enabled: true,
		Templates: []*domain.ChannelTemplate{{TemplateID: tplID, IsDefault: true}},
	}
	if err := h.deps.Channels.Insert(context.Background(), ch); err != nil {
		t.Fatalf("seed channel: %v", err)
	}
	seedDLQ(t, h, tenantID, ch.ID, 3)

	// List.
	resp, err := client.Get(h.fullURL("/api/v1/deadletters"))
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
	if len(items) != 3 {
		t.Fatalf("dlq items = %d", len(items))
	}
	firstID := int64(items[0].(map[string]any)["id"].(float64))

	// Replay first item.
	req, _ := http.NewRequest(http.MethodPost,
		h.fullURL("/api/v1/deadletters/"+strInt64(firstID)+"/replay"), nil)
	resp, err = client.Do(withCSRF(req, csrf))
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("replay status = %d body=%s", resp.StatusCode, drain(resp))
	}
	var rep map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&rep)
	resp.Body.Close()
	if rep["status"] != "queued" {
		t.Fatalf("status field = %v", rep["status"])
	}
	if newID, _ := rep["new_outbox_id"].(float64); newID <= 0 {
		t.Fatalf("new_outbox_id = %v", rep["new_outbox_id"])
	}

	// Confirm push_outbox got a fresh pending row (status='pending').
	var count int
	err = h.db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM push_outbox WHERE channel_id = ? AND status = 'pending'`,
		ch.ID).Scan(&count)
	if err != nil {
		if err != sql.ErrNoRows {
			t.Fatalf("count outbox: %v", err)
		}
	}
	if count != 1 {
		t.Fatalf("expected 1 pending outbox row, got %d", count)
	}
}

func TestDLQReplayCrossTenantReturns404(t *testing.T) {
	h := newTestHarness(t)
	_, _ = h.seedAdmin("admin@example.com", "adminpass", "INV1")

	other := &domain.Tenant{Email: "o@x", PasswordHash: "x", Role: domain.RoleUser, Status: domain.TenantActive}
	_ = h.deps.Tenants.Insert(context.Background(), other)
	otherBot, otherTpl := seedBotAndTemplate(t, h, other.ID)
	otherCh := &domain.Channel{
		TenantID: other.ID, Name: "Y", PushToken: "tOther",
		BotID: otherBot,
		ChatID: "c", RatePerMin: 60, Enabled: true,
		Templates: []*domain.ChannelTemplate{{TemplateID: otherTpl, IsDefault: true}},
	}
	_ = h.deps.Channels.Insert(context.Background(), otherCh)
	seedDLQ(t, h, other.ID, otherCh.ID, 1)

	// Find other's DLQ id.
	rows, _, _ := h.deps.DLQ.ListByTenant(context.Background(), other.ID, 1, 10)
	if len(rows) == 0 {
		t.Fatal("seed dlq did not insert anything")
	}
	otherDLID := rows[0].ID

	inv2 := &domain.InviteCode{Code: "INV2", CreatedBy: other.ID}
	_ = h.deps.Invites.Insert(context.Background(), inv2)
	client, csrf := registerTenantAPI(t, h, "alice@example.com", "alicepass", "INV2")

	req, _ := http.NewRequest(http.MethodPost,
		h.fullURL("/api/v1/deadletters/"+strInt64(otherDLID)+"/replay"), nil)
	resp, err := client.Do(withCSRF(req, csrf))
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-tenant replay status = %d, expected 404", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestUIDeadlettersRenders(t *testing.T) {
	h := newTestHarness(t)
	_, _ = h.seedAdmin("admin@example.com", "adminpass", "INV1")
	client, _ := registerTenantAPI(t, h, "alice@example.com", "alicepass", "INV1")

	resp, err := client.Get(h.fullURL("/ui/deadletters"))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	bs, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(bs), "死信队列") {
		t.Fatalf("body missing heading: %s", string(bs)[:200])
	}
}

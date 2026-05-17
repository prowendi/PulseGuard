package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// seedTelegramBot creates a single Telegram bot through the public
// JSON API so subsequent /api/v1/commands inserts have a bot to
// bind to. Returns the bot's id.
func seedTelegramBot(t *testing.T, h *testHarness, client *http.Client, csrf string, name string) int64 {
	t.Helper()
	body := mustJSON(t, map[string]any{
		"name":      name,
		"platform":  "telegram",
		"bot_token": "1:secret",
	})
	req, _ := http.NewRequest(http.MethodPost, h.fullURL("/api/v1/bots"), bytes.NewReader(body))
	resp, err := client.Do(withCSRF(req, csrf))
	if err != nil {
		t.Fatalf("seed bot: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("seed bot status=%d body=%s", resp.StatusCode, drain(resp))
	}
	var v botView
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatalf("decode bot: %v", err)
	}
	return v.ID
}

// TestCommandsAPI_CodeSizeLimit_RejectsOverCap proves that a Starlark
// source larger than MaxCommandCodeBytes (64 KiB) is rejected with a
// 400 VALIDATION at create time. The Starlark compiler is single
// threaded per script, so an unbounded source lets a tenant pin a
// worker — the cap is the cheapest mitigation.
//
// Refs: round2-security-report S2-M1.
func TestCommandsAPI_CodeSizeLimit_RejectsOverCap(t *testing.T) {
	h := newTestHarness(t)
	_, _ = h.seedAdmin("admin@example.com", "adminpass", "INV1")
	client, csrf := registerTenantAPI(t, h, "alice@example.com", "alicepass", "INV1")
	botID := seedTelegramBot(t, h, client, csrf, "cmd-bot")

	// 70 KiB of source (> 64 KiB cap). The exact contents are
	// irrelevant — we never reach the Starlark compiler.
	over := "def handle(args):\n    return \"x\"\n" +
		"# " + strings.Repeat("a", 70*1024) + "\n"
	body := mustJSON(t, map[string]any{
		"bot_id": botID,
		"name":   "too-big",
		"code":   over,
	})
	req, _ := http.NewRequest(http.MethodPost, h.fullURL("/api/v1/commands"),
		bytes.NewReader(body))
	resp, err := client.Do(withCSRF(req, csrf))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s, want 400", resp.StatusCode, drain(resp))
	}
}

// TestCommandsAPI_CodeSizeLimit_AcceptsExactCap is the boundary test:
// a 64 KiB body (right at MaxCommandCodeBytes) must succeed. This
// guards against off-by-one regressions that would flip the boundary
// to "<" rather than ">".
func TestCommandsAPI_CodeSizeLimit_AcceptsExactCap(t *testing.T) {
	h := newTestHarness(t)
	_, _ = h.seedAdmin("admin@example.com", "adminpass", "INV1")
	client, csrf := registerTenantAPI(t, h, "alice@example.com", "alicepass", "INV1")
	botID := seedTelegramBot(t, h, client, csrf, "cmd-bot")

	const header = "def handle(args):\n    return \"x\"\n"
	pad := MaxCommandCodeBytes - len(header) - len("# \n")
	if pad < 1 {
		t.Fatalf("cap too small for header (%d bytes)", MaxCommandCodeBytes)
	}
	exact := header + "# " + strings.Repeat("a", pad) + "\n"
	if len(exact) != MaxCommandCodeBytes {
		t.Fatalf("payload size = %d, want %d", len(exact), MaxCommandCodeBytes)
	}
	body := mustJSON(t, map[string]any{
		"bot_id": botID,
		"name":   "boundary",
		"code":   exact,
	})
	req, _ := http.NewRequest(http.MethodPost, h.fullURL("/api/v1/commands"),
		bytes.NewReader(body))
	resp, err := client.Do(withCSRF(req, csrf))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d body=%s, want 201", resp.StatusCode, drain(resp))
	}
}

// TestCommandsAPI_CodeSizeLimit_UpdateAlsoCapped guards the update
// path: a tenant must not be able to grow an existing command past
// the cap via PUT.
func TestCommandsAPI_CodeSizeLimit_UpdateAlsoCapped(t *testing.T) {
	h := newTestHarness(t)
	_, _ = h.seedAdmin("admin@example.com", "adminpass", "INV1")
	client, csrf := registerTenantAPI(t, h, "alice@example.com", "alicepass", "INV1")
	botID := seedTelegramBot(t, h, client, csrf, "cmd-bot")

	// Create a tiny command first.
	createBody := mustJSON(t, map[string]any{
		"bot_id": botID,
		"name":   "growable",
		"code":   "def handle(args):\n    return \"tiny\"\n",
	})
	req, _ := http.NewRequest(http.MethodPost, h.fullURL("/api/v1/commands"),
		bytes.NewReader(createBody))
	resp, err := client.Do(withCSRF(req, csrf))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("seed create status = %d body=%s", resp.StatusCode, drain(resp))
	}
	var created commandView
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()

	// Then PUT with > 64 KiB code → 400. We use the id from the
	// create response so the test is robust against schema seeding
	// reordering.
	over := "def handle(args):\n    return \"x\"\n# " +
		strings.Repeat("z", 70*1024) + "\n"
	updBody := mustJSON(t, map[string]any{
		"code": over,
	})
	updReq, _ := http.NewRequest(http.MethodPut,
		h.fullURL("/api/v1/commands/"+strconv.FormatInt(created.ID, 10)), bytes.NewReader(updBody))
	updResp, err := client.Do(withCSRF(updReq, csrf))
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	defer updResp.Body.Close()
	if updResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("update status = %d body=%s, want 400",
			updResp.StatusCode, drain(updResp))
	}
}

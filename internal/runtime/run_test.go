package runtime

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wendi/pulseguard/internal/config"
	"github.com/wendi/pulseguard/internal/domain"
)

// fakeSender is a goroutine-safe domain.Sender that records every call
// and always succeeds. The integration test polls (*fakeSender).count()
// to confirm the worker actually dispatched a message.
type fakeSender struct {
	mu   sync.Mutex
	sent []sentMsg
}

type sentMsg struct {
	BotToken  string
	ChatID    string
	ParseMode string
	Text      string
}

func (f *fakeSender) Send(_ context.Context, botToken, chatID, parseMode, text string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, sentMsg{BotToken: botToken, ChatID: chatID, ParseMode: parseMode, Text: text})
	return int64(len(f.sent)), nil
}

func (f *fakeSender) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

func (f *fakeSender) snapshot() []sentMsg {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]sentMsg, len(f.sent))
	copy(out, f.sent)
	return out
}

// makeMasterKey returns a fresh base64-encoded 32-byte key suitable for
// the AES-GCM cipher used by store.NewCipher.
func makeMasterKey(t *testing.T) string {
	t.Helper()
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	return base64.StdEncoding.EncodeToString(b)
}

// newTestConfig builds a self-contained Config wired to a temp SQLite
// file and listening on :0 (kernel picks port). Cookie-secure off so
// the cookiejar accepts cookies over http.
func newTestConfig(t *testing.T, adminEmail, adminPassword string) *config.Config {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "pulseguard.db")
	return &config.Config{
		Server: config.Server{
			ListenAddr:      "127.0.0.1:0",
			BaseURL:         "http://127.0.0.1:0",
			ReadTimeout:     config.Duration(5 * time.Second),
			WriteTimeout:    config.Duration(5 * time.Second),
			ShutdownTimeout: config.Duration(3 * time.Second),
		},
		Database: config.Database{
			Path:        dbPath,
			BusyTimeout: config.Duration(5 * time.Second),
		},
		Security: config.Security{
			MasterKeyB64: makeMasterKey(t),
			SessionTTL:   config.Duration(24 * time.Hour),
			CookieSecure: false,
			BcryptCost:   4, // fast for tests
		},
		Worker: config.Worker{
			Count:                2,
			PollInterval:         config.Duration(50 * time.Millisecond),
			MaxAttempts:          3,
			InflightReclaimAfter: config.Duration(60 * time.Second),
			BackoffSchedule: []config.Duration{
				config.Duration(50 * time.Millisecond),
				config.Duration(100 * time.Millisecond),
				config.Duration(200 * time.Millisecond),
			},
		},
		Telegram: config.Telegram{
			APIBase:     "http://127.0.0.1:1", // unused (fake sender), but keep non-empty
			HTTPTimeout: config.Duration(2 * time.Second),
		},
		Bootstrap: config.Bootstrap{
			InitialAdminEmail:    adminEmail,
			InitialAdminPassword: adminPassword,
		},
		Logging: config.Logging{Level: "warn", Format: "text"},
		Cleanup: config.Cleanup{
			PushLogsKeepDays:       7,
			DedupKeysSweepInterval: config.Duration(time.Hour),
			SessionsSweepInterval:  config.Duration(time.Hour),
		},
	}
}

// eventually polls cond until it returns true or timeout elapses. Tests
// MUST use eventually rather than time.Sleep so flakiness is bounded by
// a clear deadline instead of a guess.
func eventually(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("eventually: condition not met within %s", timeout)
}

// testLogger returns a slog logger that funnels into t.Log so failed
// tests show every log line.
func testLogger(t *testing.T) *slog.Logger {
	return slog.New(slog.NewTextHandler(testWriter{t}, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// httpClient returns a cookiejar-backed client that follows no
// redirects (so tests can assert on Location headers).
func httpClient(t *testing.T) *http.Client {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	return &http.Client{
		Jar:     jar,
		Timeout: 3 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func jarCookie(t *testing.T, c *http.Client, base, name string) string {
	t.Helper()
	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse %q: %v", base, err)
	}
	for _, ck := range c.Jar.Cookies(u) {
		if ck.Name == name {
			return ck.Value
		}
	}
	return ""
}

// readErr drains an http.Response and returns a string for log output.
func readErr(resp *http.Response) string {
	bs, _ := io.ReadAll(resp.Body)
	return string(bs)
}

// startRuntime spawns runtime.RunWithDeps in a goroutine, blocks until
// the HTTP listener address is known + the runtime signals readiness,
// and returns the base URL + a stop func that cancels the runtime and
// waits for it to return.
func startRuntime(t *testing.T, cfg *config.Config, sender domain.Sender) (string, *fakeSender, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	addrCh := make(chan net.Addr, 1)
	readyCh := make(chan struct{})
	doneCh := make(chan error, 1)

	fs, _ := sender.(*fakeSender)

	go func() {
		doneCh <- RunWithDeps(ctx, cfg, testLogger(t), Overrides{
			Sender:     sender,
			ListenerCh: addrCh,
			ReadyCh:    readyCh,
		})
	}()

	var addr net.Addr
	select {
	case addr = <-addrCh:
	case err := <-doneCh:
		cancel()
		t.Fatalf("runtime exited before binding: %v", err)
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("timed out waiting for listener address")
	}

	select {
	case <-readyCh:
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("timed out waiting for runtime readiness")
	}

	base := "http://" + addr.String()

	// Health probe: wait for /healthz to return 200 before any test logic.
	eventually(t, 3*time.Second, func() bool {
		resp, err := http.Get(base + "/healthz")
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	})

	stop := func() {
		cancel()
		select {
		case err := <-doneCh:
			if err != nil {
				t.Logf("runtime returned: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for runtime shutdown")
		}
	}
	return base, fs, stop
}

// TestRunHealthAndShutdown is the smallest possible smoke: start the
// runtime, hit /healthz, send SIGTERM (via ctx cancel), confirm clean
// exit.
func TestRunHealthAndShutdown(t *testing.T) {
	cfg := newTestConfig(t, "admin@example.com", "adminpass")
	base, _, stop := startRuntime(t, cfg, &fakeSender{})
	defer stop()

	resp, err := http.Get(base + "/healthz")
	if err != nil {
		t.Fatalf("healthz: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz status = %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestRunBootstrapMissingCredsFails confirms fail-loud behaviour on an
// empty database without admin creds.
func TestRunBootstrapMissingCredsFails(t *testing.T) {
	cfg := newTestConfig(t, "", "") // both empty → bootstrap fails

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := RunWithDeps(ctx, cfg, testLogger(t), Overrides{Sender: &fakeSender{}})
	if err == nil {
		t.Fatal("expected error when bootstrap creds missing")
	}
	if !strings.Contains(err.Error(), "bootstrap") {
		t.Fatalf("error should mention bootstrap, got: %v", err)
	}
}

// TestRunEndToEnd covers the full lifecycle: bootstrap admin → login
// → create invite → register user with invite → user creates
// bot/template/channel → push → worker drains → log appears →
// shutdown.
func TestRunEndToEnd(t *testing.T) {
	cfg := newTestConfig(t, "admin@example.com", "adminpass")
	sender := &fakeSender{}
	base, fs, stop := startRuntime(t, cfg, sender)
	defer stop()

	// ── 1. Admin login.
	adminC := httpClient(t)
	loginBody := mustJSON(t, map[string]any{"email": "admin@example.com", "password": "adminpass"})
	resp := postJSON(t, adminC, base+"/api/v1/auth/login", loginBody, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin login: %d body=%s", resp.StatusCode, readErr(resp))
	}
	resp.Body.Close()
	adminCSRF := jarCookie(t, adminC, base, "psg_csrf")
	if adminCSRF == "" {
		t.Fatal("admin csrf cookie missing")
	}

	// ── 2. Admin hits /api/v1/me to verify session.
	req, _ := http.NewRequest(http.MethodGet, base+"/api/v1/me", nil)
	resp, err := adminC.Do(req)
	if err != nil {
		t.Fatalf("admin me: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin me status=%d", resp.StatusCode)
	}
	var meBody map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&meBody)
	resp.Body.Close()
	tenantObj, _ := meBody["tenant"].(map[string]any)
	if role, _ := tenantObj["role"].(string); role != "admin" {
		t.Fatalf("admin role = %q", role)
	}

	// ── 3. Admin creates an invite.
	inviteResp := postJSON(t, adminC, base+"/api/v1/invites",
		mustJSON(t, map[string]any{"count": 1, "ttl_seconds": 3600}), adminCSRF)
	if inviteResp.StatusCode != http.StatusCreated {
		t.Fatalf("create invite: %d body=%s", inviteResp.StatusCode, readErr(inviteResp))
	}
	var inviteBody map[string]any
	_ = json.NewDecoder(inviteResp.Body).Decode(&inviteBody)
	inviteResp.Body.Close()
	items, _ := inviteBody["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("invite items len = %d", len(items))
	}
	inviteCode, _ := items[0].(map[string]any)["code"].(string)
	if inviteCode == "" {
		t.Fatal("invite code missing in response")
	}

	// ── 4. User registers with that invite.
	userC := httpClient(t)
	registerBody := mustJSON(t, map[string]any{
		"email":       "alice@example.com",
		"password":    "alicepass",
		"invite_code": inviteCode,
	})
	regResp := postJSON(t, userC, base+"/api/v1/auth/register", registerBody, "")
	if regResp.StatusCode != http.StatusCreated {
		t.Fatalf("register: %d body=%s", regResp.StatusCode, readErr(regResp))
	}
	regResp.Body.Close()
	userCSRF := jarCookie(t, userC, base, "psg_csrf")
	if userCSRF == "" {
		t.Fatal("user csrf cookie missing")
	}

	// ── 5. User creates bot, template, channel.
	botResp := postJSON(t, userC, base+"/api/v1/bots",
		mustJSON(t, map[string]any{
			"name":     "alice-bot",
			"bot_token": "111111:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		}), userCSRF)
	if botResp.StatusCode != http.StatusCreated {
		t.Fatalf("bot create: %d body=%s", botResp.StatusCode, readErr(botResp))
	}
	var botBody map[string]any
	_ = json.NewDecoder(botResp.Body).Decode(&botBody)
	botResp.Body.Close()
	botID := int64(botBody["id"].(float64))

	tplResp := postJSON(t, userC, base+"/api/v1/templates",
		mustJSON(t, map[string]any{
			"name":       "alert",
			"parse_mode": "None",
			"body":       "Alert: {{ .title }} on {{ .host }}",
		}), userCSRF)
	if tplResp.StatusCode != http.StatusCreated {
		t.Fatalf("template create: %d body=%s", tplResp.StatusCode, readErr(tplResp))
	}
	var tplBody map[string]any
	_ = json.NewDecoder(tplResp.Body).Decode(&tplBody)
	tplResp.Body.Close()
	tplID := int64(tplBody["id"].(float64))

	chResp := postJSON(t, userC, base+"/api/v1/channels",
		mustJSON(t, map[string]any{
			"name":                "primary",
			"bot_id":              botID,
			"template_ids":        []int64{tplID},
			"default_template_id": tplID,
			"chat_id":             "12345",
			"rate_per_min":        60,
			"dedup_window_s":      0,
			"enabled":             true,
		}), userCSRF)
	if chResp.StatusCode != http.StatusCreated {
		t.Fatalf("channel create: %d body=%s", chResp.StatusCode, readErr(chResp))
	}
	var chBody map[string]any
	_ = json.NewDecoder(chResp.Body).Decode(&chBody)
	chResp.Body.Close()
	pushToken, _ := chBody["push_token"].(string)
	channelID := int64(chBody["id"].(float64))
	if pushToken == "" {
		t.Fatal("push_token missing in channel response")
	}

	// ── 6. Push a payload (no session needed; bearer = push_token).
	pushResp, err := http.Post(base+"/api/v1/push/"+pushToken,
		"application/json",
		bytes.NewReader([]byte(`{"title":"CPU high","host":"db01"}`)))
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if pushResp.StatusCode != http.StatusAccepted {
		t.Fatalf("push status = %d body=%s", pushResp.StatusCode, readErr(pushResp))
	}
	pushResp.Body.Close()

	// ── 7. Wait for the worker to consume the row → fakeSender records.
	eventually(t, 5*time.Second, func() bool { return fs.count() >= 1 })

	sent := fs.snapshot()
	if sent[0].ChatID != "12345" {
		t.Fatalf("sent chat_id = %q", sent[0].ChatID)
	}
	if !strings.Contains(sent[0].Text, "Alert: CPU high on db01") {
		t.Fatalf("sent text = %q", sent[0].Text)
	}

	// ── 8. /api/v1/logs?channel_id=X should show a 'sent' row.
	eventually(t, 5*time.Second, func() bool {
		req, _ := http.NewRequest(http.MethodGet,
			fmt.Sprintf("%s/api/v1/logs?channel_id=%d", base, channelID), nil)
		resp, err := userC.Do(req)
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return false
		}
		var body map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			return false
		}
		items, _ := body["items"].([]any)
		for _, it := range items {
			m, _ := it.(map[string]any)
			if status, _ := m["status"].(string); status == "sent" {
				return true
			}
		}
		return false
	})
}

// ── helpers ────────────────────────────────────────────────────────────

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	bs, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return bs
}

// postJSON POSTs body to url. If csrf is non-empty it is sent both as
// the X-CSRF-Token header (covering authed routes) — auth/register
// is exempted from CSRF inline, so csrf="" is fine for those.
func postJSON(t *testing.T, c *http.Client, url string, body []byte, csrf string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if csrf != "" {
		req.Header.Set("X-CSRF-Token", csrf)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	return resp
}

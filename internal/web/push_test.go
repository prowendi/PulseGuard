package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
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
		PushToken: pushToken, BotID: bot.ID,
		ChatID: "chat-123", RatePerMin: 60,
		DedupWindowS: dedupWindow, Enabled: enabled,
		Templates:    []*domain.ChannelTemplate{{TemplateID: tpl.ID, IsDefault: true}},
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

// TestWriteInternalDoesNotLeakErrorDetail is the regression guard for
// security-report S-H2: the client-facing body of a 500 must contain
// neither the wrapped sentinel sentinel ("schema_migrations failed: ..."
// style messages) nor any low-level identifier (path of the SQLite db,
// SQL fragment, internal Go file paths). The full error must reach the
// structured logger so on-call can still triage by request_id.
func TestWriteInternalDoesNotLeakErrorDetail(t *testing.T) {	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/push/some-token", nil)
	req.Header.Set("X-Request-Id", "rid-test-1234")

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	deps := Deps{Logger: logger}

	secret := "schema secret: SELECT password_hash FROM tenants /* sql leak */"
	writeInternal(rec, req, deps, "push: ingest", errors.New(secret))

	// Body must be opaque.
	body := rec.Body.String()
	if strings.Contains(body, secret) {
		t.Fatalf("response body leaks internal error: %s", body)
	}
	if !strings.Contains(body, "internal error") {
		t.Fatalf("response body missing generic message: %s", body)
	}
	// Status must be 500 with INTERNAL code.
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", rec.Code)
	}
	var env apiError
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if env.Error.Code != "INTERNAL" {
		t.Fatalf("code = %q", env.Error.Code)
	}
	// request_id must propagate so operator can correlate.
	if env.Error.RequestID == "" {
		t.Fatalf("missing request_id in envelope")
	}

	// Privileged log line must include the actual error.
	logs := logBuf.String()
	if !strings.Contains(logs, secret) {
		t.Fatalf("logger did not record full error; got: %s", logs)
	}
}

// TestPushBodyTooLargeReturns413 is the regression guard for
// security-report S-M4 / code-review-report C-I5. A 2 MiB JSON body
// must be rejected with 413 before any decoding allocates an unbounded
// map.
func TestPushBodyTooLargeReturns413(t *testing.T) {
	h := newTestHarness(t)
	seedTenantBotTemplateChannel(t, h, "tok-big", 0, true)

	// Build a 2 MiB body with a long string value so the JSON is valid
	// if it ever managed to reach the decoder — we want to prove the
	// guard rejects on size, not on parse error.
	const size = 2 << 20
	buf := bytes.NewBuffer(make([]byte, 0, size+64))
	buf.WriteString(`{"title":"`)
	for i := 0; i < size; i++ {
		buf.WriteByte('A')
	}
	buf.WriteString(`"}`)

	resp, err := http.Post(h.fullURL("/api/v1/push/tok-big"),
		"application/json", buf)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; body=%s", resp.StatusCode, drain(resp))
	}
	var env apiError
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if env.Error.Code != "VALIDATION" {
		t.Fatalf("code = %q, want VALIDATION", env.Error.Code)
	}
	if !strings.Contains(env.Error.Message, "1 MiB") {
		t.Fatalf("message %q does not mention size cap", env.Error.Message)
	}
}

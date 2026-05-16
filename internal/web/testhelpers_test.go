package web

import (
	"context"
	"database/sql"
	"encoding/base64"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/wendi/pulseguard/internal/auth"
	"github.com/wendi/pulseguard/internal/config"
	"github.com/wendi/pulseguard/internal/domain"
	"github.com/wendi/pulseguard/internal/pipeline"
	"github.com/wendi/pulseguard/internal/store"
)

// testHarness wires every repo/service against a temp SQLite DB so test
// cases can hit the real wire surface end to end. Each call returns an
// independent harness with its own clock so timestamp assertions are
// deterministic.
type testHarness struct {
	t       *testing.T
	db      *sql.DB
	deps    Deps
	clock   *domain.FakeClock
	server  *httptest.Server
	cleanup []func()
}

func newTestHarness(t *testing.T) *testHarness {
	t.Helper()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	db, err := store.Open(config.Database{
		Path:        dbPath,
		BusyTimeout: config.Duration(5 * time.Second),
	})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	clock := &domain.FakeClock{T: now}
	if err := store.Migrate(context.Background(), db, clock); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// 32-byte key (all zeros is fine for tests; only need GCM to function).
	key := make([]byte, 32)
	cipher, err := store.NewCipher(base64.StdEncoding.EncodeToString(key))
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}

	tenants := store.NewTenantRepo(db, clock)
	invites := store.NewInviteRepo(db, clock)
	sessions := store.NewSessionRepo(db, clock)
	bots := store.NewBotRepo(db, clock, cipher)
	templates := store.NewTemplateRepo(db, clock)
	channels := store.NewChannelRepo(db, clock)
	outbox := store.NewOutboxRepo(db, clock)
	logs := store.NewLogRepo(db, clock)
	dlq := store.NewDeadLetterRepo(db, clock)
	dedupRepo := store.NewDedupRepo(db)
	rl := store.NewRateLimitRepo(db, clock)
	commands := store.NewCommandRepo(db, clock)
	subscribers := store.NewSubscriberRepo(db, clock)

	cfg := &config.Config{}
	cfg.Security.SessionTTL = config.Duration(14 * 24 * time.Hour)
	cfg.Security.CookieSecure = false
	cfg.Security.BcryptCost = 4 // fast bcrypt for tests

	authSvc := auth.New(db, tenants, invites, sessions, cfg.Security, clock)
	dedup := pipeline.NewDedup(dedupRepo, clock)
	ingest := pipeline.NewIngestor(outbox, dedup, clock)

	logger := slog.New(slog.NewTextHandler(testLogWriter{t}, &slog.HandlerOptions{Level: slog.LevelWarn}))

	deps := Deps{
		Cfg:         cfg,
		Logger:      logger,
		Tenants:     tenants,
		Invites:     invites,
		Sessions:    sessions,
		Bots:        bots,
		Templates:   templates,
		Channels:    channels,
		Outbox:      outbox,
		Logs:        logs,
		DLQ:         dlq,
		RL:          rl,
		Commands:    commands,
		Subscribers: subscribers,
		Cipher:      cipher,
		Auth:        authSvc,
		Ingest:      ingest,
		Clock:       clock,
	}
	srv := httptest.NewServer(NewServer(deps))

	h := &testHarness{t: t, db: db, deps: deps, clock: clock, server: srv}
	h.cleanup = append(h.cleanup, func() { srv.Close() }, func() { _ = db.Close() })
	t.Cleanup(h.close)
	return h
}

func (h *testHarness) close() {
	for i := len(h.cleanup) - 1; i >= 0; i-- {
		h.cleanup[i]()
	}
}

// seedAdmin inserts a baseline admin tenant + an unused invite code so
// register/login tests can claim it. Returns (adminTenant, invite).
func (h *testHarness) seedAdmin(email, password, invite string) (*domain.Tenant, *domain.InviteCode) {
	h.t.Helper()
	hash, err := auth.HashPassword(password, 4)
	if err != nil {
		h.t.Fatalf("hash admin password: %v", err)
	}
	admin := &domain.Tenant{
		Email:        email,
		PasswordHash: string(hash),
		Role:         domain.RoleAdmin,
		Status:       domain.TenantActive,
	}
	if err := h.deps.Tenants.Insert(context.Background(), admin); err != nil {
		h.t.Fatalf("insert admin: %v", err)
	}
	inv := &domain.InviteCode{Code: invite, CreatedBy: admin.ID}
	if err := h.deps.Invites.Insert(context.Background(), inv); err != nil {
		h.t.Fatalf("insert invite: %v", err)
	}
	return admin, inv
}

// newJarClient returns a cookie-jar-backed *http.Client so multi-step
// flows reuse cookies. Redirect chain stops at the first 3xx so tests
// can assert on it.
func (h *testHarness) newJarClient() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{
		Jar: jar,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// fullURL prepends the live test server base URL to p.
func (h *testHarness) fullURL(p string) string {
	return h.server.URL + p
}

// jarValue extracts a cookie from a jar attached to baseURL.
func jarValue(t *testing.T, c *http.Client, base, name string) string {
	t.Helper()
	if c.Jar == nil {
		t.Fatalf("client has no cookie jar")
	}
	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	for _, ck := range c.Jar.Cookies(u) {
		if ck.Name == name {
			return ck.Value
		}
	}
	return ""
}

// testLogWriter funnels slog output into t.Log so test failures show
// every log line, without spamming stdout for passing tests.
type testLogWriter struct{ t *testing.T }

func (w testLogWriter) Write(p []byte) (int, error) {
	w.t.Log(string(p))
	return len(p), nil
}

package web

import (
	"log/slog"
	"os"
	"time"

	"github.com/wendi/pulseguard/internal/auth"
	"github.com/wendi/pulseguard/internal/config"
	"github.com/wendi/pulseguard/internal/domain"
	"github.com/wendi/pulseguard/internal/pipeline"
	"github.com/wendi/pulseguard/internal/platform"
	"github.com/wendi/pulseguard/internal/scripting"
	"github.com/wendi/pulseguard/internal/store"
)

// Deps is the dependency-injection bundle wired into NewServer. Every
// repo is an interface; tests can substitute fakes by populating the
// struct directly.
type Deps struct {
	Cfg         *config.Config
	Logger      *slog.Logger
	Tenants     domain.TenantRepo
	Invites     domain.InviteRepo
	Sessions    domain.SessionRepo
	Bots        domain.BotRepo
	Templates   domain.TemplateRepo
	Channels    domain.ChannelRepo
	Outbox      domain.OutboxRepo
	Logs        domain.LogRepo
	DLQ         domain.DeadLetterRepo
	RL          domain.RateLimiter
	Commands    domain.CommandRepo
	Subscribers domain.SubscriberRepo
	Cipher      *store.Cipher
	Auth        *auth.Service
	Ingest      *pipeline.Ingestor
	TG          domain.Sender
	Clock       domain.Clock

	// ScriptExec is the Starlark executor used by the commands "test
	// run" endpoint. nil-safe: handlers fall back to a stub when unset,
	// so httptest harnesses that do not exercise the endpoint can skip
	// constructing one.
	ScriptExec *scripting.Executor

	// BotListeners (optional) drives the per-bot long-poll loops the
	// /api/v1/bots CRUD layer needs to (re)start when a bot is created,
	// updated, or deleted. nil-safe: handlers no-op when unset so unit
	// tests that don't wire it in still work.
	BotListeners *platform.Manager

	// RateLimit is the per-IP request-per-second budget for /api/*.
	// Defaults to 100 when zero.
	RateLimit int
}

// normalize fills in safe defaults for optional Deps fields.
func (d *Deps) normalize() {
	if d.Logger == nil {
		d.Logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	}
	if d.Clock == nil {
		d.Clock = domain.RealClock()
	}
	if d.RateLimit <= 0 {
		d.RateLimit = 100
	}
}

// cookieSecure surfaces the configured cookie-secure flag (defaults to
// true when no Cfg is supplied; tests that need plain HTTP cookies must
// pass a Cfg with CookieSecure=false).
func (d *Deps) cookieSecure() bool {
	if d.Cfg == nil {
		return true
	}
	return d.Cfg.Security.CookieSecure
}

// sessionTTL returns the configured session lifetime, defaulting to 14d.
func (d *Deps) sessionTTL() time.Duration {
	if d.Cfg == nil || d.Cfg.Security.SessionTTL.Std() <= 0 {
		return 14 * 24 * time.Hour
	}
	return d.Cfg.Security.SessionTTL.Std()
}

// csrfSecret returns the bytes used to HMAC-bind CSRF tokens to session
// IDs. We reuse the master_key_b64 because it is already required to
// boot and rotating it implicitly invalidates every outstanding CSRF
// token (acceptable: the user just logs in again). When Cfg is nil
// (httptest minimal harnesses) the empty byte slice is returned and the
// HMAC degenerates to a constant — still constant-time compared, so
// anonymous flows that never set a session_id pass cleanly.
//
// Refs: security-report S-M2.
func (d *Deps) csrfSecret() []byte {
	if d.Cfg == nil || d.Cfg.Security.MasterKeyB64 == "" {
		return nil
	}
	return []byte(d.Cfg.Security.MasterKeyB64)
}

package web

import (
	"log/slog"
	"os"
	"time"

	"github.com/wendi/pulseguard/internal/auth"
	"github.com/wendi/pulseguard/internal/config"
	"github.com/wendi/pulseguard/internal/domain"
	"github.com/wendi/pulseguard/internal/pipeline"
	"github.com/wendi/pulseguard/internal/store"
)

// Deps is the dependency-injection bundle wired into NewServer. Every
// repo is an interface; tests can substitute fakes by populating the
// struct directly.
type Deps struct {
	Cfg       *config.Config
	Logger    *slog.Logger
	Tenants   domain.TenantRepo
	Invites   domain.InviteRepo
	Sessions  domain.SessionRepo
	Bots      domain.BotRepo
	Templates domain.TemplateRepo
	Channels  domain.ChannelRepo
	Outbox    domain.OutboxRepo
	Logs      domain.LogRepo
	DLQ       domain.DeadLetterRepo
	RL        domain.RateLimiter
	Cipher    *store.Cipher
	Auth      *auth.Service
	Ingest    *pipeline.Ingestor
	TG        domain.Sender
	Clock     domain.Clock

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

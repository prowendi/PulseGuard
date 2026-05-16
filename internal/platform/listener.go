// Package platform defines the lifecycle abstraction PulseGuard uses to
// run one event-loop per bot per chat platform. The Manager (this
// package's heart) owns one goroutine per active bot and dispatches
// platform-specific Factories to build the Listener that drains updates.
//
// The split makes future platforms (Discord, Slack, WeChat, ...) a
// plug-in story: register a new Factory at process start and any bot
// whose `platform` column matches the Factory's identifier will be
// driven by it. The domain layer is untouched.
package platform

import (
	"context"

	"github.com/wendi/pulseguard/internal/domain"
)

// Listener is a single bot's event loop. Implementations MUST block in
// Run until the supplied context is cancelled or a permanent error is
// encountered. Run returning nil signals "ctx cancelled, clean exit";
// any non-nil return aborts that bot's lifecycle until an operator
// re-enables it (or, in current MVP, the process restarts).
//
// Listener.Run is invoked exactly once per Manager.Start call. Run
// MUST be safe to interleave with arbitrary cancellation timing.
type Listener interface {
	// Run blocks until ctx is cancelled (return nil or ctx.Err()) or a
	// permanent error is encountered (return the error). The Manager
	// removes the bot from its active map either way.
	Run(ctx context.Context) error
}

// Factory turns a bot entity into a Listener. One Factory per platform
// identifier (e.g. "telegram"). The Manager looks up the Factory by
// Platform() when Start is called.
//
// Build MUST NOT spawn goroutines or hit the network — those happen
// inside Listener.Run after the Manager has taken its goroutine slot.
// Build is allowed to fail (e.g. malformed bot token shape); the
// Manager surfaces the error to the caller.
type Factory interface {
	// Platform identifies which domain.Bot.Platform value this Factory
	// is responsible for. Must be one of the identifiers exposed in
	// the domain package (e.g. domain.PlatformTelegram).
	Platform() string

	// Build constructs a Listener for the supplied bot. The bot pointer
	// is owned by the caller and MUST NOT be retained beyond Build —
	// listeners should snapshot the fields they need (botID, token,
	// tenantID, name) so subsequent mutations elsewhere do not race.
	Build(bot *domain.Bot) (Listener, error)
}

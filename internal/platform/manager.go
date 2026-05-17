package platform

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/prowendi/PulseGuard/internal/domain"
)

// ErrUnknownPlatform is returned by Start when no Factory matches the
// bot's Platform identifier.
var ErrUnknownPlatform = errors.New("platform: unknown bot platform")

// ErrManagerClosed is returned when Start is called after Shutdown.
var ErrManagerClosed = errors.New("platform: manager closed")

// ErrTokenInvalid is the platform-agnostic sentinel a Listener returns
// from Run when the upstream credentials are permanently invalid (e.g.
// Telegram 401 Unauthorized). The Manager observes this with
// errors.Is and routes it through OnTokenInvalid so the runtime can
// flip bots.enabled=false. Concrete adapters (e.g. telegram) alias
// their own package-level sentinel to this value so callers can keep
// using the adapter-specific name without coupling adapter packages
// to the Manager.
var ErrTokenInvalid = errors.New("platform: bot token invalid")

// TokenInvalidCallback is invoked when a listener exits with a
// platform-specific "token invalid" sentinel (currently
// telegram.ErrTokenInvalid). The runtime wires this to SetEnabled(false)
// so an upstream-revoked token automatically pauses the bot instead of
// dumping retries into the log.
//
// The callback runs synchronously inside the Manager's listener
// goroutine after the listener has already returned, so implementations
// must be cheap and non-blocking (do a single DB UPDATE + a log line).
// Errors from the callback are not surfaced — the bot is already off
// the rails; the callback's job is to record the new state and let
// the operator pick it up from the UI.
type TokenInvalidCallback func(ctx context.Context, bot *domain.Bot)

// Manager owns one goroutine per active bot. Start spawns a listener,
// Stop cancels it, Shutdown terminates every active listener. Start is
// idempotent on a per-bot basis: calling Start with the same botID stops
// any prior goroutine before spawning the new one, so token rotations
// and platform switches are safe.
//
// Manager is safe for concurrent use by multiple goroutines.
type Manager struct {
	logger          *slog.Logger
	factories       map[string]Factory
	onTokenInvalid  TokenInvalidCallback
	health          *healthState
	now             func() time.Time

	mu     sync.Mutex
	active map[int64]*entry
	closed bool
}

// entry tracks one running listener so Stop can cancel it and Wait until
// the goroutine actually exits before reporting "not running".
type entry struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// NewManager constructs a Manager. factories is an open set of platform
// adapters — pass one Factory per platform you intend to support. A nil
// logger is replaced with a discarding logger so call sites stay terse.
//
// Use SetTokenInvalidCallback to register the 401-auto-disable hook
// after construction (kept out of the variadic constructor signature so
// existing callers in tests do not have to change).
func NewManager(logger *slog.Logger, factories ...Factory) *Manager {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(discardWriter{}, nil))
	}
	fm := make(map[string]Factory, len(factories))
	for _, f := range factories {
		if f == nil {
			continue
		}
		fm[f.Platform()] = f
	}
	return &Manager{
		logger:    logger,
		factories: fm,
		active:    map[int64]*entry{},
		health:    newHealthState(),
		now:       time.Now,
	}
}

// Health returns a by-value snapshot of the BotHealth recorded for
// botID. Zero value is returned when no signal has been received yet
// (listener still warming up, or no traffic). Safe for concurrent use.
func (m *Manager) Health(botID int64) BotHealth {
	return m.health.get(botID)
}

// HealthSnapshot returns a copy of every recorded BotHealth keyed by
// botID. The web layer uses this to hydrate the bots list page in a
// single lock acquisition.
func (m *Manager) HealthSnapshot() map[int64]BotHealth {
	return m.health.snapshot()
}

// RecordUpdate is invoked by a listener after a successful
// non-empty getUpdates batch so the health panel can show "last seen"
// freshness. Safe to call from listener goroutines.
func (m *Manager) RecordUpdate(botID int64) {
	if botID == 0 {
		return
	}
	m.health.recordUpdate(botID, m.now())
}

// RecordDispatch is invoked by a listener after a successful custom-
// command dispatch (not built-ins). Bumps CommandsDispatched +
// LastSeenAt.
func (m *Manager) RecordDispatch(botID int64) {
	if botID == 0 {
		return
	}
	m.health.recordDispatch(botID, m.now())
}

// RecordError is invoked by a listener when an operation fails so the
// UI can surface the most recent error message. The message is
// truncated to ~200 chars at the store boundary; empty strings are
// ignored so callers can pass err.Error() without nil-checking.
func (m *Manager) RecordError(botID int64, msg string) {
	if botID == 0 {
		return
	}
	m.health.recordError(botID, msg, m.now())
}

// SetTokenInvalidCallback installs the callback invoked when a listener
// exits with a token-invalid sentinel (e.g. telegram.ErrTokenInvalid).
// nil clears the hook. Safe to call before any Start; not safe to
// race with concurrent listener termination — call it during wire-up.
func (m *Manager) SetTokenInvalidCallback(cb TokenInvalidCallback) {
	m.mu.Lock()
	m.onTokenInvalid = cb
	m.mu.Unlock()
}

// Start spawns (or restarts) the listener for the supplied bot. When a
// listener for the same botID already exists it is stopped (and waited
// on) before the new one starts so the active map only ever holds the
// freshest goroutine. ctx is the parent context — cancelling it tears
// down every listener the Manager has spawned via this Start call.
//
// When bot.Enabled is false Start refuses to spawn (or replace) the
// goroutine — it only logs at debug level and returns nil. This is the
// hot path the web /disable endpoint and the 401 auto-disable callback
// rely on: they SetEnabled(false) in the DB and then ALSO Stop() the
// running goroutine; a subsequent Start (e.g. a process restart that
// boots from ListAll) sees Enabled=false and stays out. nil is
// returned (not an error) because callers treat "bot intentionally
// paused" as a success path, not a failure to wire up.
func (m *Manager) Start(ctx context.Context, bot *domain.Bot) error {
	if bot == nil {
		return errors.New("platform: bot is nil")
	}
	if bot.ID == 0 {
		return errors.New("platform: bot id is zero")
	}
	if !bot.Enabled {
		// Make sure any stale goroutine for this bot is torn down — a
		// caller flipping Enabled in the DB and then immediately Start-ing
		// to "reload" the bot would otherwise leak the prior listener.
		m.Stop(bot.ID)
		m.logger.Info("platform: listener skipped (bot disabled)",
			"bot_id", bot.ID,
			"tenant_id", bot.TenantID,
			"platform", bot.Platform,
			"bot_name", bot.Name)
		return nil
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return ErrManagerClosed
	}
	factory, ok := m.factories[bot.Platform]
	if !ok {
		m.mu.Unlock()
		// Some platforms (currently Lark / 飞书 custom-bot webhooks)
		// are push-only and intentionally have no Factory registered.
		// Returning ErrUnknownPlatform here would surface as a startup
		// "listener start failed" warn for every Lark bot in the DB —
		// noisy and misleading because nothing is wrong. Silently skip
		// with an INFO log so operators still see the row but no error
		// bubbles up.
		if isPushOnlyPlatform(bot.Platform) {
			m.logger.Info("platform: bot has no listener support (push-only)",
				"bot_id", bot.ID,
				"tenant_id", bot.TenantID,
				"platform", bot.Platform,
				"bot_name", bot.Name)
			return nil
		}
		return fmt.Errorf("%w: %q", ErrUnknownPlatform, bot.Platform)
	}
	// If there's already an entry for this bot, swap it out: stop it,
	// wait until its goroutine returns, then spawn the new one. We must
	// drop the lock while waiting so the running goroutine's deferred
	// cleanup (which re-acquires the lock) cannot deadlock.
	if existing, ok := m.active[bot.ID]; ok {
		m.mu.Unlock()
		existing.cancel()
		<-existing.done
		m.mu.Lock()
		if m.closed {
			m.mu.Unlock()
			return ErrManagerClosed
		}
	}
	m.mu.Unlock()

	listener, err := factory.Build(bot)
	if err != nil {
		return fmt.Errorf("platform: build %s listener: %w", bot.Platform, err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	e := &entry{cancel: cancel, done: make(chan struct{})}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		cancel()
		close(e.done)
		return ErrManagerClosed
	}
	m.active[bot.ID] = e
	m.mu.Unlock()

	go m.run(runCtx, bot, listener, e)
	m.logger.Info("platform: listener started",
		"bot_id", bot.ID,
		"tenant_id", bot.TenantID,
		"platform", bot.Platform,
		"bot_name", bot.Name)
	return nil
}

// Stop cancels the listener for botID (if any) and blocks until its
// goroutine exits. Calling Stop on an unknown botID is a no-op.
func (m *Manager) Stop(botID int64) {
	m.mu.Lock()
	e, ok := m.active[botID]
	m.mu.Unlock()
	if !ok {
		return
	}
	e.cancel()
	<-e.done
}

// Shutdown cancels every active listener and blocks until they all exit.
// Subsequent calls to Start return ErrManagerClosed. Shutdown is
// idempotent.
func (m *Manager) Shutdown() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	entries := make([]*entry, 0, len(m.active))
	for _, e := range m.active {
		entries = append(entries, e)
	}
	m.mu.Unlock()

	for _, e := range entries {
		e.cancel()
	}
	for _, e := range entries {
		<-e.done
	}
}

// IsRunning reports whether a listener for botID is currently active.
// Intended for tests; production code should never branch on this.
func (m *Manager) IsRunning(botID int64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.active[botID]
	return ok
}

// run drives a single listener and guarantees the active map is cleaned
// up on exit, regardless of how the listener terminates. When the
// listener returns telegram.ErrTokenInvalid (or any other sentinel
// future platforms wire in via the same path), the OnTokenInvalid
// callback is invoked synchronously so the runtime can flip the DB
// row to disabled before the goroutine releases its slot in the
// active map.
func (m *Manager) run(ctx context.Context, bot *domain.Bot, listener Listener, e *entry) {
	defer close(e.done)
	defer func() {
		m.mu.Lock()
		// Only delete if the entry still matches us — Start may have
		// already swapped us out for a fresher goroutine.
		if cur, ok := m.active[bot.ID]; ok && cur == e {
			delete(m.active, bot.ID)
		}
		m.mu.Unlock()
	}()
	defer func() {
		if r := recover(); r != nil {
			m.logger.Error("platform: listener panic",
				"bot_id", bot.ID,
				"platform", bot.Platform,
				"panic", fmt.Sprintf("%v", r))
		}
	}()

	err := listener.Run(ctx)
	switch {
	case err == nil, errors.Is(err, context.Canceled):
		m.logger.Info("platform: listener exited",
			"bot_id", bot.ID,
			"platform", bot.Platform)
	case errors.Is(err, ErrTokenInvalid):
		m.logger.Warn("platform: listener exited with invalid token",
			"bot_id", bot.ID,
			"tenant_id", bot.TenantID,
			"platform", bot.Platform)
		m.mu.Lock()
		cb := m.onTokenInvalid
		m.mu.Unlock()
		if cb != nil {
			// Use a fresh context: the parent ctx may already be
			// cancelled (Shutdown path), but the auto-disable update
			// still needs to land. Operators tolerate a short blocking
			// DB write here — the callback contract is "do one UPDATE".
			cb(context.Background(), bot)
		}
	default:
		m.logger.Warn("platform: listener exited with error",
			"bot_id", bot.ID,
			"platform", bot.Platform,
			"err", err.Error())
	}
}

// discardWriter is the fallback io.Writer for a nil-logger fallback.
type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// isPushOnlyPlatform reports whether bot.Platform refers to a chat
// platform PulseGuard delivers messages to but does NOT (and cannot,
// in the case of Lark custom-bot webhooks) drain inbound updates
// from. The Manager uses this to distinguish "no Factory registered
// because the platform is intentionally one-way" from "no Factory
// registered because the platform is mis-configured".
//
// Currently:
//   - domain.PlatformLark — Lark / 飞书 custom-bot webhooks. The
//     remote side is incoming-only; there is no long-poll endpoint
//     and no callback dispatch, so listener boot is skipped silently.
//     (Lark application bots — bot_kind=app — are also incoming-only
//     here; their inbound events come through the
//     /api/v1/lark/events HTTP endpoint, not a listener goroutine.)
//   - domain.PlatformSMTP — outbound email relay. SMTP has no
//     "inbox" we poll, so the listener path is intentionally a no-op.
//
// New push-only platforms (Slack incoming-webhook, Discord webhook,
// etc.) should add their identifier here. Bidirectional platforms
// like Telegram MUST stay out so a missing Factory registration is
// surfaced as a loud ErrUnknownPlatform instead of swallowed.
func isPushOnlyPlatform(p string) bool {
	switch p {
	case domain.PlatformLark, domain.PlatformSMTP:
		return true
	default:
		return false
	}
}

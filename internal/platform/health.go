package platform

import (
	"sync"
	"time"
)

// BotHealth is the in-memory snapshot of a single bot listener's
// activity. The Manager exposes this via Health() so the web layer
// can render a "last seen" indicator on the bots page (V6-2).
//
// All timestamps are UTC. Counters are monotonically increasing across
// the process lifetime and reset on restart — this is a "did we hear
// anything recently?" panel, not a long-term metric store. Persistence
// would be premature; the UI rendering only needs "fresh enough?"
// classification (green/yellow/red).
type BotHealth struct {
	// LastSeenAt is the most recent timestamp at which the listener
	// either received a non-empty getUpdates batch OR dispatched a
	// command — i.e. proof the bot is alive and reachable. Zero means
	// no signal yet (listener still warming up, or no traffic).
	LastSeenAt time.Time

	// UpdatesReceived counts successful getUpdates rounds that
	// returned at least one update. Empty long-poll rounds are NOT
	// counted; they are normal idle behaviour.
	UpdatesReceived int64

	// CommandsDispatched counts successful custom-command dispatches.
	// /start, /chatid, /commands, /unsubscribe, /ack built-ins are
	// NOT counted — only the per-tenant Starlark commands the
	// dispatcher resolves through CommandResolver.
	CommandsDispatched int64

	// LastError carries the most recent error message the listener
	// surfaced. Empty when no error has occurred since process boot.
	// Trimmed to ~200 chars at the recorder boundary so the UI
	// tooltip stays scannable.
	LastError string

	// LastErrorAt is when LastError was captured. Zero when LastError
	// is empty.
	LastErrorAt time.Time
}

// healthState is the mutex-guarded backing store. Kept private so
// callers can only mutate through RecordUpdate/RecordDispatch/RecordError,
// which apply the per-bot lookup once.
type healthState struct {
	mu  sync.Mutex
	rec map[int64]*BotHealth
}

func newHealthState() *healthState {
	return &healthState{rec: map[int64]*BotHealth{}}
}

// get returns (a copy of) the BotHealth for botID, or the zero value
// when nothing has been recorded yet. Caller-safe by-value return so
// the consumer cannot mutate internal state through the snapshot.
func (h *healthState) get(botID int64) BotHealth {
	h.mu.Lock()
	defer h.mu.Unlock()
	r, ok := h.rec[botID]
	if !ok {
		return BotHealth{}
	}
	return *r
}

// snapshot returns a copy of every recorded BotHealth keyed by botID.
// Used by the web layer to hydrate the bots table in a single lock.
func (h *healthState) snapshot() map[int64]BotHealth {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make(map[int64]BotHealth, len(h.rec))
	for k, v := range h.rec {
		out[k] = *v
	}
	return out
}

// recordUpdate bumps UpdatesReceived and LastSeenAt for botID.
func (h *healthState) recordUpdate(botID int64, now time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	r := h.ensure(botID)
	r.UpdatesReceived++
	r.LastSeenAt = now
}

// recordDispatch bumps CommandsDispatched and LastSeenAt.
func (h *healthState) recordDispatch(botID int64, now time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	r := h.ensure(botID)
	r.CommandsDispatched++
	r.LastSeenAt = now
}

// recordError captures a (possibly truncated) error message + its
// timestamp. Does NOT bump LastSeenAt — an error is a sign the bot is
// in trouble, not proof of liveness. Empty errs are ignored.
func (h *healthState) recordError(botID int64, err string, now time.Time) {
	if err == "" {
		return
	}
	const maxLen = 200
	if len(err) > maxLen {
		err = err[:maxLen]
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	r := h.ensure(botID)
	r.LastError = err
	r.LastErrorAt = now
}

// ensure returns the BotHealth row for botID, creating it on first
// touch. Caller MUST already hold h.mu.
func (h *healthState) ensure(botID int64) *BotHealth {
	r, ok := h.rec[botID]
	if !ok {
		r = &BotHealth{}
		h.rec[botID] = r
	}
	return r
}

// clear removes the BotHealth row for botID — invoked when a bot is
// deleted so a future bot reusing the same id starts with a clean
// slate. Safe to call when no row exists.
func (h *healthState) clear(botID int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.rec, botID)
}

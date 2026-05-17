package web

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/wendi/pulseguard/internal/domain"
	"github.com/wendi/pulseguard/internal/platform"
	wmw "github.com/wendi/pulseguard/internal/web/middleware"

	"github.com/go-chi/chi/v5"
)

// botView is the safe-for-wire representation of a bot. The bot_token
// is NEVER echoed in full; only the last 4 characters are exposed via
// the masked field so the UI can hint at which key is configured.
// Enabled mirrors the Bot.Enabled domain flag so the UI can render a
// distinct "paused" state and the enable/disable buttons stay idempotent
// (the client checks before issuing the toggle).
//
// Health carries the V6-2 in-memory liveness panel: LastSeenAt /
// counters / LastError so the bots page can render a colour-coded
// indicator (green/yellow/red/gray) plus a tooltip without a second
// round-trip.
type botView struct {
	ID            int64         `json:"id"`
	Name          string        `json:"name"`
	Platform      string        `json:"platform"`
	Description   string        `json:"description,omitempty"`
	BotTokenLast4 string        `json:"bot_token_last4"`
	Enabled       bool          `json:"enabled"`
	CreatedAt     string        `json:"created_at"`
	UpdatedAt     string        `json:"updated_at"`
	Health        botHealthView `json:"health"`
}

// botHealthView is the JSON projection of platform.BotHealth. Times
// are formatted as UTC RFC-3339 so the wire format matches the rest
// of the API. Empty timestamps surface as the empty string so JS can
// do a simple truthiness check.
type botHealthView struct {
	Status             string `json:"status"` // green/yellow/red/gray
	LastSeenAt         string `json:"last_seen_at,omitempty"`
	LastSeenSecondsAgo int64  `json:"last_seen_seconds_ago,omitempty"`
	UpdatesReceived    int64  `json:"updates_received"`
	CommandsDispatched int64  `json:"commands_dispatched"`
	LastError          string `json:"last_error,omitempty"`
	LastErrorAt        string `json:"last_error_at,omitempty"`
}

// healthStatus classifies a BotHealth snapshot into a colour bucket
// the UI can render directly. Thresholds:
//
//   - disabled                -> "gray"
//   - LastSeenAt within 5min  -> "green"
//   - LastSeenAt within 30min -> "yellow"
//   - everything else         -> "red"
//
// An enabled bot without a single recorded signal classifies as
// "yellow" until the listener has had a chance to long-poll; that
// grace keeps a fresh deploy from immediately painting the table red.
func healthStatus(enabled bool, h platform.BotHealth, now time.Time) string {
	if !enabled {
		return "gray"
	}
	if h.LastSeenAt.IsZero() {
		return "yellow"
	}
	since := now.Sub(h.LastSeenAt)
	switch {
	case since <= 5*time.Minute:
		return "green"
	case since <= 30*time.Minute:
		return "yellow"
	default:
		return "red"
	}
}

func toBotView(b *domain.Bot) botView {
	return toBotViewWithHealth(b, platform.BotHealth{}, time.Now())
}

// toBotViewWithHealth is the workhorse constructor used when the
// caller has access to a Manager snapshot. The bare toBotView keeps
// the existing call sites working without forcing every handler to
// thread the Manager through.
func toBotViewWithHealth(b *domain.Bot, h platform.BotHealth, now time.Time) botView {
	last4 := ""
	if len(b.BotToken) >= 4 {
		last4 = b.BotToken[len(b.BotToken)-4:]
	}
	hv := botHealthView{
		Status:             healthStatus(b.Enabled, h, now),
		UpdatesReceived:    h.UpdatesReceived,
		CommandsDispatched: h.CommandsDispatched,
		LastError:          h.LastError,
	}
	if !h.LastSeenAt.IsZero() {
		hv.LastSeenAt = h.LastSeenAt.UTC().Format("2006-01-02T15:04:05Z")
		hv.LastSeenSecondsAgo = int64(now.Sub(h.LastSeenAt).Seconds())
	}
	if !h.LastErrorAt.IsZero() {
		hv.LastErrorAt = h.LastErrorAt.UTC().Format("2006-01-02T15:04:05Z")
	}
	return botView{
		ID:            b.ID,
		Name:          b.Name,
		Platform:      b.Platform,
		Description:   b.Description,
		BotTokenLast4: last4,
		Enabled:       b.Enabled,
		CreatedAt:     b.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		UpdatedAt:     b.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		Health:        hv,
	}
}

// healthFor returns the BotHealth for botID from the Manager (when
// wired) or the zero value otherwise. The zero value is safe to feed
// into toBotViewWithHealth — it classifies as "yellow" for an enabled
// bot and "gray" for a disabled one.
func healthFor(deps Deps, botID int64) platform.BotHealth {
	if deps.BotListeners == nil {
		return platform.BotHealth{}
	}
	return deps.BotListeners.Health(botID)
}

type botCreatePayload struct {
	Name        string `json:"name"`
	Platform    string `json:"platform"`
	BotToken    string `json:"bot_token"`
	Description string `json:"description"`
}

type botUpdatePayload struct {
	Name        *string `json:"name,omitempty"`
	Platform    *string `json:"platform,omitempty"`
	BotToken    *string `json:"bot_token,omitempty"`
	Description *string `json:"description,omitempty"`
}

func installBotsAPIRoutes(r chi.Router, deps Deps) {
	r.Get("/bots", apiBotList(deps))
	r.Post("/bots", apiBotCreate(deps))
	r.Get("/bots/{id}", apiBotGet(deps))
	r.Put("/bots/{id}", apiBotUpdate(deps))
	r.Delete("/bots/{id}", apiBotDelete(deps))
	// /enable and /disable are dedicated narrow endpoints so the UI
	// pause/resume button does not have to PUT the full bot payload
	// (which would force re-encrypting the token on every toggle).
	r.Post("/bots/{id}/enable", apiBotEnable(deps))
	r.Post("/bots/{id}/disable", apiBotDisable(deps))
}

func apiBotList(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tenant := wmw.Tenant(r.Context())
		items, err := deps.Bots.ListByTenant(r.Context(), tenant.ID)
		if err != nil {
			writeRepoError(w, r, deps, err)
			return
		}
		// Hydrate the health snapshot once so the per-bot map lookup
		// is O(1) inside the loop instead of taking the Manager mutex
		// per row.
		now := time.Now()
		var snap map[int64]platform.BotHealth
		if deps.BotListeners != nil {
			snap = deps.BotListeners.HealthSnapshot()
		}
		views := make([]botView, 0, len(items))
		for _, b := range items {
			views = append(views, toBotViewWithHealth(b, snap[b.ID], now))
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": views})
	}
}

func apiBotCreate(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var p botCreatePayload
		if !decodeJSON(w, r, &p) {
			return
		}
		p.Name = strings.TrimSpace(p.Name)
		p.BotToken = strings.TrimSpace(p.BotToken)
		p.Platform = strings.TrimSpace(p.Platform)
		if p.Platform == "" {
			p.Platform = domain.PlatformTelegram
		}
		if !domain.IsValidPlatform(p.Platform) {
			writeError(w, r, http.StatusBadRequest, "VALIDATION", "unknown platform")
			return
		}
		if !validateName(w, r, p.Name, 64) {
			return
		}
		if !botTokenLooksValid(p.Platform, p.BotToken) {
			writeError(w, r, http.StatusBadRequest, "VALIDATION", "bot_token format invalid for platform "+p.Platform)
			return
		}
		tenant := wmw.Tenant(r.Context())
		bot := &domain.Bot{
			TenantID:    tenant.ID,
			Name:        p.Name,
			Platform:    p.Platform,
			BotToken:    p.BotToken,
			Description: p.Description,
		}
		if err := deps.Bots.Insert(r.Context(), bot); err != nil {
			writeRepoError(w, r, deps, err)
			return
		}
		// Best-effort: spawn the listener for this fresh bot so the
		// user can immediately /start it from Telegram and learn the
		// chat_id. Manager.Start is idempotent — duplicate Starts
		// replace any prior goroutine.
		startBotListener(deps, bot)
		writeJSON(w, http.StatusCreated, toBotView(bot))
	}
}

func apiBotGet(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := parsePathID(w, r, "id")
		if !ok {
			return
		}
		tenant := wmw.Tenant(r.Context())
		bot, err := deps.Bots.GetByID(r.Context(), tenant.ID, id)
		if err != nil {
			writeRepoError(w, r, deps, err)
			return
		}
		writeJSON(w, http.StatusOK, toBotViewWithHealth(bot, healthFor(deps, bot.ID), time.Now()))
	}
}

func apiBotUpdate(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := parsePathID(w, r, "id")
		if !ok {
			return
		}
		var p botUpdatePayload
		if !decodeJSON(w, r, &p) {
			return
		}
		tenant := wmw.Tenant(r.Context())
		existing, err := deps.Bots.GetByID(r.Context(), tenant.ID, id)
		if err != nil {
			writeRepoError(w, r, deps, err)
			return
		}
		prevToken := existing.BotToken
		prevPlatform := existing.Platform
		if p.Name != nil {
			name := strings.TrimSpace(*p.Name)
			if !validateName(w, r, name, 64) {
				return
			}
			existing.Name = name
		}
		if p.Platform != nil {
			plat := strings.TrimSpace(*p.Platform)
			if plat == "" {
				plat = domain.PlatformTelegram
			}
			if !domain.IsValidPlatform(plat) {
				writeError(w, r, http.StatusBadRequest, "VALIDATION", "unknown platform")
				return
			}
			existing.Platform = plat
		}
		if p.BotToken != nil {
			tok := strings.TrimSpace(*p.BotToken)
			if !botTokenLooksValid(existing.Platform, tok) {
				writeError(w, r, http.StatusBadRequest, "VALIDATION", "bot_token format invalid for platform "+existing.Platform)
				return
			}
			existing.BotToken = tok
		}
		if p.Description != nil {
			existing.Description = *p.Description
		}
		if err := deps.Bots.Update(r.Context(), existing); err != nil {
			writeRepoError(w, r, deps, err)
			return
		}
		// Restart the listener when the credentials that drive it
		// changed; otherwise leave it running.
		if existing.BotToken != prevToken || existing.Platform != prevPlatform {
			restartBotListener(deps, existing)
		}
		writeJSON(w, http.StatusOK, toBotView(existing))
	}
}

func apiBotDelete(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := parsePathID(w, r, "id")
		if !ok {
			return
		}
		tenant := wmw.Tenant(r.Context())
		if err := deps.Bots.Delete(r.Context(), tenant.ID, id); err != nil {
			writeRepoError(w, r, deps, err)
			return
		}
		stopBotListener(deps, id)
		w.WriteHeader(http.StatusNoContent)
	}
}

// apiBotEnable flips the bot's enabled flag to true AND restarts the
// listener so the long-poll loop comes back without the operator
// touching anything else. The bot is re-read so the listener Start sees
// the freshly-decrypted token in case it was rotated while paused.
func apiBotEnable(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := parsePathID(w, r, "id")
		if !ok {
			return
		}
		tenant := wmw.Tenant(r.Context())
		if err := deps.Bots.SetEnabled(r.Context(), tenant.ID, id, true); err != nil {
			writeRepoError(w, r, deps, err)
			return
		}
		bot, err := deps.Bots.GetByID(r.Context(), tenant.ID, id)
		if err != nil {
			writeRepoError(w, r, deps, err)
			return
		}
		// Force a fresh goroutine — the operator may have toggled the
		// flag while a stale "skipped" entry was hanging around.
		restartBotListener(deps, bot)
		writeJSON(w, http.StatusOK, toBotView(bot))
	}
}

// apiBotDisable flips the bot's enabled flag to false AND stops the
// listener so the long-poll loop drains immediately. The response
// echoes the freshly-disabled view so the UI can update without a
// follow-up GET.
func apiBotDisable(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := parsePathID(w, r, "id")
		if !ok {
			return
		}
		tenant := wmw.Tenant(r.Context())
		if err := deps.Bots.SetEnabled(r.Context(), tenant.ID, id, false); err != nil {
			writeRepoError(w, r, deps, err)
			return
		}
		stopBotListener(deps, id)
		bot, err := deps.Bots.GetByID(r.Context(), tenant.ID, id)
		if err != nil {
			writeRepoError(w, r, deps, err)
			return
		}
		writeJSON(w, http.StatusOK, toBotView(bot))
	}
}

// startBotListener spawns a per-bot listener goroutine via the Manager.
// Errors are logged, never fatal — a bot whose listener cannot start
// (e.g. unknown platform) is still usable for outbound pushes.
func startBotListener(deps Deps, bot *domain.Bot) {
	if deps.BotListeners == nil || bot == nil {
		return
	}
	// Use a background context so the listener survives this HTTP
	// request returning. The runtime's parent context (passed when
	// the Manager was constructed) governs lifecycle.
	if err := deps.BotListeners.Start(context.Background(), bot); err != nil {
		if deps.Logger != nil {
			deps.Logger.Warn("bot listener start failed",
				"bot_id", bot.ID,
				"tenant_id", bot.TenantID,
				"platform", bot.Platform,
				"err", err.Error())
		}
	}
}

func restartBotListener(deps Deps, bot *domain.Bot) {
	if deps.BotListeners == nil || bot == nil {
		return
	}
	deps.BotListeners.Stop(bot.ID)
	startBotListener(deps, bot)
}

func stopBotListener(deps Deps, botID int64) {
	if deps.BotListeners == nil {
		return
	}
	deps.BotListeners.Stop(botID)
}

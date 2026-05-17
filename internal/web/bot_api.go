package web

import (
	"context"
	"net/http"
	"strings"

	"github.com/wendi/pulseguard/internal/domain"
	wmw "github.com/wendi/pulseguard/internal/web/middleware"

	"github.com/go-chi/chi/v5"
)

// botView is the safe-for-wire representation of a bot. The bot_token
// is NEVER echoed in full; only the last 4 characters are exposed via
// the masked field so the UI can hint at which key is configured.
// Enabled mirrors the Bot.Enabled domain flag so the UI can render a
// distinct "paused" state and the enable/disable buttons stay idempotent
// (the client checks before issuing the toggle).
type botView struct {
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	Platform      string `json:"platform"`
	Description   string `json:"description,omitempty"`
	BotTokenLast4 string `json:"bot_token_last4"`
	Enabled       bool   `json:"enabled"`
	CreatedAt     string `json:"created_at"`
	UpdatedAt     string `json:"updated_at"`
}

func toBotView(b *domain.Bot) botView {
	last4 := ""
	if len(b.BotToken) >= 4 {
		last4 = b.BotToken[len(b.BotToken)-4:]
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
	}
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
		views := make([]botView, 0, len(items))
		for _, b := range items {
			views = append(views, toBotView(b))
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
		if !botTokenPattern.MatchString(p.BotToken) {
			writeError(w, r, http.StatusBadRequest, "VALIDATION", "bot_token format invalid")
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
		writeJSON(w, http.StatusOK, toBotView(bot))
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
			if !botTokenPattern.MatchString(tok) {
				writeError(w, r, http.StatusBadRequest, "VALIDATION", "bot_token format invalid")
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

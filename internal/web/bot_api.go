package web

import (
	"net/http"
	"strings"

	"github.com/wendi/pulseguard/internal/domain"
	wmw "github.com/wendi/pulseguard/internal/web/middleware"

	"github.com/go-chi/chi/v5"
)

// botView is the safe-for-wire representation of a bot. The bot_token
// is NEVER echoed in full; only the last 4 characters are exposed via
// the masked field so the UI can hint at which key is configured.
type botView struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	BotTokenLast4 string `json:"bot_token_last4"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
}

func toBotView(b *domain.Bot) botView {
	last4 := ""
	if len(b.BotToken) >= 4 {
		last4 = b.BotToken[len(b.BotToken)-4:]
	}
	return botView{
		ID:          b.ID,
		Name:        b.Name,
		Description: b.Description,
		BotTokenLast4: last4,
		CreatedAt:   b.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		UpdatedAt:   b.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
}

type botCreatePayload struct {
	Name        string `json:"name"`
	BotToken    string `json:"bot_token"`
	Description string `json:"description"`
}

type botUpdatePayload struct {
	Name        *string `json:"name,omitempty"`
	BotToken    *string `json:"bot_token,omitempty"`
	Description *string `json:"description,omitempty"`
}

func installBotsAPIRoutes(r chi.Router, deps Deps) {
	r.Get("/bots", apiBotList(deps))
	r.Post("/bots", apiBotCreate(deps))
	r.Get("/bots/{id}", apiBotGet(deps))
	r.Put("/bots/{id}", apiBotUpdate(deps))
	r.Delete("/bots/{id}", apiBotDelete(deps))
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
			BotToken:    p.BotToken,
			Description: p.Description,
		}
		if err := deps.Bots.Insert(r.Context(), bot); err != nil {
			writeRepoError(w, r, deps, err)
			return
		}
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
		if p.Name != nil {
			name := strings.TrimSpace(*p.Name)
			if !validateName(w, r, name, 64) {
				return
			}
			existing.Name = name
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
		w.WriteHeader(http.StatusNoContent)
	}
}

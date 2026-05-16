package web

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"

	"github.com/wendi/pulseguard/internal/domain"
	wmw "github.com/wendi/pulseguard/internal/web/middleware"

	"github.com/go-chi/chi/v5"
)

type channelView struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	PushToken    string `json:"push_token"`
	BotID        int64  `json:"bot_id"`
	TemplateID   int64  `json:"template_id"`
	ChatID       string `json:"chat_id"`
	RatePerMin   int    `json:"rate_per_min"`
	DedupWindowS int    `json:"dedup_window_s"`
	Enabled      bool   `json:"enabled"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
}

func toChannelView(c *domain.Channel) channelView {
	return channelView{
		ID:           c.ID,
		Name:         c.Name,
		PushToken:    c.PushToken,
		BotID:        c.BotID,
		TemplateID:   c.TemplateID,
		ChatID:       c.ChatID,
		RatePerMin:   c.RatePerMin,
		DedupWindowS: c.DedupWindowS,
		Enabled:      c.Enabled,
		CreatedAt:    c.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		UpdatedAt:    c.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
}

type channelCreatePayload struct {
	Name         string `json:"name"`
	BotID        int64  `json:"bot_id"`
	TemplateID   int64  `json:"template_id"`
	ChatID       string `json:"chat_id"`
	RatePerMin   int    `json:"rate_per_min"`
	DedupWindowS int    `json:"dedup_window_s"`
	Enabled      *bool  `json:"enabled,omitempty"`
}

type channelUpdatePayload struct {
	Name         *string `json:"name,omitempty"`
	BotID        *int64  `json:"bot_id,omitempty"`
	TemplateID   *int64  `json:"template_id,omitempty"`
	ChatID       *string `json:"chat_id,omitempty"`
	RatePerMin   *int    `json:"rate_per_min,omitempty"`
	DedupWindowS *int    `json:"dedup_window_s,omitempty"`
	Enabled      *bool   `json:"enabled,omitempty"`
}

func installChannelsAPIRoutes(r chi.Router, deps Deps) {
	r.Get("/channels", apiChannelList(deps))
	r.Post("/channels", apiChannelCreate(deps))
	r.Get("/channels/{id}", apiChannelGet(deps))
	r.Put("/channels/{id}", apiChannelUpdate(deps))
	r.Delete("/channels/{id}", apiChannelDelete(deps))
	r.Post("/channels/{id}/rotate-token", apiChannelRotate(deps))
}

func apiChannelList(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tenant := wmw.Tenant(r.Context())
		items, err := deps.Channels.ListByTenant(r.Context(), tenant.ID)
		if err != nil {
			writeRepoError(w, r, err)
			return
		}
		views := make([]channelView, 0, len(items))
		for _, c := range items {
			views = append(views, toChannelView(c))
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": views})
	}
}

func apiChannelCreate(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var p channelCreatePayload
		if !decodeJSON(w, r, &p) {
			return
		}
		p.Name = strings.TrimSpace(p.Name)
		p.ChatID = strings.TrimSpace(p.ChatID)
		if !validateName(w, r, p.Name, 64) {
			return
		}
		if p.BotID == 0 || p.TemplateID == 0 || p.ChatID == "" {
			writeError(w, r, http.StatusBadRequest, "VALIDATION", "bot_id, template_id, chat_id required")
			return
		}
		if p.RatePerMin < 0 || p.DedupWindowS < 0 {
			writeError(w, r, http.StatusBadRequest, "VALIDATION", "rate_per_min and dedup_window_s must be >= 0")
			return
		}
		tenant := wmw.Tenant(r.Context())
		if !checkFKOwnership(w, r, deps, tenant.ID, p.BotID, p.TemplateID) {
			return
		}
		enabled := true
		if p.Enabled != nil {
			enabled = *p.Enabled
		}
		ch := &domain.Channel{
			TenantID: tenant.ID, Name: p.Name,
			PushToken: newPushToken(),
			BotID:     p.BotID, TemplateID: p.TemplateID,
			ChatID:       p.ChatID,
			RatePerMin:   p.RatePerMin,
			DedupWindowS: p.DedupWindowS,
			Enabled:      enabled,
		}
		if err := deps.Channels.Insert(r.Context(), ch); err != nil {
			writeRepoError(w, r, err)
			return
		}
		writeJSON(w, http.StatusCreated, toChannelView(ch))
	}
}

func apiChannelGet(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := parsePathID(w, r, "id")
		if !ok {
			return
		}
		tenant := wmw.Tenant(r.Context())
		c, err := deps.Channels.GetByID(r.Context(), tenant.ID, id)
		if err != nil {
			writeRepoError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, toChannelView(c))
	}
}

func apiChannelUpdate(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := parsePathID(w, r, "id")
		if !ok {
			return
		}
		var p channelUpdatePayload
		if !decodeJSON(w, r, &p) {
			return
		}
		tenant := wmw.Tenant(r.Context())
		ch, err := deps.Channels.GetByID(r.Context(), tenant.ID, id)
		if err != nil {
			writeRepoError(w, r, err)
			return
		}
		if p.Name != nil {
			n := strings.TrimSpace(*p.Name)
			if !validateName(w, r, n, 64) {
				return
			}
			ch.Name = n
		}
		if p.BotID != nil {
			if *p.BotID == 0 {
				writeError(w, r, http.StatusBadRequest, "VALIDATION", "bot_id must be > 0")
				return
			}
			if !checkFKOwnership(w, r, deps, tenant.ID, *p.BotID, ch.TemplateID) {
				return
			}
			ch.BotID = *p.BotID
		}
		if p.TemplateID != nil {
			if *p.TemplateID == 0 {
				writeError(w, r, http.StatusBadRequest, "VALIDATION", "template_id must be > 0")
				return
			}
			if !checkFKOwnership(w, r, deps, tenant.ID, ch.BotID, *p.TemplateID) {
				return
			}
			ch.TemplateID = *p.TemplateID
		}
		if p.ChatID != nil {
			s := strings.TrimSpace(*p.ChatID)
			if s == "" {
				writeError(w, r, http.StatusBadRequest, "VALIDATION", "chat_id cannot be empty")
				return
			}
			ch.ChatID = s
		}
		if p.RatePerMin != nil {
			if *p.RatePerMin < 0 {
				writeError(w, r, http.StatusBadRequest, "VALIDATION", "rate_per_min must be >= 0")
				return
			}
			ch.RatePerMin = *p.RatePerMin
		}
		if p.DedupWindowS != nil {
			if *p.DedupWindowS < 0 {
				writeError(w, r, http.StatusBadRequest, "VALIDATION", "dedup_window_s must be >= 0")
				return
			}
			ch.DedupWindowS = *p.DedupWindowS
		}
		if p.Enabled != nil {
			ch.Enabled = *p.Enabled
		}
		if err := deps.Channels.Update(r.Context(), ch); err != nil {
			writeRepoError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, toChannelView(ch))
	}
}

func apiChannelDelete(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := parsePathID(w, r, "id")
		if !ok {
			return
		}
		tenant := wmw.Tenant(r.Context())
		if err := deps.Channels.Delete(r.Context(), tenant.ID, id); err != nil {
			writeRepoError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func apiChannelRotate(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := parsePathID(w, r, "id")
		if !ok {
			return
		}
		tenant := wmw.Tenant(r.Context())
		ch, err := deps.Channels.GetByID(r.Context(), tenant.ID, id)
		if err != nil {
			writeRepoError(w, r, err)
			return
		}
		ch.PushToken = newPushToken()
		if err := deps.Channels.Update(r.Context(), ch); err != nil {
			writeRepoError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, toChannelView(ch))
	}
}

// checkFKOwnership ensures the bot and template referenced by a channel
// belong to the same tenant. Both lookups go through the repo so cross-
// tenant ids surface as ErrNotFound.
func checkFKOwnership(w http.ResponseWriter, r *http.Request, deps Deps, tenantID, botID, templateID int64) bool {
	if _, err := deps.Bots.GetByID(r.Context(), tenantID, botID); err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			writeError(w, r, http.StatusBadRequest, "VALIDATION", "bot_id not found")
			return false
		}
		writeRepoError(w, r, err)
		return false
	}
	if _, err := deps.Templates.GetByID(r.Context(), tenantID, templateID); err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			writeError(w, r, http.StatusBadRequest, "VALIDATION", "template_id not found")
			return false
		}
		writeRepoError(w, r, err)
		return false
	}
	return true
}

// newPushToken returns a 32-character URL-safe base64 string from 24
// bytes of entropy (the same alphabet used for session ids).
func newPushToken() string {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}

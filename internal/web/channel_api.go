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

// channelTemplateBindingView is the JSON shape of a single channel ↔
// template binding exposed by the API. is_default flags which one is
// the implicit pick when push omits the ?template= query string.
type channelTemplateBindingView struct {
	TemplateID int64 `json:"template_id"`
	IsDefault  bool  `json:"is_default"`
	SortOrder  int   `json:"sort_order"`
}

type channelView struct {
	ID           int64                        `json:"id"`
	Name         string                       `json:"name"`
	PushToken    string                       `json:"push_token"`
	BotID        int64                        `json:"bot_id"`
	Templates    []channelTemplateBindingView `json:"templates"`
	ChatID       string                       `json:"chat_id"`
	RatePerMin   int                          `json:"rate_per_min"`
	DedupWindowS int                          `json:"dedup_window_s"`
	Enabled      bool                         `json:"enabled"`
	CreatedAt    string                       `json:"created_at"`
	UpdatedAt    string                       `json:"updated_at"`
}

func toChannelView(c *domain.Channel) channelView {
	bindings := make([]channelTemplateBindingView, 0, len(c.Templates))
	for _, ct := range c.Templates {
		bindings = append(bindings, channelTemplateBindingView{
			TemplateID: ct.TemplateID,
			IsDefault:  ct.IsDefault,
			SortOrder:  ct.SortOrder,
		})
	}
	return channelView{
		ID:           c.ID,
		Name:         c.Name,
		PushToken:    c.PushToken,
		BotID:        c.BotID,
		Templates:    bindings,
		ChatID:       c.ChatID,
		RatePerMin:   c.RatePerMin,
		DedupWindowS: c.DedupWindowS,
		Enabled:      c.Enabled,
		CreatedAt:    c.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		UpdatedAt:    c.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
}

// channelCreatePayload accepts the new multi-template binding shape.
// TemplateIDs is the full set to bind; DefaultTemplateID names which
// one is the default. When DefaultTemplateID is zero the first entry
// in TemplateIDs is auto-promoted.
type channelCreatePayload struct {
	Name              string  `json:"name"`
	BotID             int64   `json:"bot_id"`
	TemplateIDs       []int64 `json:"template_ids"`
	DefaultTemplateID int64   `json:"default_template_id"`
	ChatID            string  `json:"chat_id"`
	RatePerMin        int     `json:"rate_per_min"`
	DedupWindowS      int     `json:"dedup_window_s"`
	Enabled           *bool   `json:"enabled,omitempty"`
}

// channelUpdatePayload mirrors channelCreatePayload. Passing a nil
// TemplateIDs slice leaves bindings untouched; passing an empty slice
// (explicit JSON []) wipes them.
type channelUpdatePayload struct {
	Name              *string  `json:"name,omitempty"`
	BotID             *int64   `json:"bot_id,omitempty"`
	TemplateIDs       *[]int64 `json:"template_ids,omitempty"`
	DefaultTemplateID *int64   `json:"default_template_id,omitempty"`
	ChatID            *string  `json:"chat_id,omitempty"`
	RatePerMin        *int     `json:"rate_per_min,omitempty"`
	DedupWindowS      *int     `json:"dedup_window_s,omitempty"`
	Enabled           *bool    `json:"enabled,omitempty"`
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
			writeRepoError(w, r, deps, err)
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
		if p.BotID == 0 || p.ChatID == "" {
			writeError(w, r, http.StatusBadRequest, "VALIDATION", "bot_id, chat_id required")
			return
		}
		if len(p.TemplateIDs) == 0 {
			writeError(w, r, http.StatusBadRequest, "VALIDATION", "at least one template_id required")
			return
		}
		if p.RatePerMin < 0 || p.DedupWindowS < 0 {
			writeError(w, r, http.StatusBadRequest, "VALIDATION", "rate_per_min and dedup_window_s must be >= 0")
			return
		}
		tenant := wmw.Tenant(r.Context())
		if !checkBotOwnership(w, r, deps, tenant.ID, p.BotID) {
			return
		}
		bindings, ok := buildBindings(w, r, deps, tenant.ID, p.TemplateIDs, p.DefaultTemplateID)
		if !ok {
			return
		}
		enabled := true
		if p.Enabled != nil {
			enabled = *p.Enabled
		}
		ch := &domain.Channel{
			TenantID: tenant.ID, Name: p.Name,
			PushToken: newPushToken(),
			BotID:     p.BotID,
			ChatID:    p.ChatID, RatePerMin: p.RatePerMin,
			DedupWindowS: p.DedupWindowS,
			Enabled:      enabled,
			Templates:    bindings,
		}
		if err := deps.Channels.Insert(r.Context(), ch); err != nil {
			writeRepoError(w, r, deps, err)
			return
		}
		// Reload to capture default-flag normalisation.
		out, err := deps.Channels.GetByID(r.Context(), tenant.ID, ch.ID)
		if err != nil {
			writeRepoError(w, r, deps, err)
			return
		}
		writeJSON(w, http.StatusCreated, toChannelView(out))
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
			writeRepoError(w, r, deps, err)
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
			writeRepoError(w, r, deps, err)
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
			if !checkBotOwnership(w, r, deps, tenant.ID, *p.BotID) {
				return
			}
			ch.BotID = *p.BotID
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

		// Replace bindings if caller passed an explicit list. Empty
		// list explicitly clears bindings (used to wipe before re-adding
		// from a UI step that owns its own commit path).
		var newBindings []*domain.ChannelTemplate
		var replaceBindings bool
		if p.TemplateIDs != nil {
			replaceBindings = true
			if len(*p.TemplateIDs) > 0 {
				def := int64(0)
				if p.DefaultTemplateID != nil {
					def = *p.DefaultTemplateID
				}
				bindings, ok := buildBindings(w, r, deps, tenant.ID, *p.TemplateIDs, def)
				if !ok {
					return
				}
				newBindings = bindings
			}
		}

		// Leave Templates slice empty so Update does NOT replace
		// existing bindings unless we explicitly want to.
		ch.Templates = nil
		if err := deps.Channels.Update(r.Context(), ch); err != nil {
			writeRepoError(w, r, deps, err)
			return
		}
		if replaceBindings {
			if err := deps.Channels.ReplaceTemplates(r.Context(), tenant.ID, ch.ID, newBindings); err != nil {
				writeRepoError(w, r, deps, err)
				return
			}
		}
		out, err := deps.Channels.GetByID(r.Context(), tenant.ID, ch.ID)
		if err != nil {
			writeRepoError(w, r, deps, err)
			return
		}
		writeJSON(w, http.StatusOK, toChannelView(out))
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
			writeRepoError(w, r, deps, err)
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
			writeRepoError(w, r, deps, err)
			return
		}
		ch.PushToken = newPushToken()
		ch.Templates = nil // preserve existing bindings
		if err := deps.Channels.Update(r.Context(), ch); err != nil {
			writeRepoError(w, r, deps, err)
			return
		}
		out, err := deps.Channels.GetByID(r.Context(), tenant.ID, ch.ID)
		if err != nil {
			writeRepoError(w, r, deps, err)
			return
		}
		writeJSON(w, http.StatusOK, toChannelView(out))
	}
}

// checkBotOwnership ensures the bot belongs to tenantID. Template
// ownership is validated separately in buildBindings.
func checkBotOwnership(w http.ResponseWriter, r *http.Request, deps Deps, tenantID, botID int64) bool {
	if _, err := deps.Bots.GetByID(r.Context(), tenantID, botID); err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			writeError(w, r, http.StatusBadRequest, "VALIDATION", "bot_id not found")
			return false
		}
		writeRepoError(w, r, deps, err)
		return false
	}
	return true
}

// buildBindings validates every templateID belongs to tenantID and
// returns the ChannelTemplate slice ready for ReplaceTemplates/Insert.
// defaultID, if non-zero, must be among templateIDs.
func buildBindings(w http.ResponseWriter, r *http.Request, deps Deps, tenantID int64, templateIDs []int64, defaultID int64) ([]*domain.ChannelTemplate, bool) {
	// Dedup template_ids — duplicates trip the PK on channel_templates.
	seen := map[int64]bool{}
	clean := make([]int64, 0, len(templateIDs))
	for _, id := range templateIDs {
		if id == 0 {
			writeError(w, r, http.StatusBadRequest, "VALIDATION", "template_id 0 is invalid")
			return nil, false
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		clean = append(clean, id)
	}
	if defaultID != 0 && !seen[defaultID] {
		writeError(w, r, http.StatusBadRequest, "VALIDATION", "default_template_id must be in template_ids")
		return nil, false
	}
	// Validate ownership.
	for _, id := range clean {
		if _, err := deps.Templates.GetByID(r.Context(), tenantID, id); err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				writeError(w, r, http.StatusBadRequest, "VALIDATION", "template_id not found")
				return nil, false
			}
			writeRepoError(w, r, deps, err)
			return nil, false
		}
	}
	out := make([]*domain.ChannelTemplate, 0, len(clean))
	for i, id := range clean {
		isDefault := false
		if defaultID == 0 && i == 0 {
			isDefault = true
		}
		if defaultID != 0 && id == defaultID {
			isDefault = true
		}
		out = append(out, &domain.ChannelTemplate{
			TemplateID: id,
			IsDefault:  isDefault,
			SortOrder:  i,
		})
	}
	return out, true
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

package web

import (
	"net/http"

	"github.com/wendi/pulseguard/internal/domain"
	wmw "github.com/wendi/pulseguard/internal/web/middleware"

	"github.com/go-chi/chi/v5"
)

// subscriberView is the JSON projection of domain.Subscriber.
type subscriberView struct {
	ID         int64  `json:"id"`
	CommandID  int64  `json:"command_id"`
	BotID      int64  `json:"bot_id"`
	ChatID     string `json:"chat_id"`
	Platform   string `json:"platform"`
	CreatedAt  string `json:"created_at"`
	LastSeenAt string `json:"last_seen_at"`
}

func toSubscriberView(s *domain.Subscriber) subscriberView {
	return subscriberView{
		ID:         s.ID,
		CommandID:  s.CommandID,
		BotID:      s.BotID,
		ChatID:     s.ChatID,
		Platform:   s.Platform,
		CreatedAt:  s.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		LastSeenAt: s.LastSeenAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
}

func installSubscribersAPIRoutes(r chi.Router, deps Deps) {
	r.Get("/subscribers", apiSubscriberList(deps))
	r.Delete("/subscribers/{id}", apiSubscriberDelete(deps))
}

func apiSubscriberList(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tenant := wmw.Tenant(r.Context())
		items, err := deps.Subscribers.ListByTenant(r.Context(), tenant.ID)
		if err != nil {
			writeRepoError(w, r, deps, err)
			return
		}
		views := make([]subscriberView, 0, len(items))
		for _, s := range items {
			views = append(views, toSubscriberView(s))
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": views})
	}
}

func apiSubscriberDelete(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := parsePathID(w, r, "id")
		if !ok {
			return
		}
		tenant := wmw.Tenant(r.Context())
		if err := deps.Subscribers.Delete(r.Context(), tenant.ID, id); err != nil {
			writeRepoError(w, r, deps, err)
			return
		}
		// RFC 9110 §15.3.5: 204 No Content MUST NOT carry a body.
		// channel_api.go uses WriteHeader; mirror that here so the
		// API surface is consistent.
		w.WriteHeader(http.StatusNoContent)
	}
}

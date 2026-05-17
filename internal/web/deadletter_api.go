package web

import (
	"net/http"

	"github.com/prowendi/PulseGuard/internal/domain"
	wmw "github.com/prowendi/PulseGuard/internal/web/middleware"

	"github.com/go-chi/chi/v5"
)

type dlqView struct {
	ID           int64  `json:"id"`
	OutboxID     int64  `json:"outbox_id"`
	ChannelID    int64  `json:"channel_id"`
	PayloadJSON  string `json:"payload_json"`
	RenderedText string `json:"rendered_text,omitempty"`
	LastError    string `json:"last_error"`
	Attempts     int    `json:"attempts"`
	CreatedAt    string `json:"created_at"`
}

func toDLQView(dl *domain.DeadLetter) dlqView {
	v := dlqView{
		ID:          dl.ID,
		OutboxID:    dl.OutboxID,
		ChannelID:   dl.ChannelID,
		PayloadJSON: dl.PayloadJSON,
		LastError:   dl.LastError,
		Attempts:    dl.Attempts,
		CreatedAt:   dl.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
	if dl.RenderedText != nil {
		v.RenderedText = *dl.RenderedText
	}
	return v
}

func installDLQAPIRoutes(r chi.Router, deps Deps) {
	r.Get("/deadletters", apiDLQList(deps))
	r.Post("/deadletters/{id}/replay", apiDLQReplay(deps))
}

func apiDLQList(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tenant := wmw.Tenant(r.Context())
		page, perPage := parsePagination(r)
		rows, total, err := deps.DLQ.ListByTenant(r.Context(), tenant.ID, page, perPage)
		if err != nil {
			writeRepoError(w, r, deps, err)
			return
		}
		views := make([]dlqView, 0, len(rows))
		for _, dl := range rows {
			views = append(views, toDLQView(dl))
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"items":    views,
			"total":    total,
			"page":     page,
			"per_page": perPage,
		})
	}
}

func apiDLQReplay(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := parsePathID(w, r, "id")
		if !ok {
			return
		}
		tenant := wmw.Tenant(r.Context())
		newID, err := deps.DLQ.Replay(r.Context(), tenant.ID, id)
		if err != nil {
			writeRepoError(w, r, deps, err)
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]any{
			"new_outbox_id": newID,
			"status":        "queued",
		})
	}
}

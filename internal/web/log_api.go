package web

import (
	"net/http"
	"strconv"

	"github.com/wendi/pulseguard/internal/domain"
	wmw "github.com/wendi/pulseguard/internal/web/middleware"

	"github.com/go-chi/chi/v5"
)

type logView struct {
	ID           int64  `json:"id"`
	OutboxID     *int64 `json:"outbox_id,omitempty"`
	ChannelID    int64  `json:"channel_id"`
	PayloadJSON  string `json:"payload_json"`
	RenderedText string `json:"rendered_text"`
	TGMessageID  *int64 `json:"tg_message_id,omitempty"`
	Status       string `json:"status"`
	Error        string `json:"error,omitempty"`
	Attempts     int    `json:"attempts"`
	CreatedAt    string `json:"created_at"`
}

func toLogView(l *domain.PushLog) logView {
	v := logView{
		ID:           l.ID,
		OutboxID:     l.OutboxID,
		ChannelID:    l.ChannelID,
		PayloadJSON:  l.PayloadJSON,
		RenderedText: l.RenderedText,
		TGMessageID:  l.TGMessageID,
		Status:       string(l.Status),
		Attempts:     l.Attempts,
		CreatedAt:    l.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
	if l.Error != nil {
		v.Error = *l.Error
	}
	return v
}

func installLogsAPIRoutes(r chi.Router, deps Deps) {
	r.Get("/logs", apiLogList(deps))
}

func apiLogList(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tenant := wmw.Tenant(r.Context())
		page, perPage := parsePagination(r)
		channelID, _ := strconv.ParseInt(r.URL.Query().Get("channel_id"), 10, 64)
		var (
			rows  []*domain.PushLog
			total int
			err   error
		)
		if channelID > 0 {
			rows, total, err = deps.Logs.ListByChannel(r.Context(), tenant.ID, channelID, page, perPage)
		} else {
			rows, total, err = deps.Logs.ListByTenant(r.Context(), tenant.ID, page, perPage)
		}
		if err != nil {
			writeRepoError(w, r, deps, err)
			return
		}
		views := make([]logView, 0, len(rows))
		for _, l := range rows {
			views = append(views, toLogView(l))
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"items":    views,
			"total":    total,
			"page":     page,
			"per_page": perPage,
		})
	}
}

// parsePagination clamps page and per_page to safe defaults.
// Default per_page=50, hard max per_page=200 (spec §W7 acceptance).
func parsePagination(r *http.Request) (page, perPage int) {
	page, _ = strconv.Atoi(r.URL.Query().Get("page"))
	if page <= 0 {
		page = 1
	}
	perPage, _ = strconv.Atoi(r.URL.Query().Get("per_page"))
	if perPage <= 0 {
		perPage = 50
	}
	if perPage > 200 {
		perPage = 200
	}
	return page, perPage
}

package web

import (
	"net/http"
	"strconv"

	wmw "github.com/wendi/pulseguard/internal/web/middleware"

	"github.com/go-chi/chi/v5"
)

type logListPage struct {
	pageData
	Logs      []logView
	Total     int
	Page      int
	PerPage   int
	ChannelID int64
}

func installLogsUIRoutes(r chi.Router, deps Deps) {
	r.Get("/logs", uiLogList(deps))
}

func uiLogList(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tenant := wmw.Tenant(r.Context())
		page, perPage := parsePagination(r)
		channelID, _ := strconv.ParseInt(r.URL.Query().Get("channel_id"), 10, 64)
		var (
			rows  []logView
			total int
		)
		if channelID > 0 {
			items, t, err := deps.Logs.ListByChannel(r.Context(), tenant.ID, channelID, page, perPage)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			total = t
			for _, l := range items {
				rows = append(rows, toLogView(l))
			}
		} else {
			items, t, err := deps.Logs.ListByTenant(r.Context(), tenant.ID, page, perPage)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			total = t
			for _, l := range items {
				rows = append(rows, toLogView(l))
			}
		}
		_ = Render(w, http.StatusOK, "logs-page", logListPage{
			pageData: pageData{
				Title:  "推送日志",
				Tenant: tenant,
				Active: "logs",
				CSRF:   readCSRFCookie(r),
			},
			Logs:      rows,
			Total:     total,
			Page:      page,
			PerPage:   perPage,
			ChannelID: channelID,
		})
	}
}

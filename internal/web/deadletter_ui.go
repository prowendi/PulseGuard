package web

import (
	"net/http"

	wmw "github.com/wendi/pulseguard/internal/web/middleware"

	"github.com/go-chi/chi/v5"
)

type dlqListPage struct {
	pageData
	DLQ     []dlqView
	Total   int
	Page    int
	PerPage int
}

func installDLQUIRoutes(r chi.Router, deps Deps) {
	r.Get("/deadletters", uiDLQList(deps))
	r.Post("/deadletters/{id}/replay", uiDLQReplay(deps))
}

func uiDLQList(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tenant := wmw.Tenant(r.Context())
		page, perPage := parsePagination(r)
		rows, total, _ := deps.DLQ.ListByTenant(r.Context(), tenant.ID, page, perPage)
		views := make([]dlqView, 0, len(rows))
		for _, dl := range rows {
			views = append(views, toDLQView(dl))
		}
		_ = Render(w, http.StatusOK, "deadletters-page", dlqListPage{
			pageData: pageData{
				Title:  "死信",
				Tenant: tenant,
				Active: "deadletters",
				CSRF:   readCSRFCookie(r),
			},
			DLQ:     views,
			Total:   total,
			Page:    page,
			PerPage: perPage,
		})
	}
}

func uiDLQReplay(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !VerifyCSRF(r, deps.csrfSecret()) {
			http.Error(w, "csrf", http.StatusForbidden)
			return
		}
		id, ok := parsePathID(w, r, "id")
		if !ok {
			return
		}
		tenant := wmw.Tenant(r.Context())
		_, _ = deps.DLQ.Replay(r.Context(), tenant.ID, id)
		http.Redirect(w, r, "/ui/deadletters", http.StatusSeeOther)
	}
}

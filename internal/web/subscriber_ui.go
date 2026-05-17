package web

import (
	"net/http"

	"github.com/prowendi/PulseGuard/internal/domain"
	wmw "github.com/prowendi/PulseGuard/internal/web/middleware"

	"github.com/go-chi/chi/v5"
)

// subscribersPage data wrapper for /ui/subscribers.
type subscribersPage struct {
	pageData
	Subscribers []subscriberView
}

func installSubscribersUIRoutes(r chi.Router, deps Deps) {
	r.Get("/subscribers", uiSubscriberList(deps))
	r.Post("/subscribers/{id}/delete", uiSubscriberDelete(deps))
}

func uiSubscriberList(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tenant := wmw.Tenant(r.Context())
		renderSubscribersPage(w, r, deps, tenant, nil)
	}
}

func uiSubscriberDelete(deps Deps) http.HandlerFunc {
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
		_ = deps.Subscribers.Delete(r.Context(), tenant.ID, id)
		http.Redirect(w, r, "/ui/subscribers", http.StatusSeeOther)
	}
}

func renderSubscribersPage(w http.ResponseWriter, r *http.Request, deps Deps, tenant *domain.Tenant, fl *flash) {
	items, _ := deps.Subscribers.ListByTenant(r.Context(), tenant.ID)
	views := make([]subscriberView, 0, len(items))
	for _, s := range items {
		views = append(views, toSubscriberView(s))
	}
	_ = Render(w, http.StatusOK, "subscribers-page", subscribersPage{
		pageData: pageData{
			Title:  "订阅者",
			Tenant: tenant,
			Active: "subscribers",
			CSRF:   readCSRFCookie(r),
			Flash:  fl,
			Theme:  themeFromRequest(r),
		},
		Subscribers: views,
	})
}

package web

import (
	wmw "github.com/wendi/pulseguard/internal/web/middleware"

	"github.com/go-chi/chi/v5"
)

// mountAuthAPI installs the public auth endpoints (register/login).
func mountAuthAPI(r chi.Router, deps Deps) {
	installAuthAPIRoutes(r, deps)
}

// mountPushAPI installs the public push endpoint.
func mountPushAPI(r chi.Router, deps Deps) {
	installPushAPIRoutes(r, deps)
}

// mountAuthedAPI installs every session-gated /api/v1/* endpoint.
// CSRF + auth middleware applied here once for the whole subtree.
func mountAuthedAPI(r chi.Router, deps Deps) {
	r.Group(func(sec chi.Router) {
		sec.Use(wmw.RequireAuth(deps.Auth))
		sec.Use(wmw.CSRFCheck())
		sec.Get("/me", apiMe(deps))
		sec.Post("/auth/logout", apiLogout(deps))
		installBotsAPIRoutes(sec, deps)
		installTemplatesAPIRoutes(sec, deps)
		installChannelsAPIRoutes(sec, deps)
	})
}

// mountAuthUI installs the public login/register HTMX views.
func mountAuthUI(r chi.Router, deps Deps) {
	installAuthUIRoutes(r, deps)
}

// mountAuthedUI installs every session-gated /ui/* view.
func mountAuthedUI(r chi.Router, deps Deps) {
	r.Group(func(sec chi.Router) {
		sec.Use(wmw.RequireAuth(deps.Auth))
		sec.Get("/dashboard", uiDashboard(deps))
		sec.Post("/logout", uiLogoutPost(deps))
		installBotsUIRoutes(sec, deps)
		installTemplatesUIRoutes(sec, deps)
		installChannelsUIRoutes(sec, deps)
	})
}

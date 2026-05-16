package web

import (
	wmw "github.com/wendi/pulseguard/internal/web/middleware"

	"github.com/go-chi/chi/v5"
)

// mountAuthAPI installs the public auth endpoints (register/login).
func mountAuthAPI(r chi.Router, deps Deps) {
	installAuthAPIRoutes(r, deps)
}

// mountPushAPI installs the public push endpoint. Populated in W3.
func mountPushAPI(r chi.Router, deps Deps) {
	_ = deps
}

// mountAuthedAPI installs every session-gated /api/v1/* endpoint.
// CSRF + auth middleware applied here once for the whole subtree.
func mountAuthedAPI(r chi.Router, deps Deps) {
	r.Group(func(sec chi.Router) {
		sec.Use(wmw.RequireAuth(deps.Auth))
		sec.Use(wmw.CSRFCheck())
		sec.Get("/me", apiMe(deps))
		sec.Post("/auth/logout", apiLogout(deps))
		// W4..W9 will append CRUD routes here.
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
		// W4..W9 will append /ui/{bots,templates,channels,...} routes here.
	})
}

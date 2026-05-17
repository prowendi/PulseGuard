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

// mountLarkEventsAPI installs the public Lark events endpoint
// (POST /api/v1/lark/events). Public because the body signature
// (not a PulseGuard session) is the authentication mechanism;
// Lark's developer console would have no way to present our cookie.
// The route is mounted under /api/v1 so the global IP rate limiter
// still applies — a misbehaving publisher cannot starve the rest
// of the API.
func mountLarkEventsAPI(r chi.Router, deps Deps) {
	installLarkEventsRoutes(r, deps)
}

// mountAuthedAPI installs every session-gated /api/v1/* endpoint.
// CSRF + auth middleware applied here once for the whole subtree.
//
// Ordering invariant: RequireAuth MUST run before CSRFCheck so the
// session id is attached to ctx; CSRFCheck rebuilds the expected HMAC
// from that session id to reject cookie-injection attacks
// (round2-security-report S2-M3).
func mountAuthedAPI(r chi.Router, deps Deps) {
	r.Group(func(sec chi.Router) {
		sec.Use(wmw.RequireAuth(deps.Auth))
		sec.Use(EnsureCSRFCookie(deps))
		sec.Use(wmw.CSRFCheck(deps.csrfSecret()))
		sec.Get("/me", apiMe(deps))
		sec.Post("/auth/logout", apiLogout(deps))
		installBotsAPIRoutes(sec, deps)
		installTemplatesAPIRoutes(sec, deps)
		installChannelsAPIRoutes(sec, deps)
		installLogsAPIRoutes(sec, deps)
		installDLQAPIRoutes(sec, deps)
		installCommandsAPIRoutes(sec, deps)
		installSubscribersAPIRoutes(sec, deps)
		// Admin-only sub-group for invite codes.
		sec.Group(func(adm chi.Router) {
			adm.Use(wmw.RequireAdmin())
			installInvitesAPIRoutes(adm, deps)
		})
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
		sec.Use(EnsureCSRFCookie(deps))
		sec.Get("/dashboard", uiDashboard(deps))
		sec.Post("/logout", uiLogoutPost(deps))
		installBotsUIRoutes(sec, deps)
		installTemplatesUIRoutes(sec, deps)
		installChannelsUIRoutes(sec, deps)
		installLogsUIRoutes(sec, deps)
		installDLQUIRoutes(sec, deps)
		installCommandsUIRoutes(sec, deps)
		installSubscribersUIRoutes(sec, deps)
		installAPIDocsUIRoutes(sec, deps)
		sec.Group(func(adm chi.Router) {
			adm.Use(wmw.RequireAdmin())
			installInvitesUIRoutes(adm, deps)
		})
	})
}

// Package web wires the PulseGuard HTTP surface — JSON API + HTMX UI —
// onto a chi router. NewServer builds the http.Handler that main.go
// serves; tests construct it the same way against an httptest backend.
package web

import (
	"net/http"

	"github.com/wendi/pulseguard/internal/web/middleware"

	"github.com/go-chi/chi/v5"
	chimid "github.com/go-chi/chi/v5/middleware"
)

// NewServer assembles every route documented in spec §Appendix A and
// returns the composed http.Handler. The handler is safe for concurrent
// use; main.go places it behind net/http.Server.
func NewServer(deps Deps) http.Handler {
	deps.normalize()
	r := chi.NewRouter()

	// Built-in chi middleware: request id + sane default handlers. The
	// request id is surfaced to error responses via X-Request-Id.
	r.Use(chimid.RequestID)
	r.Use(middleware.Recover(deps.Logger))
	r.Use(middleware.Logger(deps.Logger))

	// Plain health check — kept outside every other middleware so probes
	// stay cheap even when middleware misbehaves.
	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	// Static assets (HTMX, CSS, JS) served straight off the embedded FS.
	r.Handle("/static/*", http.StripPrefix("/static/", http.FileServer(http.FS(staticFS()))))

	// JSON API surface. All /api/* paths share IP rate-limiting and CSRF
	// enforcement on state-mutating methods (login/register and the push
	// webhook are exempted inline below).
	r.Route("/api/v1", func(api chi.Router) {
		api.Use(middleware.RateLimit(deps.RateLimit))
		mountAuthAPI(api, deps)
		mountPushAPI(api, deps)
		mountAuthedAPI(api, deps)
	})

	// HTMX UI surface — separate router so partials and full pages can
	// share the same auth middleware.
	r.Route("/ui", func(ui chi.Router) {
		mountAuthUI(ui, deps)
		mountAuthedUI(ui, deps)
	})

	return r
}

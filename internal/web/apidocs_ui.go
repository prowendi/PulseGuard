package web

import (
	"net/http"

	wmw "github.com/wendi/pulseguard/internal/web/middleware"

	"github.com/go-chi/chi/v5"
)

// apidocsPage carries the rendered REST API reference. The content is
// entirely static (driven by the template) so the handler only needs
// to fill in the standard pageData scaffolding.
type apidocsPage struct {
	pageData
	BaseURL string
}

func installAPIDocsUIRoutes(r chi.Router, deps Deps) {
	r.Get("/apidocs", uiAPIDocs(deps))
}

func uiAPIDocs(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		t := wmw.Tenant(r.Context())
		base := ""
		if deps.Cfg != nil {
			base = deps.Cfg.Server.BaseURL
		}
		if base == "" {
			base = "https://your-pulseguard.example.com"
		}
		_ = Render(w, http.StatusOK, "apidocs-page", apidocsPage{
			pageData: pageData{
				Title:  "API 文档",
				Tenant: t,
				Active: "apidocs",
				CSRF:   readCSRFCookie(r),
				Theme:  themeFromRequest(r),
			},
			BaseURL: base,
		})
	}
}

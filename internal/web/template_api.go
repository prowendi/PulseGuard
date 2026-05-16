package web

import (
	"net/http"
	"strings"
	ttpl "text/template"

	"github.com/wendi/pulseguard/internal/domain"
	"github.com/wendi/pulseguard/internal/render"
	wmw "github.com/wendi/pulseguard/internal/web/middleware"

	"github.com/go-chi/chi/v5"
)

// templateView is the JSON projection of domain.Template.
type templateView struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	ParseMode string `json:"parse_mode"`
	Body      string `json:"body"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

func toTemplateView(t *domain.Template) templateView {
	return templateView{
		ID:        t.ID,
		Name:      t.Name,
		ParseMode: string(t.ParseMode),
		Body:      t.Body,
		CreatedAt: t.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		UpdatedAt: t.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
}

type templateCreatePayload struct {
	Name      string `json:"name"`
	ParseMode string `json:"parse_mode"`
	Body      string `json:"body"`
}

type templateUpdatePayload struct {
	Name      *string `json:"name,omitempty"`
	ParseMode *string `json:"parse_mode,omitempty"`
	Body      *string `json:"body,omitempty"`
}

type templatePreviewPayload struct {
	Body      string         `json:"body"`
	ParseMode string         `json:"parse_mode"`
	Sample    map[string]any `json:"sample"`
}

func installTemplatesAPIRoutes(r chi.Router, deps Deps) {
	r.Get("/templates", apiTemplateList(deps))
	r.Post("/templates", apiTemplateCreate(deps))
	r.Post("/templates/preview", apiTemplatePreview(deps))
	r.Get("/templates/{id}", apiTemplateGet(deps))
	r.Put("/templates/{id}", apiTemplateUpdate(deps))
	r.Delete("/templates/{id}", apiTemplateDelete(deps))
}

func apiTemplateList(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tenant := wmw.Tenant(r.Context())
		items, err := deps.Templates.ListByTenant(r.Context(), tenant.ID)
		if err != nil {
			writeRepoError(w, r, err)
			return
		}
		views := make([]templateView, 0, len(items))
		for _, x := range items {
			views = append(views, toTemplateView(x))
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": views})
	}
}

func apiTemplateCreate(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var p templateCreatePayload
		if !decodeJSON(w, r, &p) {
			return
		}
		p.Name = strings.TrimSpace(p.Name)
		if !validateName(w, r, p.Name, 64) {
			return
		}
		mode, ok := normalizeParseMode(w, r, p.ParseMode)
		if !ok {
			return
		}
		if p.Body == "" {
			writeError(w, r, http.StatusBadRequest, "VALIDATION", "body is required")
			return
		}
		if err := preparseTemplate(p.Body); err != nil {
			writeError(w, r, http.StatusBadRequest, "VALIDATION", "template parse: "+err.Error())
			return
		}
		tenant := wmw.Tenant(r.Context())
		tpl := &domain.Template{
			TenantID:  tenant.ID,
			Name:      p.Name,
			ParseMode: mode,
			Body:      p.Body,
		}
		if err := deps.Templates.Insert(r.Context(), tpl); err != nil {
			writeRepoError(w, r, err)
			return
		}
		writeJSON(w, http.StatusCreated, toTemplateView(tpl))
	}
}

func apiTemplateGet(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := parsePathID(w, r, "id")
		if !ok {
			return
		}
		tenant := wmw.Tenant(r.Context())
		t, err := deps.Templates.GetByID(r.Context(), tenant.ID, id)
		if err != nil {
			writeRepoError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, toTemplateView(t))
	}
}

func apiTemplateUpdate(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := parsePathID(w, r, "id")
		if !ok {
			return
		}
		var p templateUpdatePayload
		if !decodeJSON(w, r, &p) {
			return
		}
		tenant := wmw.Tenant(r.Context())
		t, err := deps.Templates.GetByID(r.Context(), tenant.ID, id)
		if err != nil {
			writeRepoError(w, r, err)
			return
		}
		if p.Name != nil {
			n := strings.TrimSpace(*p.Name)
			if !validateName(w, r, n, 64) {
				return
			}
			t.Name = n
		}
		if p.ParseMode != nil {
			mode, ok := normalizeParseMode(w, r, *p.ParseMode)
			if !ok {
				return
			}
			t.ParseMode = mode
		}
		if p.Body != nil {
			if *p.Body == "" {
				writeError(w, r, http.StatusBadRequest, "VALIDATION", "body cannot be empty")
				return
			}
			if err := preparseTemplate(*p.Body); err != nil {
				writeError(w, r, http.StatusBadRequest, "VALIDATION", "template parse: "+err.Error())
				return
			}
			t.Body = *p.Body
		}
		if err := deps.Templates.Update(r.Context(), t); err != nil {
			writeRepoError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, toTemplateView(t))
	}
}

func apiTemplateDelete(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := parsePathID(w, r, "id")
		if !ok {
			return
		}
		tenant := wmw.Tenant(r.Context())
		if err := deps.Templates.Delete(r.Context(), tenant.ID, id); err != nil {
			writeRepoError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// apiTemplatePreview renders the supplied body against the sample
// payload using the same render pipeline as the worker. The result is
// returned in the JSON envelope; nothing is persisted.
func apiTemplatePreview(deps Deps) http.HandlerFunc {
	_ = deps
	return func(w http.ResponseWriter, r *http.Request) {
		var p templatePreviewPayload
		if !decodeJSON(w, r, &p) {
			return
		}
		mode, ok := normalizeParseMode(w, r, p.ParseMode)
		if !ok {
			return
		}
		if p.Body == "" {
			writeError(w, r, http.StatusBadRequest, "VALIDATION", "body is required")
			return
		}
		out, err := render.Render(r.Context(), &domain.Template{
			Name: "preview", ParseMode: mode, Body: p.Body,
		}, p.Sample)
		if err != nil {
			writeError(w, r, http.StatusBadRequest, "VALIDATION", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"rendered":   out,
			"parse_mode": string(mode),
		})
	}
}

// normalizeParseMode accepts an inbound value and maps to a domain
// constant; on failure writes a 400 and returns ok=false.
func normalizeParseMode(w http.ResponseWriter, r *http.Request, raw string) (domain.ParseMode, bool) {
	switch strings.TrimSpace(raw) {
	case "", string(domain.ParseMarkdownV2):
		return domain.ParseMarkdownV2, true
	case string(domain.ParseHTML):
		return domain.ParseHTML, true
	case string(domain.ParseNone):
		return domain.ParseNone, true
	default:
		writeError(w, r, http.StatusBadRequest, "VALIDATION", "parse_mode must be MarkdownV2/HTML/None")
		return "", false
	}
}

// preparseTemplate validates that body is a syntactically-valid
// text/template using the same FuncMap as the worker. We do NOT execute
// the template here.
func preparseTemplate(body string) error {
	_, err := ttpl.New("validate").Funcs(render.FuncMap).Parse(body)
	return err
}

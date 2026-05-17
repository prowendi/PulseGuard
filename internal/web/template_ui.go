package web

import (
	"net/http"
	"strings"

	"github.com/prowendi/PulseGuard/internal/domain"
	wmw "github.com/prowendi/PulseGuard/internal/web/middleware"

	"github.com/go-chi/chi/v5"
)

type templateListPage struct {
	pageData
	Templates []templateView
	Demos     []templateDemo
}

func installTemplatesUIRoutes(r chi.Router, deps Deps) {
	r.Get("/templates", uiTemplateList(deps))
	r.Post("/templates", uiTemplateCreate(deps))
	r.Post("/templates/{id}/update", uiTemplateUpdate(deps))
	r.Post("/templates/{id}/delete", uiTemplateDelete(deps))
}

func uiTemplateList(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tenant := wmw.Tenant(r.Context())
		renderTemplatePage(w, r, deps, tenant, nil)
	}
}

func uiTemplateCreate(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !VerifyCSRF(r, deps.csrfSecret()) {
			http.Error(w, "csrf", http.StatusForbidden)
			return
		}
		_ = r.ParseForm()
		name := strings.TrimSpace(r.PostForm.Get("name"))
		body := r.PostForm.Get("body")
		mode := strings.TrimSpace(r.PostForm.Get("parse_mode"))
		if mode == "" {
			mode = string(domain.ParseMarkdownV2)
		}
		tenant := wmw.Tenant(r.Context())
		if name == "" || body == "" {
			renderTemplatePage(w, r, deps, tenant, &flash{Level: "error", Message: "name 与 body 不能为空"})
			return
		}
		if err := preparseTemplate(body); err != nil {
			renderTemplatePage(w, r, deps, tenant, &flash{Level: "error", Message: "模板语法错误: " + err.Error()})
			return
		}
		tpl := &domain.Template{
			TenantID: tenant.ID, Name: name,
			ParseMode: domain.ParseMode(mode), Body: body,
		}
		if err := deps.Templates.Insert(r.Context(), tpl); err != nil {
			renderTemplatePage(w, r, deps, tenant, &flash{Level: "error", Message: err.Error()})
			return
		}
		http.Redirect(w, r, "/ui/templates", http.StatusSeeOther)
	}
}

func uiTemplateDelete(deps Deps) http.HandlerFunc {
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
		_ = deps.Templates.Delete(r.Context(), tenant.ID, id)
		http.Redirect(w, r, "/ui/templates", http.StatusSeeOther)
	}
}

// uiTemplateUpdate handles in-place edits from the shared edit-drawer.
// The drawer carries hidden inputs for name / parse_mode / body the
// user can tweak; we run the same preparseTemplate guard as the create
// path so a broken template body never reaches the worker.
func uiTemplateUpdate(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !VerifyCSRF(r, deps.csrfSecret()) {
			http.Error(w, "csrf", http.StatusForbidden)
			return
		}
		id, ok := parsePathID(w, r, "id")
		if !ok {
			return
		}
		_ = r.ParseForm()
		name := strings.TrimSpace(r.PostForm.Get("name"))
		body := r.PostForm.Get("body")
		mode := strings.TrimSpace(r.PostForm.Get("parse_mode"))
		if mode == "" {
			mode = string(domain.ParseMarkdownV2)
		}
		tenant := wmw.Tenant(r.Context())
		if name == "" || body == "" {
			renderTemplatePage(w, r, deps, tenant, &flash{Level: "error", Message: "name 与 body 不能为空"})
			return
		}
		if err := preparseTemplate(body); err != nil {
			renderTemplatePage(w, r, deps, tenant, &flash{Level: "error", Message: "模板语法错误: " + err.Error()})
			return
		}
		tpl, err := deps.Templates.GetByID(r.Context(), tenant.ID, id)
		if err != nil {
			renderTemplatePage(w, r, deps, tenant, &flash{Level: "error", Message: "模板不存在或不属于当前租户"})
			return
		}
		tpl.Name = name
		tpl.ParseMode = domain.ParseMode(mode)
		tpl.Body = body
		if err := deps.Templates.Update(r.Context(), tpl); err != nil {
			renderTemplatePage(w, r, deps, tenant, &flash{Level: "error", Message: err.Error()})
			return
		}
		http.Redirect(w, r, "/ui/templates", http.StatusSeeOther)
	}
}

func renderTemplatePage(w http.ResponseWriter, r *http.Request, deps Deps, tenant *domain.Tenant, fl *flash) {
	items, _ := deps.Templates.ListByTenant(r.Context(), tenant.ID)
	views := make([]templateView, 0, len(items))
	for _, t := range items {
		views = append(views, toTemplateView(t))
	}
	_ = Render(w, http.StatusOK, "templates-page", templateListPage{
		pageData: pageData{
			Title:  "模板",
			Tenant: tenant,
			Active: "templates",
			CSRF:   readCSRFCookie(r),
			Flash:  fl,
			Theme:  themeFromRequest(r),
		},
		Templates: views,
		Demos:     templateDemos(),
	})
}

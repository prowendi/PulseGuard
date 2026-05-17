package web

import (
	"net/http"
	"strings"

	"github.com/wendi/pulseguard/internal/domain"
	wmw "github.com/wendi/pulseguard/internal/web/middleware"

	"github.com/go-chi/chi/v5"
)

// commandsPage data wrapper for /ui/commands.
type commandsPage struct {
	pageData
	Commands []commandView
	Demos    []commandDemo
}

// commandDemo is a small showcase the page renders so operators can
// copy-paste a working script. Three entries cover the three skill
// classes: echo (pure compute), time (helper module), http (network).
type commandDemo struct {
	Name        string
	Title       string
	Description string
	Code        string
}

func commandDemos() []commandDemo {
	return []commandDemo{
		{
			Name:        "echo",
			Title:       "/echo",
			Description: "把参数原样回显。",
			Code: `def handle(args):
    return "echo: " + " ".join(args)
`,
		},
		{
			Name:        "time",
			Title:       "/time",
			Description: "返回当前 UTC 时间 (RFC3339)。",
			Code: `def handle(args):
    return "现在: " + time.now()
`,
		},
		{
			Name:        "github-zen",
			Title:       "/zen",
			Description: "调用 GitHub Zen API，演示 HTTP 模块用法。",
			Code: `def handle(args):
    r = http.get("https://api.github.com/zen")
    return r["body"]
`,
		},
	}
}

func installCommandsUIRoutes(r chi.Router, deps Deps) {
	r.Get("/commands", uiCommandList(deps))
	r.Post("/commands", uiCommandCreate(deps))
	r.Post("/commands/{id}/update", uiCommandUpdate(deps))
	r.Post("/commands/{id}/delete", uiCommandDelete(deps))
	r.Post("/commands/{id}/toggle", uiCommandToggle(deps))
}

func uiCommandList(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tenant := wmw.Tenant(r.Context())
		renderCommandsPage(w, r, deps, tenant, nil)
	}
}

func uiCommandCreate(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !VerifyCSRF(r, deps.csrfSecret()) {
			http.Error(w, "csrf", http.StatusForbidden)
			return
		}
		_ = r.ParseForm()
		name := normalizeCommandName(r.PostForm.Get("name"))
		desc := r.PostForm.Get("description")
		code := r.PostForm.Get("code")
		enabled := r.PostForm.Get("enabled") != ""
		tenant := wmw.Tenant(r.Context())
		if name == "" || strings.TrimSpace(code) == "" {
			renderCommandsPage(w, r, deps, tenant, &flash{Level: "error", Message: "name 与 code 不能为空"})
			return
		}
		if len(code) > MaxCommandCodeBytes {
			renderCommandsPage(w, r, deps, tenant, &flash{
				Level:   "error",
				Message: "code 超出 64KB 上限",
			})
			return
		}
		c := &domain.Command{
			TenantID:    tenant.ID,
			Name:        name,
			Description: desc,
			Code:        code,
			Enabled:     enabled,
		}
		if err := deps.Commands.Insert(r.Context(), c); err != nil {
			renderCommandsPage(w, r, deps, tenant, &flash{Level: "error", Message: err.Error()})
			return
		}
		http.Redirect(w, r, "/ui/commands", http.StatusSeeOther)
	}
}

func uiCommandDelete(deps Deps) http.HandlerFunc {
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
		_ = deps.Commands.Delete(r.Context(), tenant.ID, id)
		http.Redirect(w, r, "/ui/commands", http.StatusSeeOther)
	}
}

// uiCommandUpdate handles inline edits from the shared edit-drawer.
// The handler revalidates name/code/enabled, then patches the
// command row. Listener subscribers are unaffected — they key on
// (bot_id, chat_id, command_id) which stays stable across renames.
func uiCommandUpdate(deps Deps) http.HandlerFunc {
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
		name := normalizeCommandName(r.PostForm.Get("name"))
		desc := r.PostForm.Get("description")
		code := r.PostForm.Get("code")
		enabled := r.PostForm.Get("enabled") != ""
		tenant := wmw.Tenant(r.Context())
		if name == "" || strings.TrimSpace(code) == "" {
			renderCommandsPage(w, r, deps, tenant, &flash{Level: "error", Message: "name 与 code 不能为空"})
			return
		}
		if len(code) > MaxCommandCodeBytes {
			renderCommandsPage(w, r, deps, tenant, &flash{Level: "error", Message: "code 超出 64 KiB 上限"})
			return
		}
		cmd, err := deps.Commands.GetByID(r.Context(), tenant.ID, id)
		if err != nil {
			renderCommandsPage(w, r, deps, tenant, &flash{Level: "error", Message: "命令不存在或不属于当前租户"})
			return
		}
		cmd.Name = name
		cmd.Description = desc
		cmd.Code = code
		cmd.Enabled = enabled
		if err := deps.Commands.Update(r.Context(), cmd); err != nil {
			renderCommandsPage(w, r, deps, tenant, &flash{Level: "error", Message: err.Error()})
			return
		}
		http.Redirect(w, r, "/ui/commands", http.StatusSeeOther)
	}
}

// uiCommandToggle flips Enabled and persists.
func uiCommandToggle(deps Deps) http.HandlerFunc {
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
		c, err := deps.Commands.GetByID(r.Context(), tenant.ID, id)
		if err != nil {
			http.Redirect(w, r, "/ui/commands", http.StatusSeeOther)
			return
		}
		c.Enabled = !c.Enabled
		_ = deps.Commands.Update(r.Context(), c)
		http.Redirect(w, r, "/ui/commands", http.StatusSeeOther)
	}
}

func renderCommandsPage(w http.ResponseWriter, r *http.Request, deps Deps, tenant *domain.Tenant, fl *flash) {
	items, _ := deps.Commands.ListByTenant(r.Context(), tenant.ID)
	views := make([]commandView, 0, len(items))
	for _, c := range items {
		views = append(views, toCommandView(c))
	}
	_ = Render(w, http.StatusOK, "commands-page", commandsPage{
		pageData: pageData{
			Title:  "命令",
			Tenant: tenant,
			Active: "commands",
			CSRF:   readCSRFCookie(r),
			Flash:  fl,
			Theme:  themeFromRequest(r),
		},
		Commands: views,
		Demos:    commandDemos(),
	})
}

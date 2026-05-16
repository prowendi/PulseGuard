package web

import (
	"errors"
	"net/http"
	"strings"

	"github.com/wendi/pulseguard/internal/domain"
	"github.com/wendi/pulseguard/internal/scripting"
	wmw "github.com/wendi/pulseguard/internal/web/middleware"

	"github.com/go-chi/chi/v5"
)

// commandView is the JSON projection of domain.Command. The Code field
// is echoed back so the operator can edit it; tenants only see their
// own rows by virtue of the repo's tenant scoping.
type commandView struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Code        string `json:"code"`
	Enabled     bool   `json:"enabled"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
}

func toCommandView(c *domain.Command) commandView {
	return commandView{
		ID:          c.ID,
		Name:        c.Name,
		Description: c.Description,
		Code:        c.Code,
		Enabled:     c.Enabled,
		CreatedAt:   c.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		UpdatedAt:   c.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
}

type commandCreatePayload struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Code        string `json:"code"`
	Enabled     *bool  `json:"enabled,omitempty"`
}

type commandUpdatePayload struct {
	Name        *string `json:"name,omitempty"`
	Description *string `json:"description,omitempty"`
	Code        *string `json:"code,omitempty"`
	Enabled     *bool   `json:"enabled,omitempty"`
}

type commandTestPayload struct {
	Args []string `json:"args"`
}

type commandTestResult struct {
	Output string `json:"output"`
	Return string `json:"return"`
	Error  string `json:"error,omitempty"`
}

func installCommandsAPIRoutes(r chi.Router, deps Deps) {
	r.Get("/commands", apiCommandList(deps))
	r.Post("/commands", apiCommandCreate(deps))
	r.Get("/commands/{id}", apiCommandGet(deps))
	r.Put("/commands/{id}", apiCommandUpdate(deps))
	r.Delete("/commands/{id}", apiCommandDelete(deps))
	r.Post("/commands/{id}/test", apiCommandTest(deps))
}

func apiCommandList(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tenant := wmw.Tenant(r.Context())
		items, err := deps.Commands.ListByTenant(r.Context(), tenant.ID)
		if err != nil {
			writeRepoError(w, r, deps, err)
			return
		}
		views := make([]commandView, 0, len(items))
		for _, c := range items {
			views = append(views, toCommandView(c))
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": views})
	}
}

func apiCommandCreate(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var p commandCreatePayload
		if !decodeJSON(w, r, &p) {
			return
		}
		p.Name = normalizeCommandName(p.Name)
		if !validateName(w, r, p.Name, 64) {
			return
		}
		if strings.TrimSpace(p.Code) == "" {
			writeError(w, r, http.StatusBadRequest, "VALIDATION", "code is required")
			return
		}
		enabled := true
		if p.Enabled != nil {
			enabled = *p.Enabled
		}
		tenant := wmw.Tenant(r.Context())
		c := &domain.Command{
			TenantID:    tenant.ID,
			Name:        p.Name,
			Description: p.Description,
			Code:        p.Code,
			Enabled:     enabled,
		}
		if err := deps.Commands.Insert(r.Context(), c); err != nil {
			writeRepoError(w, r, deps, err)
			return
		}
		writeJSON(w, http.StatusCreated, toCommandView(c))
	}
}

func apiCommandGet(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := parsePathID(w, r, "id")
		if !ok {
			return
		}
		tenant := wmw.Tenant(r.Context())
		c, err := deps.Commands.GetByID(r.Context(), tenant.ID, id)
		if err != nil {
			writeRepoError(w, r, deps, err)
			return
		}
		writeJSON(w, http.StatusOK, toCommandView(c))
	}
}

func apiCommandUpdate(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := parsePathID(w, r, "id")
		if !ok {
			return
		}
		var p commandUpdatePayload
		if !decodeJSON(w, r, &p) {
			return
		}
		tenant := wmw.Tenant(r.Context())
		c, err := deps.Commands.GetByID(r.Context(), tenant.ID, id)
		if err != nil {
			writeRepoError(w, r, deps, err)
			return
		}
		if p.Name != nil {
			n := normalizeCommandName(*p.Name)
			if !validateName(w, r, n, 64) {
				return
			}
			c.Name = n
		}
		if p.Description != nil {
			c.Description = *p.Description
		}
		if p.Code != nil {
			if strings.TrimSpace(*p.Code) == "" {
				writeError(w, r, http.StatusBadRequest, "VALIDATION", "code is required")
				return
			}
			c.Code = *p.Code
		}
		if p.Enabled != nil {
			c.Enabled = *p.Enabled
		}
		if err := deps.Commands.Update(r.Context(), c); err != nil {
			writeRepoError(w, r, deps, err)
			return
		}
		writeJSON(w, http.StatusOK, toCommandView(c))
	}
}

func apiCommandDelete(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := parsePathID(w, r, "id")
		if !ok {
			return
		}
		tenant := wmw.Tenant(r.Context())
		if err := deps.Commands.Delete(r.Context(), tenant.ID, id); err != nil {
			writeRepoError(w, r, deps, err)
			return
		}
		writeJSON(w, http.StatusNoContent, nil)
	}
}

// apiCommandTest runs the command's script with the supplied args
// inside the configured scripting.Executor and returns the captured
// output / return value (or a sanitised error string). The subscriber
// table is NOT written by this path — it's a developer affordance.
func apiCommandTest(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := parsePathID(w, r, "id")
		if !ok {
			return
		}
		var p commandTestPayload
		if !decodeJSON(w, r, &p) {
			return
		}
		if deps.ScriptExec == nil {
			writeError(w, r, http.StatusServiceUnavailable, "INTERNAL", "script executor not configured")
			return
		}
		tenant := wmw.Tenant(r.Context())
		c, err := deps.Commands.GetByID(r.Context(), tenant.ID, id)
		if err != nil {
			writeRepoError(w, r, deps, err)
			return
		}
		res, runErr := deps.ScriptExec.Execute(r.Context(), c.Code, p.Args)
		out := commandTestResult{}
		if res != nil {
			out.Output = res.Output
			out.Return = res.Return
		}
		if runErr != nil {
			switch {
			case errors.Is(runErr, scripting.ErrTimeout):
				out.Error = "命令执行超时"
			case errors.Is(runErr, scripting.ErrUnsafeHost):
				out.Error = "命令请求的地址不允许"
			case errors.Is(runErr, scripting.ErrUnsupportedScheme):
				out.Error = "命令请求的协议不允许"
			case errors.Is(runErr, scripting.ErrOutputTooLarge):
				out.Error = "输出超出限制"
			case errors.Is(runErr, scripting.ErrMissingHandle):
				out.Error = "脚本必须定义 handle(args) 函数"
			default:
				out.Error = runErr.Error()
			}
		}
		writeJSON(w, http.StatusOK, out)
	}
}

// normalizeCommandName trims whitespace; we deliberately keep both
// "/echo" and "echo" forms accepted (the listener tries both).
func normalizeCommandName(s string) string {
	return strings.TrimSpace(s)
}

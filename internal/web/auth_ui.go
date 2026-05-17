package web

import (
	"errors"
	"net/http"
	"strings"

	"github.com/prowendi/PulseGuard/internal/domain"
	wmw "github.com/prowendi/PulseGuard/internal/web/middleware"

	"github.com/go-chi/chi/v5"
)

// uiLoginGet renders the login page. A CSRF cookie is issued on every GET
// so that the subsequent POST can be verified.
func uiLoginGet(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if t := wmw.Tenant(r.Context()); t != nil {
			http.Redirect(w, r, "/ui/dashboard", http.StatusSeeOther)
			return
		}
		tok := IssueCSRF(w, sessionIDFromRequest(r), deps.csrfSecret(), deps.cookieSecure())
		_ = Render(w, http.StatusOK, "login-page", pageData{
			Title: "登录",
			CSRF:  tok,
			Theme: themeFromRequest(r),
		})
	}
}

// uiLoginPost handles HTML form login. On success it issues the session
// cookie and redirects to /ui/dashboard; on failure it re-renders the
// login form with an error flash.
func uiLoginPost(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !VerifyCSRF(r, deps.csrfSecret()) {
			renderAuthFlash(w, r, deps, "login-page", "登录", "CSRF token mismatch")
			return
		}
		_ = r.ParseForm()
		email := strings.TrimSpace(strings.ToLower(r.PostForm.Get("email")))
		password := r.PostForm.Get("password")
		sess, err := deps.Auth.Login(r.Context(), email, password)
		if err != nil {
			renderAuthFlash(w, r, deps, "login-page", "登录", authFlashMessage(err))
			return
		}
		setSessionCookie(w, sess, deps)
		// Bind CSRF token to the new session id directly (ctx still
		// holds the pre-login "" session id).
		IssueCSRF(w, sess.ID, deps.csrfSecret(), deps.cookieSecure())
		http.Redirect(w, r, "/ui/dashboard", http.StatusSeeOther)
	}
}

func uiRegisterGet(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if t := wmw.Tenant(r.Context()); t != nil {
			http.Redirect(w, r, "/ui/dashboard", http.StatusSeeOther)
			return
		}
		tok := IssueCSRF(w, sessionIDFromRequest(r), deps.csrfSecret(), deps.cookieSecure())
		_ = Render(w, http.StatusOK, "register-page", pageData{
			Title: "注册",
			CSRF:  tok,
			Theme: themeFromRequest(r),
		})
	}
}

func uiRegisterPost(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !VerifyCSRF(r, deps.csrfSecret()) {
			renderAuthFlash(w, r, deps, "register-page", "注册", "CSRF token mismatch")
			return
		}
		_ = r.ParseForm()
		email := strings.TrimSpace(strings.ToLower(r.PostForm.Get("email")))
		password := r.PostForm.Get("password")
		invite := strings.TrimSpace(r.PostForm.Get("invite_code"))
		_, sess, err := deps.Auth.Register(r.Context(), email, password, invite)
		if err != nil {
			renderAuthFlash(w, r, deps, "register-page", "注册", authFlashMessage(err))
			return
		}
		setSessionCookie(w, sess, deps)
		IssueCSRF(w, sess.ID, deps.csrfSecret(), deps.cookieSecure())
		http.Redirect(w, r, "/ui/dashboard", http.StatusSeeOther)
	}
}

func uiLogoutPost(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !VerifyCSRF(r, deps.csrfSecret()) {
			http.Redirect(w, r, "/ui/dashboard", http.StatusSeeOther)
			return
		}
		sess := wmw.Session(r.Context())
		if sess != nil {
			_ = deps.Auth.Logout(r.Context(), sess.ID)
		}
		clearSessionCookie(w, deps)
		ClearCSRF(w, deps.cookieSecure())
		http.Redirect(w, r, "/ui/login", http.StatusSeeOther)
	}
}

// dashboardPage feeds the dashboard the four stat cards + recent log
// rows the home view summarises. Counts are best-effort: failures here
// degrade to zero so the page always renders.
type dashboardPage struct {
	pageData
	BotCount      int
	ChannelCount  int
	TemplateCount int
	RecentPushes  int
	DLQCount      int
	RecentLogs    []logView
}

func uiDashboard(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		t := wmw.Tenant(r.Context())
		tok := readCSRFCookie(r)

		var page dashboardPage
		page.pageData = pageData{Title: "概览", Tenant: t, Active: "dashboard", CSRF: tok, Theme: themeFromRequest(r)}

		if bots, err := deps.Bots.ListByTenant(r.Context(), t.ID); err == nil {
			page.BotCount = len(bots)
		}
		if chs, err := deps.Channels.ListByTenant(r.Context(), t.ID); err == nil {
			page.ChannelCount = len(chs)
		}
		if tpls, err := deps.Templates.ListByTenant(r.Context(), t.ID); err == nil {
			page.TemplateCount = len(tpls)
		}
		if rows, total, err := deps.Logs.ListByTenant(r.Context(), t.ID, 1, 10); err == nil {
			page.RecentPushes = total
			views := make([]logView, 0, len(rows))
			for _, l := range rows {
				views = append(views, toLogView(l))
			}
			page.RecentLogs = views
		}
		if _, total, err := deps.DLQ.ListByTenant(r.Context(), t.ID, 1, 1); err == nil {
			page.DLQCount = total
		}

		_ = Render(w, http.StatusOK, "dashboard-page", page)
	}
}

// rootRedirect points the bare hostname at /ui/dashboard (or /ui/login
// when unauthenticated; the middleware decides).
func rootRedirect(_ Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/ui/dashboard", http.StatusSeeOther)
	}
}

// renderAuthFlash re-renders an unauthenticated form page with an error
// flash + a freshly issued CSRF token.
func renderAuthFlash(w http.ResponseWriter, r *http.Request, deps Deps, page, title, msg string) {
	tok := IssueCSRF(w, sessionIDFromRequest(r), deps.csrfSecret(), deps.cookieSecure())
	_ = Render(w, http.StatusUnauthorized, page, pageData{
		Title: title,
		CSRF:  tok,
		Flash: &flash{Level: "error", Message: msg},
		Theme: themeFromRequest(r),
	})
}

func authFlashMessage(err error) string {
	switch {
	case errors.Is(err, domain.ErrInviteInvalid):
		return "邀请码无效或已过期"
	case errors.Is(err, domain.ErrConflict):
		return "邮箱已被注册"
	case errors.Is(err, domain.ErrValidation):
		return err.Error()
	case errors.Is(err, domain.ErrUnauthorized):
		return "邮箱或密码错误"
	default:
		return "服务错误，请稍后重试"
	}
}

func readCSRFCookie(r *http.Request) string {
	v, _ := lookupCSRFCookie(r)
	return v
}

// installAuthUIRoutes wires the public auth UI endpoints onto the
// /ui sub-router.
func installAuthUIRoutes(r chi.Router, deps Deps) {
	r.Get("/login", uiLoginGet(deps))
	r.Post("/login", uiLoginPost(deps))
	r.Get("/register", uiRegisterGet(deps))
	r.Post("/register", uiRegisterPost(deps))
}

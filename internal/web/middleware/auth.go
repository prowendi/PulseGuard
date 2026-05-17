package middleware

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/wendi/pulseguard/internal/auth"
	"github.com/wendi/pulseguard/internal/domain"
)

// CookieSession is the legacy (non-secure) cookie name carrying the
// session id. When the server runs with cookie_secure=true the handler
// layer instead writes "__Host-psg_session"; readSessionCookie tries
// both so the middleware does not need to know the deployment posture.
const CookieSession = "psg_session"

// CookieSessionHost is the secure-mode counterpart with the
// browser-enforced __Host- prefix. Mirrors web.SessionCookieName(true);
// duplicated here so the middleware package stays import-cycle free.
const CookieSessionHost = "__Host-psg_session"

// RequireAuth resolves the session cookie via the supplied auth.Service.
// On success the active tenant and session are attached to ctx; on
// failure /api/* returns 401 JSON and /ui/* redirects to /ui/login.
//
// API requests are detected by Accept: application/json or path prefix
// /api/.
func RequireAuth(svc *auth.Service) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := readSessionCookie(r)
			if id == "" {
				rejectUnauth(w, r)
				return
			}
			tenant, sess, err := svc.SessionFromID(r.Context(), id)
			if err != nil {
				if errors.Is(err, domain.ErrUnauthorized) || errors.Is(err, domain.ErrNotFound) {
					rejectUnauth(w, r)
					return
				}
				http.Error(w, "internal auth error", http.StatusInternalServerError)
				return
			}
			ctx := WithTenant(r.Context(), tenant)
			ctx = WithSession(ctx, sess)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// RequireAdmin assumes RequireAuth ran first and rejects non-admin
// callers. UI requests are redirected to /ui/dashboard, API to 403 JSON.
func RequireAdmin() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t := Tenant(r.Context())
			if t == nil || t.Role != domain.RoleAdmin {
				if isAPIRequest(r) {
					writeJSONError(w, http.StatusForbidden, "FORBIDDEN", "admin only")
				} else {
					http.Redirect(w, r, "/ui/dashboard", http.StatusSeeOther)
				}
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// readSessionCookie returns the session id from whichever variant of
// the cookie the browser sent. We try the strict __Host- name first
// because in production both variants may briefly coexist after a
// CookieSecure flip; preferring the strict one ensures cookie-toss
// attempts on the legacy name cannot win.
func readSessionCookie(r *http.Request) string {
	if c, err := r.Cookie(CookieSessionHost); err == nil && c.Value != "" {
		return c.Value
	}
	if c, err := r.Cookie(CookieSession); err == nil && c.Value != "" {
		return c.Value
	}
	return ""
}

func rejectUnauth(w http.ResponseWriter, r *http.Request) {
	if isAPIRequest(r) {
		writeJSONError(w, http.StatusUnauthorized, "UNAUTHORIZED", "login required")
		return
	}
	http.Redirect(w, r, "/ui/login", http.StatusSeeOther)
}

func isAPIRequest(r *http.Request) bool {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		return true
	}
	if strings.Contains(r.Header.Get("Accept"), "application/json") {
		return true
	}
	return false
}

// writeJSONError is a local copy to avoid an import cycle with the
// outer web package — the middleware package must not depend on web.
func writeJSONError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"code": code, "message": msg},
	})
}

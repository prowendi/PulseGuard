package middleware

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/wendi/pulseguard/internal/auth"
	"github.com/wendi/pulseguard/internal/domain"
)

// CookieSession is the cookie name carrying the session id.
const CookieSession = "psg_session"

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

func readSessionCookie(r *http.Request) string {
	c, err := r.Cookie(CookieSession)
	if err != nil || c.Value == "" {
		return ""
	}
	return c.Value
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

package web

import (
	"net/http"

	wmw "github.com/wendi/pulseguard/internal/web/middleware"
)

// EnsureCSRFCookie auto-issues a session-bound CSRF token for any
// authenticated request that does not already carry the psg_csrf
// cookie. Closes the "user lands on /ui/dashboard with a fresh session
// then POSTs without a csrf cookie -> 403" gap described in
// code-review-report C-I4.
//
// Installed AFTER RequireAuth (so the session is available in ctx) and
// BEFORE the CSRFCheck middleware that enforces verification on
// mutating methods.
func EnsureCSRFCookie(deps Deps) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Only authenticated requests get an auto-issued token.
			if sess := wmw.Session(r.Context()); sess != nil {
				if _, ok := lookupCSRFCookie(r); !ok {
					IssueCSRF(w, sess.ID, deps.csrfSecret(), deps.cookieSecure())
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

package middleware

import (
	"crypto/subtle"
	"net/http"
)

// CSRFCheck rejects state-mutating requests whose X-CSRF-Token header
// (or csrf form field) does not match the psg_csrf cookie. It is meant
// to be installed on authenticated UI/API routes; public endpoints
// (login/register/push) skip it because they either issue the cookie
// themselves or use a different bearer (push_token).
//
// We duplicate the cookie+header check here (rather than reach into the
// outer web package) to keep this package import-cycle free.
const (
	cookieCSRF = "psg_csrf"
	headerCSRF = "X-CSRF-Token"
)

func CSRFCheck() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch:
			default:
				next.ServeHTTP(w, r)
				return
			}
			cookie, err := r.Cookie(cookieCSRF)
			if err != nil || cookie.Value == "" {
				writeJSONError(w, http.StatusForbidden, "FORBIDDEN", "missing csrf cookie")
				return
			}
			tok := r.Header.Get(headerCSRF)
			if tok == "" {
				if err := r.ParseForm(); err == nil {
					tok = r.PostForm.Get("csrf")
				}
			}
			if tok == "" {
				writeJSONError(w, http.StatusForbidden, "FORBIDDEN", "missing csrf token")
				return
			}
			if subtle.ConstantTimeCompare([]byte(tok), []byte(cookie.Value)) != 1 {
				writeJSONError(w, http.StatusForbidden, "FORBIDDEN", "csrf token mismatch")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

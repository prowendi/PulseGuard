package middleware

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"strings"
)

// CSRFCheck rejects state-mutating requests whose X-CSRF-Token header
// (or csrf form field) does not match the psg_csrf cookie AND whose
// embedded HMAC tag does not bind the token to the current session.
// It is meant to be installed on authenticated UI/API routes; public
// endpoints (login/register/push) skip it because they either issue
// the cookie themselves or use a different bearer (push_token).
//
// secret is typically the master-key bytes. CSRFCheck MUST be installed
// AFTER RequireAuth so the session id is available in ctx; anonymous
// flows fall back to an empty session id binding (still HMAC-validated,
// just with the empty string, which matches the behaviour of
// web.IssueCSRF for pre-login flows).
//
// The HMAC verification logic mirrors web.VerifyCSRFToken; we duplicate
// it here (rather than importing the outer web package) to keep this
// package import-cycle free. Refs: round2-security-report S2-M3.
const (
	// cookieCSRF is the legacy (non-secure) CSRF cookie name. In
	// secure-mode deployments the handler layer writes the
	// __Host- prefixed variant instead; readCSRFCookie tries both.
	cookieCSRF     = "psg_csrf"
	cookieCSRFHost = "__Host-psg_csrf"
	headerCSRF     = "X-CSRF-Token"
	csrfTokenSep   = "."
)

// readCSRFCookie returns the value of whichever variant of the CSRF
// cookie the browser sent. Prefers the strict __Host- name so a
// cookie-toss on the legacy name cannot win during a transition.
func readCSRFCookie(r *http.Request) (string, bool) {
	if c, err := r.Cookie(cookieCSRFHost); err == nil && c.Value != "" {
		return c.Value, true
	}
	if c, err := r.Cookie(cookieCSRF); err == nil && c.Value != "" {
		return c.Value, true
	}
	return "", false
}

func CSRFCheck(secret []byte) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch:
			default:
				next.ServeHTTP(w, r)
				return
			}
			cookieVal, ok := readCSRFCookie(r)
			if !ok {
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
			// 1. Header/cookie must match (constant time). Catches the
			//    "attacker has only one of them" case.
			if subtle.ConstantTimeCompare([]byte(tok), []byte(cookieVal)) != 1 {
				writeJSONError(w, http.StatusForbidden, "FORBIDDEN", "csrf token mismatch")
				return
			}
			// 2. The token must HMAC-validate against the current
			//    session id. Catches the cookie-injection / cookie
			//    tossing case where a sibling subdomain plants a
			//    valid-looking psg_csrf cookie: the attacker does not
			//    know the master key so the embedded MAC will not
			//    match the verifier's recomputed one.
			sessionID := ""
			if sess := Session(r.Context()); sess != nil {
				sessionID = sess.ID
			}
			if !verifyCSRFTokenHMAC(cookieVal, sessionID, secret) {
				writeJSONError(w, http.StatusForbidden, "FORBIDDEN", "csrf token invalid")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// verifyCSRFTokenHMAC re-derives the expected HMAC from (sessionID,
// secret) and constant-time compares it against the tag embedded in
// the token. The token format is `nonce + "." + base64url(MAC)`
// produced by web.mintCSRFToken; both sides must agree.
func verifyCSRFTokenHMAC(token, sessionID string, secret []byte) bool {
	parts := strings.SplitN(token, csrfTokenSep, 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return false
	}
	nonce := parts[0]
	got, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(sessionID + "|" + nonce))
	want := mac.Sum(nil)
	return subtle.ConstantTimeCompare(got, want) == 1
}

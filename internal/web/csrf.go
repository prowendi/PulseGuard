package web

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"strings"

	wmw "github.com/prowendi/PulseGuard/internal/web/middleware"
)

// CookieCSRF is the legacy (non-secure) CSRF cookie name. When the
// server runs with cookie_secure=true the handler writes the
// `__Host-psg_csrf` variant instead (see web.CSRFCookieName). Lookup
// helpers must try both during a transition.
const CookieCSRF = "psg_csrf"

// HeaderCSRF is the header name HTMX (and JSON clients) must echo.
const HeaderCSRF = "X-CSRF-Token"

// csrfTokenSep splits the nonce from the HMAC tag inside a CSRF token.
// Chosen so URL-safe base64 (alphabet [A-Za-z0-9_-]) cannot contain it.
const csrfTokenSep = "."

// sessionIDFromRequest extracts the session ID from the request
// context (set by middleware.RequireAuth) so handlers can bind a
// freshly-issued CSRF token to it. Returns "" for anonymous flows
// (pre-login UI render, etc.) — the empty string still produces a
// stable HMAC the verifier can match.
func sessionIDFromRequest(r *http.Request) string {
	if sess := wmw.Session(r.Context()); sess != nil {
		return sess.ID
	}
	return ""
}

// IssueCSRF mints a CSRF token bound to sessionID via HMAC-SHA256, writes
// it as a non-HttpOnly cookie (HTMX needs to read it from JS), and
// returns the value so handlers can embed it in HTML forms too. When
// sessionID is empty (anonymous flows like /ui/login before the
// session lands) the binding falls back to the empty string — still
// safer than the prior pure-random token because the HMAC tag prevents
// a cookie-injection attacker from forging a matching header.
//
// The cookie name carries the `__Host-` prefix when secure is true so
// browsers enforce Secure + Path=/ + no Domain (RFC 6265bis §4.1.3.2),
// closing the cookie-toss vector from sibling subdomains.
//
// Refs: security-report S-M2, round2-security-report S2-L2.
func IssueCSRF(w http.ResponseWriter, sessionID string, secret []byte, secure bool) string {
	tok := mintCSRFToken(sessionID, secret)
	http.SetCookie(w, &http.Cookie{
		Name:     CSRFCookieName(secure),
		Value:    tok,
		Path:     "/",
		HttpOnly: false,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
	return tok
}

// mintCSRFToken builds `nonce + "." + base64url(HMAC(secret, sessionID|nonce))`.
// secret is typically the master key bytes; sessionID may be empty.
func mintCSRFToken(sessionID string, secret []byte) string {
	nonce := randomURLToken(24)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(sessionID + "|" + nonce))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return nonce + csrfTokenSep + sig
}

// VerifyCSRFToken constant-time compares the supplied token against a
// freshly-computed expected token for (sessionID, secret). Returns true
// when the embedded HMAC matches; rejects malformed inputs.
func VerifyCSRFToken(token, sessionID string, secret []byte) bool {
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

// ClearCSRF removes the CSRF cookie (logout flow). MaxAge=-1 deletes
// whichever variant of the cookie name was previously installed for
// this deployment posture.
func ClearCSRF(w http.ResponseWriter, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     CSRFCookieName(secure),
		Value:    "",
		Path:     "/",
		HttpOnly: false,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// VerifyCSRF compares the X-CSRF-Token header (or csrf form field, as a
// fallback for vanilla form POSTs) against the cookie value via constant
// time compare AND validates the HMAC binding the token to the session.
// Returns false on any mismatch (a 403 should follow).
//
// Only state-mutating methods (POST/PUT/DELETE/PATCH) need verification.
// The cookie is looked up under both the strict `__Host-` name and the
// legacy plain name so the function works regardless of deployment
// posture.
func VerifyCSRF(r *http.Request, secret []byte) bool {
	switch r.Method {
	case http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch:
	default:
		return true
	}
	cookieVal, ok := lookupCSRFCookie(r)
	if !ok {
		return false
	}
	header := r.Header.Get(HeaderCSRF)
	if header == "" {
		// Form fallback (vanilla browser POST without HTMX).
		if err := r.ParseForm(); err == nil {
			header = r.PostForm.Get("csrf")
		}
	}
	if header == "" {
		return false
	}
	if subtle.ConstantTimeCompare([]byte(header), []byte(cookieVal)) != 1 {
		return false
	}
	// Bind to session: rebuild the expected HMAC from the session id
	// the auth middleware already attached to ctx (empty string when
	// anonymous — verification still passes for the anonymous flow as
	// long as both header and cookie embed the same HMAC over "").
	sessionID := ""
	if sess := wmw.Session(r.Context()); sess != nil {
		sessionID = sess.ID
	}
	return VerifyCSRFToken(cookieVal, sessionID, secret)
}

// lookupCSRFCookie returns the request's CSRF cookie value under
// whichever variant of the name (legacy or __Host-prefixed) is present.
// Prefer the strict variant so a cookie-toss on the legacy name during
// a transition cannot win.
func lookupCSRFCookie(r *http.Request) (string, bool) {
	if c, err := r.Cookie(hostPrefix + csrfCookieBase); err == nil && c.Value != "" {
		return c.Value, true
	}
	if c, err := r.Cookie(CookieCSRF); err == nil && c.Value != "" {
		return c.Value, true
	}
	return "", false
}

// randomURLToken returns base64url(rand(n)) without padding.
func randomURLToken(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		// rand.Read is documented to never return a non-nil error on
		// linux/darwin/windows; if it does we cannot recover.
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}

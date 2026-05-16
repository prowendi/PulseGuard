package web

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
)

// CookieCSRF is the cookie name carrying the CSRF token.
const CookieCSRF = "psg_csrf"

// HeaderCSRF is the header name HTMX (and JSON clients) must echo.
const HeaderCSRF = "X-CSRF-Token"

// IssueCSRF mints a fresh CSRF token, writes it as a non-HttpOnly cookie
// (HTMX needs to read it from JS), and returns the value so handlers can
// embed it in HTML forms too.
func IssueCSRF(w http.ResponseWriter, secure bool) string {
	tok := randomURLToken(24)
	http.SetCookie(w, &http.Cookie{
		Name:     CookieCSRF,
		Value:    tok,
		Path:     "/",
		HttpOnly: false,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
	return tok
}

// ClearCSRF removes the CSRF cookie (logout flow).
func ClearCSRF(w http.ResponseWriter, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieCSRF,
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
// time compare. Returns false and writes a 403 on mismatch.
//
// Only state-mutating methods (POST/PUT/DELETE/PATCH) need verification.
func VerifyCSRF(r *http.Request) bool {
	switch r.Method {
	case http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch:
	default:
		return true
	}
	cookie, err := r.Cookie(CookieCSRF)
	if err != nil || cookie.Value == "" {
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
	return subtle.ConstantTimeCompare([]byte(header), []byte(cookie.Value)) == 1
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

package middleware

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wendi/pulseguard/internal/domain"
)

// mintTestToken duplicates web.mintCSRFToken so middleware tests can
// produce valid tokens without importing the outer web package
// (avoiding the same import cycle the production code dodges).
func mintTestToken(sessionID string, secret []byte, nonce string) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(sessionID + "|" + nonce))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return nonce + csrfTokenSep + sig
}

// withSession returns r decorated with a Session in ctx so CSRFCheck
// can rebuild the HMAC for verification.
func withSession(r *http.Request, sid string) *http.Request {
	return r.WithContext(WithSession(r.Context(), &domain.Session{ID: sid}))
}

// runCSRFCheck is a small driver that wires CSRFCheck around a 200
// terminal handler and returns the recorded response.
func runCSRFCheck(t *testing.T, secret []byte, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	terminal := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	CSRFCheck(secret)(terminal).ServeHTTP(rec, req)
	return rec
}

// TestCSRFCheck_RejectsCookieHeaderMatchButBadHMAC is the headline
// regression for round2-security-report S2-M3. A cookie-injection
// attacker (e.g. sibling subdomain XSS) can plant a matching cookie
// and replay the same value in the X-CSRF-Token header — the old
// middleware passed because cookie == header. The new HMAC-bound
// check must reject because the attacker cannot forge a MAC over
// the victim's session id without the master key.
func TestCSRFCheck_RejectsCookieHeaderMatchButBadHMAC(t *testing.T) {
	secret := []byte("server-master-key-very-secret-32")

	// A "token" with the right cosmetic shape but signed with the
	// wrong key — exactly what an attacker would synthesise via
	// cookie tossing.
	fake := mintTestToken("victim-sess", []byte("attacker-controlled-key"), "noncexyz")

	r := withSession(httptest.NewRequest(http.MethodPost, "/api/v1/anything",
		strings.NewReader("")), "victim-sess")
	r.AddCookie(&http.Cookie{Name: cookieCSRF, Value: fake})
	r.Header.Set(headerCSRF, fake)

	rec := runCSRFCheck(t, secret, r)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (HMAC must reject forged token)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "invalid") {
		t.Fatalf("body should mention 'invalid', got %s", rec.Body.String())
	}
}

// TestCSRFCheck_AcceptsValidHMACToken is the happy-path test: a token
// minted for the active session via the real HMAC must pass through
// to the terminal handler with 200.
func TestCSRFCheck_AcceptsValidHMACToken(t *testing.T) {
	secret := []byte("server-master-key-very-secret-32")
	const sid = "alice-sess"
	tok := mintTestToken(sid, secret, "validnonce123")

	r := withSession(httptest.NewRequest(http.MethodPost, "/api/v1/anything",
		strings.NewReader("")), sid)
	r.AddCookie(&http.Cookie{Name: cookieCSRF, Value: tok})
	r.Header.Set(headerCSRF, tok)

	rec := runCSRFCheck(t, secret, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (valid HMAC must pass)", rec.Code)
	}
	if rec.Body.String() != "ok" {
		t.Fatalf("body = %q, want 'ok'", rec.Body.String())
	}
}

// TestCSRFCheck_RejectsCrossSessionToken proves the binding: a token
// minted for session "old-sess" must not authorise a request whose
// active session is "new-sess". This is the post-login rotation
// scenario — the attacker captured the pre-rotation cookie and
// replays it; the recomputed HMAC over the new session id mismatches.
func TestCSRFCheck_RejectsCrossSessionToken(t *testing.T) {
	secret := []byte("server-master-key-very-secret-32")
	oldTok := mintTestToken("old-sess", secret, "noncebeforerotate")

	r := withSession(httptest.NewRequest(http.MethodPost, "/api/v1/anything",
		strings.NewReader("")), "new-sess")
	r.AddCookie(&http.Cookie{Name: cookieCSRF, Value: oldTok})
	r.Header.Set(headerCSRF, oldTok)

	rec := runCSRFCheck(t, secret, r)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (token from prior session must not verify)", rec.Code)
	}
}

// TestCSRFCheck_RejectsMissingCookie covers the empty-jar case.
func TestCSRFCheck_RejectsMissingCookie(t *testing.T) {
	r := withSession(httptest.NewRequest(http.MethodPost, "/x", strings.NewReader("")), "s1")
	r.Header.Set(headerCSRF, "anything")
	rec := runCSRFCheck(t, []byte("k"), r)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "missing csrf cookie") {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

// TestCSRFCheck_RejectsMissingHeader covers cookie present, header/
// form absent — exactly the no-op CSRF case.
func TestCSRFCheck_RejectsMissingHeader(t *testing.T) {
	r := withSession(httptest.NewRequest(http.MethodPost, "/x", strings.NewReader("")), "s1")
	r.AddCookie(&http.Cookie{Name: cookieCSRF, Value: "irrelevant"})
	rec := runCSRFCheck(t, []byte("k"), r)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "missing csrf token") {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

// TestCSRFCheck_RejectsCookieHeaderMismatch keeps the original
// constant-time-compare property under coverage: even if both are
// HMAC-valid, the header must match the cookie value verbatim.
func TestCSRFCheck_RejectsCookieHeaderMismatch(t *testing.T) {
	secret := []byte("k")
	a := mintTestToken("s1", secret, "nonceA")
	b := mintTestToken("s1", secret, "nonceB")

	r := withSession(httptest.NewRequest(http.MethodPost, "/x", strings.NewReader("")), "s1")
	r.AddCookie(&http.Cookie{Name: cookieCSRF, Value: a})
	r.Header.Set(headerCSRF, b)

	rec := runCSRFCheck(t, secret, r)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "mismatch") {
		t.Fatalf("body = %s, want mismatch message", rec.Body.String())
	}
}

// TestCSRFCheck_PassesSafeMethods makes sure GET / HEAD / OPTIONS are
// never gated — they must not require a token.
func TestCSRFCheck_PassesSafeMethods(t *testing.T) {
	for _, m := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
		r := httptest.NewRequest(m, "/x", nil)
		rec := runCSRFCheck(t, []byte("k"), r)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d, want 200 (safe methods skip CSRF)", m, rec.Code)
		}
	}
}

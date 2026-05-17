package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prowendi/PulseGuard/internal/domain"
	wmw "github.com/prowendi/PulseGuard/internal/web/middleware"
)

func TestSessionCookieName_SecurePrefix(t *testing.T) {
	if got := SessionCookieName(true); got != "__Host-psg_session" {
		t.Fatalf("secure session name = %q; want __Host-psg_session", got)
	}
	if got := SessionCookieName(false); got != "psg_session" {
		t.Fatalf("plain session name = %q; want psg_session", got)
	}
}

func TestCSRFCookieName_SecurePrefix(t *testing.T) {
	if got := CSRFCookieName(true); got != "__Host-psg_csrf" {
		t.Fatalf("secure csrf name = %q; want __Host-psg_csrf", got)
	}
	if got := CSRFCookieName(false); got != "psg_csrf" {
		t.Fatalf("plain csrf name = %q; want psg_csrf", got)
	}
}

func TestCookieConstants_AgreeBetweenPackages(t *testing.T) {
	// Sanity: the middleware package mirrors web.SessionCookieName so
	// readers don't drift apart. Keep these in lockstep.
	if wmw.CookieSession != SessionCookieName(false) {
		t.Fatalf("middleware.CookieSession = %q != web.SessionCookieName(false) = %q",
			wmw.CookieSession, SessionCookieName(false))
	}
	if wmw.CookieSessionHost != SessionCookieName(true) {
		t.Fatalf("middleware.CookieSessionHost = %q != web.SessionCookieName(true) = %q",
			wmw.CookieSessionHost, SessionCookieName(true))
	}
}

// TestIssueCSRF_SecureUsesHostPrefix asserts the wire format of the
// Set-Cookie header when secure is on: name must be __Host-psg_csrf,
// Path must be "/", Domain must be absent, and Secure must be on.
func TestIssueCSRF_SecureUsesHostPrefix(t *testing.T) {
	w := httptest.NewRecorder()
	tok := IssueCSRF(w, "sid-x", []byte("k"), true)
	if tok == "" {
		t.Fatal("IssueCSRF returned empty token")
	}
	setCookie := w.Header().Get("Set-Cookie")
	if !strings.Contains(setCookie, "__Host-psg_csrf=") {
		t.Fatalf("Set-Cookie missing __Host- name: %q", setCookie)
	}
	if !strings.Contains(setCookie, "Path=/") {
		t.Fatalf("Set-Cookie missing Path=/: %q", setCookie)
	}
	if !strings.Contains(setCookie, "Secure") {
		t.Fatalf("Set-Cookie missing Secure: %q", setCookie)
	}
	if strings.Contains(setCookie, "Domain=") {
		t.Fatalf("Set-Cookie must not carry Domain: %q", setCookie)
	}
}

// TestIssueCSRF_PlainKeepsLegacyName asserts the legacy name is used
// when cookie_secure is off so dev/test over plain HTTP still works
// (browsers silently drop __Host- without Secure).
func TestIssueCSRF_PlainKeepsLegacyName(t *testing.T) {
	w := httptest.NewRecorder()
	IssueCSRF(w, "sid-y", []byte("k"), false)
	setCookie := w.Header().Get("Set-Cookie")
	if !strings.HasPrefix(setCookie, "psg_csrf=") {
		t.Fatalf("plain Set-Cookie should start with psg_csrf= got %q", setCookie)
	}
	if strings.Contains(setCookie, "__Host-") {
		t.Fatalf("plain Set-Cookie must not carry __Host-: %q", setCookie)
	}
}

// TestVerifyCSRF_AcceptsHostPrefixCookie ensures the verifier honours
// the __Host- variant (so a secure-mode deployment is not locked out).
func TestVerifyCSRF_AcceptsHostPrefixCookie(t *testing.T) {
	secret := []byte("test-secret")
	tok := mintCSRFToken("sid-z", secret)

	req := httptest.NewRequest(http.MethodPost, "/x", nil)
	req.AddCookie(&http.Cookie{Name: "__Host-psg_csrf", Value: tok})
	req.Header.Set("X-CSRF-Token", tok)

	// Attach the session id to ctx so HMAC binding lines up.
	req = req.WithContext(wmw.WithSession(req.Context(), &domain.Session{ID: "sid-z"}))

	if !VerifyCSRF(req, secret) {
		t.Fatal("VerifyCSRF should accept __Host-psg_csrf")
	}
}

// TestVerifyCSRF_AcceptsLegacyCookie keeps the legacy non-prefixed
// behaviour intact for HTTP dev deployments.
func TestVerifyCSRF_AcceptsLegacyCookie(t *testing.T) {
	secret := []byte("test-secret")
	tok := mintCSRFToken("sid-q", secret)

	req := httptest.NewRequest(http.MethodPost, "/x", nil)
	req.AddCookie(&http.Cookie{Name: "psg_csrf", Value: tok})
	req.Header.Set("X-CSRF-Token", tok)
	req = req.WithContext(wmw.WithSession(req.Context(), &domain.Session{ID: "sid-q"}))

	if !VerifyCSRF(req, secret) {
		t.Fatal("VerifyCSRF should accept legacy psg_csrf")
	}
}

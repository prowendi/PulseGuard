package web

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/prowendi/PulseGuard/internal/config"
	"github.com/prowendi/PulseGuard/internal/domain"
	wmw "github.com/prowendi/PulseGuard/internal/web/middleware"
)

// TestServerHealthz boots NewServer with a minimal Deps bundle and
// verifies the unauthenticated /healthz probe returns 200 "ok".
func TestServerHealthz(t *testing.T) {
	h := NewServer(Deps{})
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("get /healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(body) != "ok" {
		t.Fatalf("body = %q, want 'ok'", body)
	}
}

// TestServerStaticAsset ensures the embedded /static/app.css is served.
func TestServerStaticAsset(t *testing.T) {
	h := NewServer(Deps{})
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/static/app.css")
	if err != nil {
		t.Fatalf("get static: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !strings.Contains(string(body), "PulseGuard") {
		t.Fatalf("css body missing marker: %s", string(body)[:60])
	}
}

// TestCSRFTokensRoundTrip verifies that IssueCSRF emits a cookie which
// VerifyCSRF accepts only when the header matches AND the HMAC binding
// to the session id remains intact.
func TestCSRFTokensRoundTrip(t *testing.T) {
	secret := []byte("super-secret-key-bytes-32-long_!")
	const sid = "sess-123"

	rec := httptest.NewRecorder()
	tok := IssueCSRF(rec, sid, secret, false)
	if tok == "" {
		t.Fatal("IssueCSRF returned empty token")
	}
	if len(rec.Result().Cookies()) == 0 {
		t.Fatal("expected csrf cookie")
	}
	cookie := rec.Result().Cookies()[0]
	if cookie.Name != CookieCSRF || cookie.Value != tok {
		t.Fatalf("cookie mismatch: %+v vs %q", cookie, tok)
	}

	// We need a request whose context carries the same session id so
	// VerifyCSRF can rebuild the HMAC.
	withSess := func(r *http.Request) *http.Request {
		ctx := wmw.WithSession(r.Context(), &domain.Session{ID: sid})
		return r.WithContext(ctx)
	}

	// Header matches → verify true.
	r := withSess(httptest.NewRequest(http.MethodPost, "/x", nil))
	r.AddCookie(&http.Cookie{Name: CookieCSRF, Value: tok})
	r.Header.Set(HeaderCSRF, tok)
	if !VerifyCSRF(r, secret) {
		t.Fatal("VerifyCSRF should accept matching header bound to session")
	}

	// Header missing → verify false.
	r2 := withSess(httptest.NewRequest(http.MethodPost, "/x", nil))
	r2.AddCookie(&http.Cookie{Name: CookieCSRF, Value: tok})
	if VerifyCSRF(r2, secret) {
		t.Fatal("VerifyCSRF should reject missing header")
	}

	// Header tampered → verify false.
	r3 := withSess(httptest.NewRequest(http.MethodPost, "/x", nil))
	r3.AddCookie(&http.Cookie{Name: CookieCSRF, Value: tok})
	r3.Header.Set(HeaderCSRF, tok+"x")
	if VerifyCSRF(r3, secret) {
		t.Fatal("VerifyCSRF should reject tampered header")
	}

	// Cookie HMAC for a DIFFERENT session must NOT verify under our
	// session id — defends against cookie-injection via a sibling
	// subdomain (security-report S-M2).
	otherRec := httptest.NewRecorder()
	otherTok := IssueCSRF(otherRec, "sess-OTHER", secret, false)
	r4 := withSess(httptest.NewRequest(http.MethodPost, "/x", nil))
	r4.AddCookie(&http.Cookie{Name: CookieCSRF, Value: otherTok})
	r4.Header.Set(HeaderCSRF, otherTok)
	if VerifyCSRF(r4, secret) {
		t.Fatal("VerifyCSRF must reject token bound to a different session")
	}

	// Safe method → always allowed.
	rg := withSess(httptest.NewRequest(http.MethodGet, "/x", nil))
	if !VerifyCSRF(rg, secret) {
		t.Fatal("VerifyCSRF should allow GET")
	}
}

// TestEnsureCSRFCookieAutoIssuesOnFirstAuthedRequest is the regression
// guard for code-review-report C-I4: a user that already holds a valid
// session cookie but lacks the psg_csrf cookie (e.g. cleared by the
// browser, or first navigation after upgrade) must NOT get a 403 from
// CSRFCheck — the EnsureCSRFCookie middleware should issue one before
// CSRFCheck runs. Because the auto-issuer only attaches the cookie to
// the response and CSRFCheck reads from the request, the FIRST POST
// against an authed endpoint will still 403; subsequent ones succeed.
// What we assert here: a GET against /api/v1/me without a csrf cookie
// returns 200 AND attaches a fresh psg_csrf cookie in Set-Cookie.
func TestEnsureCSRFCookieAutoIssuesOnFirstAuthedRequest(t *testing.T) {
	h := newTestHarness(t)
	_, _ = h.seedAdmin("admin@example.com", "adminpass", "INVITE-OK")

	// Register to obtain a session, then strip the csrf cookie from the
	// jar to simulate a stale browser state.
	c := h.newJarClient()
	regBody := mustJSON(t, map[string]any{
		"email":       "alice@example.com",
		"password":    "alicepass",
		"invite_code": "INVITE-OK",
	})
	resp, err := c.Post(h.fullURL("/api/v1/auth/register"), "application/json",
		bytes.NewReader(regBody))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	resp.Body.Close()

	// Drop the csrf cookie by walking the jar and rewriting it to expire.
	clearJarCookie(t, c, h.server.URL, CookieCSRF)
	if v := jarValue(t, c, h.server.URL, CookieCSRF); v != "" {
		t.Fatalf("csrf cookie should be cleared, still %q", v)
	}

	// Issue a GET — RequireAuth resolves the session, EnsureCSRFCookie
	// detects the missing cookie and writes a fresh one.
	getReq, _ := http.NewRequest(http.MethodGet, h.fullURL("/api/v1/me"), nil)
	resp, err = c.Do(getReq)
	if err != nil {
		t.Fatalf("me: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("me status = %d", resp.StatusCode)
	}
	resp.Body.Close()

	if v := jarValue(t, c, h.server.URL, CookieCSRF); v == "" {
		t.Fatalf("EnsureCSRFCookie did not auto-issue psg_csrf")
	}
}

// clearJarCookie expires the named cookie in the jar attached to base.
func clearJarCookie(t *testing.T, c *http.Client, base, name string) {
	t.Helper()
	u, _ := url.Parse(base)
	expired := &http.Cookie{Name: name, Value: "", Path: "/", MaxAge: -1}
	c.Jar.SetCookies(u, []*http.Cookie{expired})
}
func TestSecureHeadersOnEveryResponse(t *testing.T) {
	cases := []struct {
		name       string
		secure     bool
		wantHSTS   bool
	}{
		{name: "http_no_hsts", secure: false, wantHSTS: false},
		{name: "https_with_hsts", secure: true, wantHSTS: true},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Security.CookieSecure = c.secure
			h := NewServer(Deps{Cfg: cfg})
			srv := httptest.NewServer(h)
			defer srv.Close()

			resp, err := http.Get(srv.URL + "/healthz")
			if err != nil {
				t.Fatalf("get /healthz: %v", err)
			}
			defer resp.Body.Close()

			want := map[string]string{
				"X-Frame-Options":        "DENY",
				"X-Content-Type-Options": "nosniff",
				"Referrer-Policy":        "strict-origin-when-cross-origin",
			}
			for k, v := range want {
				if got := resp.Header.Get(k); got != v {
					t.Fatalf("header %s = %q, want %q", k, got, v)
				}
			}
			csp := resp.Header.Get("Content-Security-Policy")
			if !strings.Contains(csp, "default-src 'self'") {
				t.Fatalf("CSP missing default-src 'self': %q", csp)
			}
			if !strings.Contains(csp, "frame-ancestors 'none'") {
				t.Fatalf("CSP missing frame-ancestors: %q", csp)
			}
			hsts := resp.Header.Get("Strict-Transport-Security")
			if c.wantHSTS {
				if hsts == "" {
					t.Fatalf("HSTS header missing under cookie_secure=true")
				}
			} else if hsts != "" {
				t.Fatalf("HSTS unexpectedly set when cookie_secure=false: %q", hsts)
			}
		})
	}
}

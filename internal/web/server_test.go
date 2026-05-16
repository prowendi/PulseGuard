package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
// VerifyCSRF accepts only when the header matches.
func TestCSRFTokensRoundTrip(t *testing.T) {
	rec := httptest.NewRecorder()
	tok := IssueCSRF(rec, false)
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

	// Header matches → verify true.
	r := httptest.NewRequest(http.MethodPost, "/x", nil)
	r.AddCookie(&http.Cookie{Name: CookieCSRF, Value: tok})
	r.Header.Set(HeaderCSRF, tok)
	if !VerifyCSRF(r) {
		t.Fatal("VerifyCSRF should accept matching header")
	}

	// Header missing → verify false.
	r2 := httptest.NewRequest(http.MethodPost, "/x", nil)
	r2.AddCookie(&http.Cookie{Name: CookieCSRF, Value: tok})
	if VerifyCSRF(r2) {
		t.Fatal("VerifyCSRF should reject missing header")
	}

	// Header tampered → verify false.
	r3 := httptest.NewRequest(http.MethodPost, "/x", nil)
	r3.AddCookie(&http.Cookie{Name: CookieCSRF, Value: tok})
	r3.Header.Set(HeaderCSRF, tok+"x")
	if VerifyCSRF(r3) {
		t.Fatal("VerifyCSRF should reject tampered header")
	}

	// Safe method → always allowed.
	rg := httptest.NewRequest(http.MethodGet, "/x", nil)
	if !VerifyCSRF(rg) {
		t.Fatal("VerifyCSRF should allow GET")
	}
}

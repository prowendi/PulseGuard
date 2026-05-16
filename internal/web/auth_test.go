package web

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestAuthRegisterLoginMeLogout exercises the full session lifecycle.
func TestAuthRegisterLoginMeLogout(t *testing.T) {
	h := newTestHarness(t)
	_, _ = h.seedAdmin("admin@example.com", "adminpass", "INVITE-OK")

	c := h.newJarClient()

	// 1. Register with the invite code.
	regBody := mustJSON(t, map[string]any{
		"email":       "alice@example.com",
		"password":    "alicepass",
		"invite_code": "INVITE-OK",
	})
	resp, err := c.Post(h.fullURL("/api/v1/auth/register"), "application/json", bytes.NewReader(regBody))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("register status = %d, body=%s", resp.StatusCode, drain(resp))
	}
	resp.Body.Close()

	// 2. CSRF + session cookies should be present in the jar.
	if v := jarValue(t, c, h.server.URL, "psg_session"); v == "" {
		t.Fatal("psg_session cookie missing after register")
	}
	csrf := jarValue(t, c, h.server.URL, "psg_csrf")
	if csrf == "" {
		t.Fatal("psg_csrf cookie missing after register")
	}

	// 3. Hitting /api/v1/me with the session cookie returns the tenant.
	mereq, _ := http.NewRequest(http.MethodGet, h.fullURL("/api/v1/me"), nil)
	resp, err = c.Do(mereq)
	if err != nil {
		t.Fatalf("me: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("me status = %d, body=%s", resp.StatusCode, drain(resp))
	}
	var meBody map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&meBody)
	resp.Body.Close()
	tenantObj, _ := meBody["tenant"].(map[string]any)
	if got, _ := tenantObj["email"].(string); got != "alice@example.com" {
		t.Fatalf("me tenant email = %q", got)
	}

	// 4. Logout: needs CSRF header.
	logoutReq, _ := http.NewRequest(http.MethodPost, h.fullURL("/api/v1/auth/logout"), nil)
	logoutReq.Header.Set("X-CSRF-Token", csrf)
	resp, err = c.Do(logoutReq)
	if err != nil {
		t.Fatalf("logout: %v", err)
	}
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("logout status = %d, body=%s", resp.StatusCode, drain(resp))
	}
	resp.Body.Close()

	// 5. /api/v1/me without a session must return 401 JSON.
	mereq2, _ := http.NewRequest(http.MethodGet, h.fullURL("/api/v1/me"), nil)
	resp, err = c.Do(mereq2)
	if err != nil {
		t.Fatalf("me after logout: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("me after logout status = %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestAuthLoginInvalid covers wrong credentials → 401.
func TestAuthLoginInvalid(t *testing.T) {
	h := newTestHarness(t)
	_, _ = h.seedAdmin("admin@example.com", "adminpass", "INVITE-OK")

	c := h.newJarClient()
	body := mustJSON(t, map[string]any{"email": "admin@example.com", "password": "nope"})
	resp, err := c.Post(h.fullURL("/api/v1/auth/login"), "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("login status = %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestAuthRegisterInvalidInvite covers a bogus invite → 400.
func TestAuthRegisterInvalidInvite(t *testing.T) {
	h := newTestHarness(t)
	_, _ = h.seedAdmin("admin@example.com", "adminpass", "GOOD")

	c := h.newJarClient()
	body := mustJSON(t, map[string]any{
		"email":       "user@example.com",
		"password":    "userpass",
		"invite_code": "DOES-NOT-EXIST",
	})
	resp, err := c.Post(h.fullURL("/api/v1/auth/register"), "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("register status = %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestUILoginGetRendersForm verifies /ui/login returns an HTML page with
// a CSRF input and the expected form action.
func TestUILoginGetRendersForm(t *testing.T) {
	h := newTestHarness(t)
	c := h.newJarClient()
	resp, err := c.Get(h.fullURL("/ui/login"))
	if err != nil {
		t.Fatalf("get login: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	bs, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	body := string(bs)
	if !strings.Contains(body, `action="/ui/login"`) {
		t.Fatalf("body missing form action; got: %s", body[:200])
	}
	if !strings.Contains(body, `name="csrf"`) {
		t.Fatalf("body missing csrf input")
	}
}

// TestUILoginPostHappyPath verifies form login establishes a session
// cookie and redirects to /ui/dashboard.
func TestUILoginPostHappyPath(t *testing.T) {
	h := newTestHarness(t)
	_, _ = h.seedAdmin("admin@example.com", "adminpass", "INVITE-OK")

	c := h.newJarClient()
	// Visit /ui/login to receive a CSRF cookie.
	gresp, err := c.Get(h.fullURL("/ui/login"))
	if err != nil {
		t.Fatalf("get login: %v", err)
	}
	gresp.Body.Close()
	csrf := jarValue(t, c, h.server.URL, "psg_csrf")
	if csrf == "" {
		t.Fatal("csrf cookie missing after GET")
	}

	form := strings.NewReader("email=admin@example.com&password=adminpass&csrf=" + csrf)
	req, _ := http.NewRequest(http.MethodPost, h.fullURL("/ui/login"), form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("post login: %v", err)
	}
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d (want 303)", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != "/ui/dashboard" {
		t.Fatalf("redirect = %q", got)
	}
	resp.Body.Close()
	if jarValue(t, c, h.server.URL, "psg_session") == "" {
		t.Fatal("session cookie missing after login")
	}
}

// TestUILogoutRequiresCSRF ensures POST /ui/logout without CSRF redirects
// back to dashboard without dropping the session.
func TestUILogoutWithoutCSRFRedirects(t *testing.T) {
	h := newTestHarness(t)
	_, _ = h.seedAdmin("admin@example.com", "adminpass", "INVITE-OK")
	c := loginUIClient(t, h, "admin@example.com", "adminpass")

	req, _ := http.NewRequest(http.MethodPost, h.fullURL("/ui/logout"), nil)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("logout no csrf: %v", err)
	}
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	resp.Body.Close()
	if jarValue(t, c, h.server.URL, "psg_session") == "" {
		t.Fatal("session cookie should still exist (logout was csrf-rejected)")
	}
}

// ---- helpers -----------------------------------------------------------

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	bs, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return bs
}

func drain(resp *http.Response) string {
	defer resp.Body.Close()
	bs, _ := io.ReadAll(resp.Body)
	return string(bs)
}

// loginUIClient walks the UI login flow and returns the authenticated
// client. Tests that need an authenticated client without re-validating
// the auth flow itself should use this helper.
func loginUIClient(t *testing.T, h *testHarness, email, password string) *http.Client {
	t.Helper()
	c := h.newJarClient()
	gresp, err := c.Get(h.fullURL("/ui/login"))
	if err != nil {
		t.Fatalf("ui get login: %v", err)
	}
	gresp.Body.Close()
	csrf := jarValue(t, c, h.server.URL, "psg_csrf")
	if csrf == "" {
		t.Fatal("csrf cookie missing")
	}
	form := strings.NewReader("email=" + email + "&password=" + password + "&csrf=" + csrf)
	req, _ := http.NewRequest(http.MethodPost, h.fullURL("/ui/login"), form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("ui login: %v", err)
	}
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("ui login status = %d", resp.StatusCode)
	}
	resp.Body.Close()
	return c
}

package web

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/wendi/pulseguard/internal/auth"
	"github.com/wendi/pulseguard/internal/domain"
)

// adminAPIClient registers no one — it logs straight in as the admin
// seeded via h.seedAdmin and returns the (client, csrf) pair.
func adminAPIClient(t *testing.T, h *testHarness, email, pwd string) (*http.Client, string) {
	t.Helper()
	c := h.newJarClient()
	body := mustJSON(t, map[string]any{"email": email, "password": pwd})
	resp, err := c.Post(h.fullURL("/api/v1/auth/login"), "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("admin login: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin login status = %d body=%s", resp.StatusCode, drain(resp))
	}
	resp.Body.Close()
	csrf := jarValue(t, c, h.server.URL, "psg_csrf")
	if csrf == "" {
		t.Fatal("admin login csrf missing")
	}
	return c, csrf
}

func TestInvitesAPILifecycle(t *testing.T) {
	h := newTestHarness(t)
	_, _ = h.seedAdmin("admin@example.com", "adminpass", "BOOTSTRAP")
	client, csrf := adminAPIClient(t, h, "admin@example.com", "adminpass")

	// List initially returns the seeded bootstrap invite.
	resp, err := client.Get(h.fullURL("/api/v1/invites"))
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d body=%s", resp.StatusCode, drain(resp))
	}
	var listBody map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&listBody)
	resp.Body.Close()
	if items, _ := listBody["items"].([]any); len(items) != 1 {
		t.Fatalf("expected 1 seeded invite, got %d", len(items))
	}

	// Generate 3 invites with a 60s TTL.
	body := mustJSON(t, map[string]any{"count": 3, "ttl_seconds": 60})
	req, _ := http.NewRequest(http.MethodPost, h.fullURL("/api/v1/invites"), bytes.NewReader(body))
	resp, err = client.Do(withCSRF(req, csrf))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d body=%s", resp.StatusCode, drain(resp))
	}
	var createBody map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&createBody)
	resp.Body.Close()
	items, _ := createBody["items"].([]any)
	if len(items) != 3 {
		t.Fatalf("create returned %d items", len(items))
	}
	first, _ := items[0].(map[string]any)
	firstCode, _ := first["code"].(string)
	if firstCode == "" {
		t.Fatal("first code is empty")
	}
	if exp, _ := first["expires_at"].(string); exp == "" {
		t.Fatal("expires_at should be populated for ttl>0")
	}

	// Final list now has 4.
	resp, _ = client.Get(h.fullURL("/api/v1/invites"))
	_ = json.NewDecoder(resp.Body).Decode(&listBody)
	resp.Body.Close()
	if items, _ := listBody["items"].([]any); len(items) != 4 {
		t.Fatalf("expected 4 invites after creation, got %d", len(items))
	}

	// Delete the first newly-created one.
	req, _ = http.NewRequest(http.MethodDelete, h.fullURL("/api/v1/invites/"+firstCode), nil)
	resp, err = client.Do(withCSRF(req, csrf))
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status = %d body=%s", resp.StatusCode, drain(resp))
	}
	resp.Body.Close()

	// Confirm down to 3.
	resp, _ = client.Get(h.fullURL("/api/v1/invites"))
	_ = json.NewDecoder(resp.Body).Decode(&listBody)
	resp.Body.Close()
	if items, _ := listBody["items"].([]any); len(items) != 3 {
		t.Fatalf("post-delete invites = %d", len(items))
	}
}

func TestInvitesAPICountValidation(t *testing.T) {
	h := newTestHarness(t)
	_, _ = h.seedAdmin("admin@example.com", "adminpass", "BOOTSTRAP")
	client, csrf := adminAPIClient(t, h, "admin@example.com", "adminpass")

	// count > 100 -> 400.
	body := mustJSON(t, map[string]any{"count": 101, "ttl_seconds": 0})
	req, _ := http.NewRequest(http.MethodPost, h.fullURL("/api/v1/invites"), bytes.NewReader(body))
	resp, err := client.Do(withCSRF(req, csrf))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d (want 400)", resp.StatusCode)
	}
	resp.Body.Close()

	// negative ttl -> 400.
	body = mustJSON(t, map[string]any{"count": 1, "ttl_seconds": -5})
	req, _ = http.NewRequest(http.MethodPost, h.fullURL("/api/v1/invites"), bytes.NewReader(body))
	resp, err = client.Do(withCSRF(req, csrf))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d (want 400)", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestInvitesAPIRejectsNonAdmin(t *testing.T) {
	h := newTestHarness(t)
	_, _ = h.seedAdmin("admin@example.com", "adminpass", "BOOTSTRAP")
	// Register a regular tenant via the bootstrap invite.
	client, csrf := registerTenantAPI(t, h, "alice@example.com", "alicepass", "BOOTSTRAP")

	// GET /invites -> 403.
	resp, err := client.Get(h.fullURL("/api/v1/invites"))
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-admin list status = %d, want 403", resp.StatusCode)
	}
	resp.Body.Close()

	// POST /invites -> 403.
	body := mustJSON(t, map[string]any{"count": 1})
	req, _ := http.NewRequest(http.MethodPost, h.fullURL("/api/v1/invites"), bytes.NewReader(body))
	resp, err = client.Do(withCSRF(req, csrf))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-admin create status = %d, want 403", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestInvitesAPIDeleteCrossAdmin(t *testing.T) {
	h := newTestHarness(t)
	_, _ = h.seedAdmin("admin1@example.com", "adminpass", "B1")

	// Seed a second admin and one of their unused invites.
	hashed, _ := bcryptHash(t, "adminpass")
	admin2 := &domain.Tenant{
		Email: "admin2@example.com", PasswordHash: hashed,
		Role: domain.RoleAdmin, Status: domain.TenantActive,
	}
	_ = h.deps.Tenants.Insert(context.Background(), admin2)
	other := &domain.InviteCode{Code: "OTHER", CreatedBy: admin2.ID}
	_ = h.deps.Invites.Insert(context.Background(), other)

	client, csrf := adminAPIClient(t, h, "admin1@example.com", "adminpass")
	req, _ := http.NewRequest(http.MethodDelete, h.fullURL("/api/v1/invites/OTHER"), nil)
	resp, err := client.Do(withCSRF(req, csrf))
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-admin delete status = %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestInvitesAPIDeleteConsumed(t *testing.T) {
	h := newTestHarness(t)
	_, _ = h.seedAdmin("admin@example.com", "adminpass", "BOOTSTRAP")
	// Register a tenant which consumes BOOTSTRAP.
	_, _ = registerTenantAPI(t, h, "alice@example.com", "alicepass", "BOOTSTRAP")

	// Admin tries to delete the consumed invite -> 409.
	client, csrf := adminAPIClient(t, h, "admin@example.com", "adminpass")
	req, _ := http.NewRequest(http.MethodDelete, h.fullURL("/api/v1/invites/BOOTSTRAP"), nil)
	resp, err := client.Do(withCSRF(req, csrf))
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("consumed delete status = %d, want 409", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestUIInvitesAdminOnly(t *testing.T) {
	h := newTestHarness(t)
	_, _ = h.seedAdmin("admin@example.com", "adminpass", "BOOTSTRAP")

	// Admin: 200 with heading.
	adminClient, _ := adminAPIClient(t, h, "admin@example.com", "adminpass")
	resp, err := adminClient.Get(h.fullURL("/ui/invites"))
	if err != nil {
		t.Fatalf("admin get: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin status = %d", resp.StatusCode)
	}
	bs, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(bs), "邀请码") || !strings.Contains(string(bs), "管理员") {
		t.Fatalf("admin body missing heading: %s", string(bs)[:200])
	}

	// Non-admin: 303 redirect to /ui/dashboard.
	userClient, _ := registerTenantAPI(t, h, "alice@example.com", "alicepass", "BOOTSTRAP")
	resp, err = userClient.Get(h.fullURL("/ui/invites"))
	if err != nil {
		t.Fatalf("user get: %v", err)
	}
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("non-admin status = %d, want 303", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != "/ui/dashboard" {
		t.Fatalf("non-admin redirect = %q", got)
	}
	resp.Body.Close()
}

// bcryptHash hashes a password using the same fast cost (4) as the
// rest of the web test suite, so direct-insert helpers can mint
// password_hash values without going through the auth.Service path.
func bcryptHash(t *testing.T, password string) (string, error) {
	t.Helper()
	b, err := auth.HashPassword(password, 4)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

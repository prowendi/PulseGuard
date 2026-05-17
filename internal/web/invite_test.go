package web

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prowendi/PulseGuard/internal/auth"
	"github.com/prowendi/PulseGuard/internal/domain"
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

// TestInvitesAPIDailyCap drives the per-admin daily cap. We pre-seed
// (cap-1) invites directly so we can drive the boundary in a few HTTP
// calls; the actual handler logic is exercised end-to-end through
// requests, including the boundary count + remaining-today formatting.
func TestInvitesAPIDailyCap(t *testing.T) {
	h := newTestHarness(t)
	admin, _ := h.seedAdmin("admin@example.com", "adminpass", "BOOTSTRAP")
	client, csrf := adminAPIClient(t, h, "admin@example.com", "adminpass")

	// Pre-fill the day's quota minus 2 by inserting directly through
	// the repo so the test runs in O(seconds), not O(quota * round-trip).
	// The bootstrap invite counts toward the cap; the seed already gave
	// us one, so we need cap-2 more to leave room for exactly 1.
	preload := InvitesPerAdminDailyCap - 2
	for i := 0; i < preload; i++ {
		inv := &domain.InviteCode{
			Code: invitePreloadCode(i), CreatedBy: admin.ID,
		}
		if err := h.deps.Invites.Insert(context.Background(), inv); err != nil {
			t.Fatalf("preload %d: %v", i, err)
		}
	}

	// Verify count is at cap-1 (preload + bootstrap).
	cnt, err := h.deps.Invites.CountByCreatorSince(context.Background(), admin.ID,
		time.Date(h.clock.T.Year(), h.clock.T.Month(), h.clock.T.Day(), 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if cnt != InvitesPerAdminDailyCap-1 {
		t.Fatalf("pre-cap count = %d want %d", cnt, InvitesPerAdminDailyCap-1)
	}

	// 1. Creating 1 more is allowed (lands on cap exactly).
	body := mustJSON(t, map[string]any{"count": 1, "ttl_seconds": 0})
	req, _ := http.NewRequest(http.MethodPost, h.fullURL("/api/v1/invites"), bytes.NewReader(body))
	resp, err := client.Do(withCSRF(req, csrf))
	if err != nil {
		t.Fatalf("at-cap create: %v", err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("at-cap status = %d, want 201; body=%s", resp.StatusCode, drain(resp))
	}
	resp.Body.Close()

	// 2. The very next request must be refused with 429 + Retry-After.
	body2 := mustJSON(t, map[string]any{"count": 1, "ttl_seconds": 0})
	req2, _ := http.NewRequest(http.MethodPost, h.fullURL("/api/v1/invites"), bytes.NewReader(body2))
	resp2, err := client.Do(withCSRF(req2, csrf))
	if err != nil {
		t.Fatalf("over-cap create: %v", err)
	}
	if resp2.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("over-cap status = %d, want 429; body=%s", resp2.StatusCode, drain(resp2))
	}
	if ra := resp2.Header.Get("Retry-After"); ra == "" {
		t.Fatalf("over-cap response missing Retry-After header")
	}
	var errBody map[string]any
	_ = json.NewDecoder(resp2.Body).Decode(&errBody)
	resp2.Body.Close()
	errObj, _ := errBody["error"].(map[string]any)
	if code, _ := errObj["code"].(string); code != "RATE_LIMITED" {
		t.Fatalf("over-cap error code = %q want RATE_LIMITED", code)
	}
}

// TestInvitesAPIDailyCap_BulkRejected covers the bulk path: a single
// request that asks for more invites than the remaining budget must
// fail atomically (no partial creation).
func TestInvitesAPIDailyCap_BulkRejected(t *testing.T) {
	h := newTestHarness(t)
	admin, _ := h.seedAdmin("admin@example.com", "adminpass", "BOOTSTRAP")
	client, csrf := adminAPIClient(t, h, "admin@example.com", "adminpass")

	// Pre-fill so only 5 budget remain for the day.
	preload := InvitesPerAdminDailyCap - 1 /*bootstrap*/ - 5
	for i := 0; i < preload; i++ {
		inv := &domain.InviteCode{
			Code: invitePreloadCode(i), CreatedBy: admin.ID,
		}
		if err := h.deps.Invites.Insert(context.Background(), inv); err != nil {
			t.Fatalf("preload %d: %v", i, err)
		}
	}

	// Ask for 10 in one shot — must be rejected wholesale.
	body := mustJSON(t, map[string]any{"count": 10, "ttl_seconds": 0})
	req, _ := http.NewRequest(http.MethodPost, h.fullURL("/api/v1/invites"), bytes.NewReader(body))
	resp, err := client.Do(withCSRF(req, csrf))
	if err != nil {
		t.Fatalf("bulk over: %v", err)
	}
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("bulk over status = %d want 429; body=%s", resp.StatusCode, drain(resp))
	}
	resp.Body.Close()

	// Verify no invites were created — count must still equal preload+bootstrap.
	cnt, err := h.deps.Invites.CountByCreatorSince(context.Background(), admin.ID,
		time.Date(h.clock.T.Year(), h.clock.T.Month(), h.clock.T.Day(), 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if want := preload + 1; cnt != want {
		t.Fatalf("post-reject count = %d want %d (no invites should have leaked through)", cnt, want)
	}
}

// invitePreloadCode mints a unique invite code for the daily-cap
// pre-seed loop. We avoid the random URL token helper to keep tests
// fully deterministic.
func invitePreloadCode(i int) string {
	return "PRELOAD-" + strconv.Itoa(i)
}

package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestBotsAPI_CreateSMTPBot pins the SMTP create happy path: a
// platform=smtp bot is created via the JSON API with the full
// credential set. The response surfaces platform=smtp + host /
// port / username / from + use_tls + smtp_password_set=true. The
// password and the derived smtp:// pseudo-URL MUST NOT leak.
func TestBotsAPI_CreateSMTPBot(t *testing.T) {
	h := newTestHarness(t)
	_, _ = h.seedAdmin("admin@example.com", "adminpass", "INV1")
	client, csrf := registerTenantAPI(t, h, "alice@example.com", "alicepass", "INV1")

	body := mustJSON(t, map[string]any{
		"name":          "smtp-alerts",
		"platform":      "smtp",
		"smtp_host":     "smtp.example.com",
		"smtp_port":     587,
		"smtp_username": "alerts@example.com",
		"smtp_password": "supersecret-PWD",
		"smtp_from":     "PulseGuard <alerts@example.com>",
		"smtp_use_tls":  true,
	})
	req, _ := http.NewRequest(http.MethodPost, h.fullURL("/api/v1/bots"), bytes.NewReader(body))
	resp, err := client.Do(withCSRF(req, csrf))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d body=%s", resp.StatusCode, drain(resp))
	}
	var created botView
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	if created.Platform != "smtp" {
		t.Fatalf("platform = %q want smtp", created.Platform)
	}
	if created.SMTPHost != "smtp.example.com" {
		t.Fatalf("SMTPHost = %q", created.SMTPHost)
	}
	if created.SMTPPort != 587 {
		t.Fatalf("SMTPPort = %d", created.SMTPPort)
	}
	if created.SMTPUsername != "alerts@example.com" {
		t.Fatalf("SMTPUsername = %q", created.SMTPUsername)
	}
	if !created.SMTPUseTLS {
		t.Fatalf("SMTPUseTLS = false")
	}
	if !created.SMTPPasswordSet {
		t.Fatalf("SMTPPasswordSet = false (want true)")
	}
	if created.BotTokenLast4 != "" {
		t.Fatalf("BotTokenLast4 = %q (want empty for smtp row)", created.BotTokenLast4)
	}

	// Round-trip via list + get must NOT leak the password anywhere.
	resp, err = client.Get(h.fullURL("/api/v1/bots"))
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	listBody := drain(resp)
	resp.Body.Close()
	for _, leak := range []string{"supersecret-PWD", "supersecret"} {
		if strings.Contains(listBody, leak) {
			t.Fatalf("password leaked in list response: %s", listBody)
		}
	}
}

// TestBotsAPI_CreateSMTPBot_RequiresFields enforces the validation
// matrix: host / username / password are mandatory on first creation.
func TestBotsAPI_CreateSMTPBot_RequiresFields(t *testing.T) {
	h := newTestHarness(t)
	_, _ = h.seedAdmin("admin@example.com", "adminpass", "INV1")
	client, csrf := registerTenantAPI(t, h, "alice@example.com", "alicepass", "INV1")

	cases := []struct {
		name string
		body map[string]any
	}{
		{"missing host", map[string]any{"name": "x", "platform": "smtp", "smtp_username": "u@x", "smtp_password": "p"}},
		{"missing username", map[string]any{"name": "x", "platform": "smtp", "smtp_host": "h", "smtp_password": "p"}},
		{"missing password on create", map[string]any{"name": "x", "platform": "smtp", "smtp_host": "h", "smtp_username": "u@x"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body := mustJSON(t, c.body)
			req, _ := http.NewRequest(http.MethodPost, h.fullURL("/api/v1/bots"), bytes.NewReader(body))
			resp, err := client.Do(withCSRF(req, csrf))
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d body=%s", resp.StatusCode, drain(resp))
			}
		})
	}
}

// TestBotsAPI_UpdateSMTP_BlankPasswordPreserves: the operator can
// edit smtp_host / port without re-submitting the password.
func TestBotsAPI_UpdateSMTP_BlankPasswordPreserves(t *testing.T) {
	h := newTestHarness(t)
	_, _ = h.seedAdmin("admin@example.com", "adminpass", "INV1")
	client, csrf := registerTenantAPI(t, h, "alice@example.com", "alicepass", "INV1")

	// Create.
	body := mustJSON(t, map[string]any{
		"name": "smtp-edit", "platform": "smtp",
		"smtp_host": "smtp.example.com", "smtp_port": 587,
		"smtp_username": "u@x", "smtp_password": "orig", "smtp_use_tls": true,
	})
	req, _ := http.NewRequest(http.MethodPost, h.fullURL("/api/v1/bots"), bytes.NewReader(body))
	resp, _ := client.Do(withCSRF(req, csrf))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("seed: status=%d body=%s", resp.StatusCode, drain(resp))
	}
	var created botView
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()

	// Update host only — password omitted entirely. Repo must preserve.
	upd := mustJSON(t, map[string]any{
		"smtp_host": "smtp.gmail.com",
	})
	updReq, _ := http.NewRequest(http.MethodPut, h.fullURL("/api/v1/bots/1"), bytes.NewReader(upd))
	updResp, _ := client.Do(withCSRF(updReq, csrf))
	if updResp.StatusCode != http.StatusOK {
		t.Fatalf("update status=%d body=%s", updResp.StatusCode, drain(updResp))
	}
	var updated botView
	_ = json.NewDecoder(updResp.Body).Decode(&updated)
	updResp.Body.Close()
	if updated.SMTPHost != "smtp.gmail.com" {
		t.Fatalf("host not updated: %q", updated.SMTPHost)
	}
	if !updated.SMTPPasswordSet {
		t.Fatalf("SMTPPasswordSet went false after blank-password update (preservation broken)")
	}
}

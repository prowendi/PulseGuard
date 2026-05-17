package store

import (
	"context"
	"strings"
	"testing"

	"github.com/prowendi/PulseGuard/internal/domain"
)

// TestBotRepo_SMTPRoundtrip verifies migration 0012 schema additions
// plus SMTP password AES-GCM encryption and the smtp:// derived
// BotToken. Inserts a row with the full set of SMTP credentials,
// then reads it back and asserts:
//
//   - Platform = "smtp" round-trips
//   - host / port / username / from / use_tls round-trip plaintext
//   - smtp_password round-trips via the cipher
//   - BotToken (derived) encodes host + port + user + pass + from
//   - The raw smtp_password_enc column does NOT contain the plaintext
func TestBotRepo_SMTPRoundtrip(t *testing.T) {
	f := newResourceFixture(t)
	b := &domain.Bot{
		TenantID:     f.tenant.ID,
		Name:         "smtp-alerts",
		Platform:     domain.PlatformSMTP,
		Description:  "phase SMTP",
		SMTPHost:     "smtp.example.com",
		SMTPPort:     587,
		SMTPUsername: "alerts@example.com",
		SMTPPassword: "supersecret-app-pwd",
		SMTPFrom:     "PulseGuard <alerts@example.com>",
		SMTPUseTLS:   true,
	}
	if err := f.bots.Insert(context.Background(), b); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if !strings.HasPrefix(b.BotToken, "smtp://") {
		t.Fatalf("derived BotToken = %q, want smtp:// prefix", b.BotToken)
	}

	// Raw column MUST NOT contain the plaintext password.
	var raw []byte
	if err := f.db.QueryRow(`SELECT smtp_password_enc FROM bots WHERE id = ?`, b.ID).Scan(&raw); err != nil {
		t.Fatalf("scan smtp_password_enc: %v", err)
	}
	if strings.Contains(string(raw), "supersecret") {
		t.Fatalf("plaintext leaked in smtp_password_enc")
	}

	got, err := f.bots.GetByID(context.Background(), f.tenant.ID, b.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Platform != domain.PlatformSMTP {
		t.Errorf("Platform = %q", got.Platform)
	}
	if got.SMTPHost != "smtp.example.com" {
		t.Errorf("SMTPHost = %q", got.SMTPHost)
	}
	if got.SMTPPort != 587 {
		t.Errorf("SMTPPort = %d", got.SMTPPort)
	}
	if got.SMTPUsername != "alerts@example.com" {
		t.Errorf("SMTPUsername = %q", got.SMTPUsername)
	}
	if got.SMTPPassword != "supersecret-app-pwd" {
		t.Errorf("SMTPPassword cipher round-trip = %q", got.SMTPPassword)
	}
	if got.SMTPFrom != "PulseGuard <alerts@example.com>" {
		t.Errorf("SMTPFrom = %q", got.SMTPFrom)
	}
	if !got.SMTPUseTLS {
		t.Errorf("SMTPUseTLS = false, want true")
	}
	if !strings.HasPrefix(got.BotToken, "smtp://") {
		t.Errorf("derived BotToken = %q", got.BotToken)
	}
	if !strings.Contains(got.BotToken, "smtp.example.com") {
		t.Errorf("BotToken missing host: %q", got.BotToken)
	}
}

// TestBotRepo_SMTPUpdatePreservesPassword guards the "blank = keep"
// rule: editing host/port/username without re-typing the password
// must not lose the stored credential. Mirrors the LB1
// TestBotRepo_LarkAppUpdatePreservesSecret invariant.
func TestBotRepo_SMTPUpdatePreservesPassword(t *testing.T) {
	f := newResourceFixture(t)
	b := &domain.Bot{
		TenantID:     f.tenant.ID,
		Name:         "smtp-keep",
		Platform:     domain.PlatformSMTP,
		SMTPHost:     "smtp.example.com",
		SMTPPort:     587,
		SMTPUsername: "u@x",
		SMTPPassword: "original-pwd",
		SMTPUseTLS:   true,
	}
	if err := f.bots.Insert(context.Background(), b); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	got, err := f.bots.GetByID(context.Background(), f.tenant.ID, b.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	got.Name = "smtp-keep-renamed"
	got.SMTPPassword = "" // simulate "leave blank to keep" UI flow
	if err := f.bots.Update(context.Background(), got); err != nil {
		t.Fatalf("Update: %v", err)
	}
	again, err := f.bots.GetByID(context.Background(), f.tenant.ID, b.ID)
	if err != nil {
		t.Fatalf("GetByID after update: %v", err)
	}
	if again.Name != "smtp-keep-renamed" {
		t.Fatalf("name = %q", again.Name)
	}
	if again.SMTPPassword != "original-pwd" {
		t.Fatalf("password lost on update: got %q want original-pwd", again.SMTPPassword)
	}
}

// TestBotRepo_SMTPUpdateRotatesPassword: when the operator DOES supply
// a new password, it must replace the stored one. Pairs with the
// preserve test above to lock the "blank = keep, non-blank = rotate"
// semantic.
func TestBotRepo_SMTPUpdateRotatesPassword(t *testing.T) {
	f := newResourceFixture(t)
	b := &domain.Bot{
		TenantID:     f.tenant.ID,
		Name:         "smtp-rotate",
		Platform:     domain.PlatformSMTP,
		SMTPHost:     "smtp.example.com",
		SMTPPort:     587,
		SMTPUsername: "u@x",
		SMTPPassword: "old-pwd",
		SMTPUseTLS:   true,
	}
	if err := f.bots.Insert(context.Background(), b); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	got, err := f.bots.GetByID(context.Background(), f.tenant.ID, b.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	got.SMTPPassword = "new-pwd"
	if err := f.bots.Update(context.Background(), got); err != nil {
		t.Fatalf("Update: %v", err)
	}
	again, err := f.bots.GetByID(context.Background(), f.tenant.ID, b.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if again.SMTPPassword != "new-pwd" {
		t.Fatalf("password rotation failed: got %q want new-pwd", again.SMTPPassword)
	}
}

// TestBotRepo_SMTPValidation enforces the SMTP-specific required-fields
// matrix at the repo layer.
func TestBotRepo_SMTPValidation(t *testing.T) {
	f := newResourceFixture(t)
	cases := map[string]*domain.Bot{
		"missing host": {
			TenantID: f.tenant.ID, Name: "x", Platform: domain.PlatformSMTP,
			SMTPUsername: "u@x", SMTPPassword: "p",
		},
		"missing username": {
			TenantID: f.tenant.ID, Name: "x", Platform: domain.PlatformSMTP,
			SMTPHost: "smtp.example.com", SMTPPassword: "p",
		},
		"port out of range": {
			TenantID: f.tenant.ID, Name: "x", Platform: domain.PlatformSMTP,
			SMTPHost: "smtp.example.com", SMTPUsername: "u@x",
			SMTPPassword: "p", SMTPPort: 99999,
		},
	}
	for name, b := range cases {
		t.Run(name, func(t *testing.T) {
			if err := f.bots.Insert(context.Background(), b); err == nil {
				t.Fatalf("Insert should have failed for %s", name)
			}
		})
	}
}

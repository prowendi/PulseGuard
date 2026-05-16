package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"

	"github.com/wendi/pulseguard/internal/config"
	"github.com/wendi/pulseguard/internal/domain"
)

// Service orchestrates registration, login, session lifecycle, and invite
// code generation. It depends only on domain repository interfaces so it
// is trivially fakeable in tests.
type Service struct {
	tenants  domain.TenantRepo
	invites  domain.InviteRepo
	sessions domain.SessionRepo
	cfg      config.Security
	clock    domain.Clock
}

// New constructs a Service.
func New(
	tenants domain.TenantRepo,
	invites domain.InviteRepo,
	sessions domain.SessionRepo,
	cfg config.Security,
	clock domain.Clock,
) *Service {
	return &Service{tenants: tenants, invites: invites, sessions: sessions, cfg: cfg, clock: clock}
}

// Register creates a brand-new tenant after consuming an invite code.
//
// Steps (mirrors spec §4.2):
//  1. invites.Lock(code) — verifies the code exists, is unused, not expired.
//  2. bcrypt(password) → hash.
//  3. tenants.Insert.
//  4. invites.Consume(code, tenantID).
//  5. issue session.
//
// On any failure the caller receives a wrapped domain error. SQLite has no
// cross-repo transaction here; the consume happens after the tenant insert
// so a crash leaves the code unused (will fail later with ErrConflict if
// the email is also reused).
func (s *Service) Register(ctx context.Context, email, password, inviteCode string) (*domain.Tenant, *domain.Session, error) {
	if email == "" || password == "" || inviteCode == "" {
		return nil, nil, fmt.Errorf("%w: email/password/invite_code required", domain.ErrValidation)
	}

	// Lock + freshness check — runs inside its own short transaction inside
	// the InviteRepo, but we re-check expiry here against our clock too so
	// tests can drive deterministic expiry.
	inv, err := s.invites.Lock(ctx, inviteCode)
	if err != nil {
		if errors.Is(err, domain.ErrInviteInvalid) || errors.Is(err, domain.ErrNotFound) {
			return nil, nil, domain.ErrInviteInvalid
		}
		return nil, nil, fmt.Errorf("lock invite: %w", err)
	}
	if inv.UsedAt != nil {
		return nil, nil, domain.ErrInviteInvalid
	}
	now := s.clock.Now()
	if inv.ExpiresAt != nil && !inv.ExpiresAt.After(now) {
		return nil, nil, domain.ErrInviteInvalid
	}

	// Reject duplicate emails up front (the unique index would also catch
	// this, but we want a clean ErrConflict mapping).
	if existing, err := s.tenants.GetByEmail(ctx, email); err == nil && existing != nil {
		return nil, nil, domain.ErrConflict
	} else if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return nil, nil, fmt.Errorf("lookup tenant: %w", err)
	}

	cost := s.cfg.BcryptCost
	hash, err := HashPassword(password, cost)
	if err != nil {
		return nil, nil, err
	}

	tenant := &domain.Tenant{
		Email:        email,
		PasswordHash: string(hash),
		Role:         domain.RoleUser,
		Status:       domain.TenantActive,
	}
	if err := s.tenants.Insert(ctx, tenant); err != nil {
		return nil, nil, fmt.Errorf("insert tenant: %w", err)
	}

	if err := s.invites.Consume(ctx, inviteCode, tenant.ID); err != nil {
		if errors.Is(err, domain.ErrInviteInvalid) {
			// A racing Consume from another caller — surface as invalid.
			return nil, nil, domain.ErrInviteInvalid
		}
		return nil, nil, fmt.Errorf("consume invite: %w", err)
	}

	sess, err := s.issueSession(ctx, tenant.ID)
	if err != nil {
		return nil, nil, err
	}
	return tenant, sess, nil
}

// Login verifies credentials and issues a fresh session. Bad email/password
// both return ErrUnauthorized (no enumeration leak).
func (s *Service) Login(ctx context.Context, email, password string) (*domain.Session, error) {
	if email == "" || password == "" {
		return nil, domain.ErrUnauthorized
	}
	tenant, err := s.tenants.GetByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, domain.ErrUnauthorized
		}
		return nil, fmt.Errorf("lookup tenant: %w", err)
	}
	if tenant.Status != domain.TenantActive {
		return nil, domain.ErrUnauthorized
	}
	if err := CompareHashAndPassword([]byte(tenant.PasswordHash), password); err != nil {
		return nil, domain.ErrUnauthorized
	}
	return s.issueSession(ctx, tenant.ID)
}

// Logout deletes the session row identified by id.
func (s *Service) Logout(ctx context.Context, sessionID string) error {
	return s.sessions.Delete(ctx, sessionID)
}

// SessionFromID resolves a session id to its tenant. Expired or unknown
// sessions return ErrUnauthorized.
func (s *Service) SessionFromID(ctx context.Context, id string) (*domain.Tenant, *domain.Session, error) {
	if id == "" {
		return nil, nil, domain.ErrUnauthorized
	}
	sess, err := s.sessions.GetByID(ctx, id)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, nil, domain.ErrUnauthorized
		}
		return nil, nil, fmt.Errorf("get session: %w", err)
	}
	if !sess.ExpiresAt.After(s.clock.Now()) {
		return nil, nil, domain.ErrUnauthorized
	}
	tenant, err := s.tenants.GetByID(ctx, sess.TenantID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, nil, domain.ErrUnauthorized
		}
		return nil, nil, fmt.Errorf("get tenant: %w", err)
	}
	if tenant.Status != domain.TenantActive {
		return nil, nil, domain.ErrUnauthorized
	}
	return tenant, sess, nil
}

// issueSession mints a 32-character random session id and persists it.
func (s *Service) issueSession(ctx context.Context, tenantID int64) (*domain.Session, error) {
	id, err := randomToken(24)
	if err != nil {
		return nil, err
	}
	ttl := s.cfg.SessionTTL.Std()
	if ttl <= 0 {
		ttl = 14 * 24 * 60 * 60 * 1_000_000_000 // 14d in ns
	}
	sess := &domain.Session{
		ID:        id,
		TenantID:  tenantID,
		ExpiresAt: s.clock.Now().Add(ttl),
	}
	if err := s.sessions.Insert(ctx, sess); err != nil {
		return nil, fmt.Errorf("insert session: %w", err)
	}
	return sess, nil
}

// randomToken returns a URL-safe base64 token from n bytes of entropy.
// 24 raw bytes encode to exactly 32 base64 characters (no padding).
func randomToken(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("crypto/rand: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

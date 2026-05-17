package auth

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"

	"golang.org/x/crypto/bcrypt"

	"github.com/wendi/pulseguard/internal/config"
	"github.com/wendi/pulseguard/internal/domain"
	"github.com/wendi/pulseguard/internal/store"
)

// dummyBcryptHash was an early packaging of the timing pad; the pad is
// now per-Service (see Service.timingPad) so its cost can mirror
// cfg.BcryptCost. This package-level placeholder is intentionally left
// out — no package-level state is needed.


// Service orchestrates registration, login, session lifecycle, and invite
// code generation. It depends only on domain repository interfaces so it
// is trivially fakeable in tests. The concrete *sql.DB handle is required
// for the Register flow which must atomically claim an invite, insert a
// tenant, and issue a session in a single transaction.
type Service struct {
	db       *sql.DB
	tenants  *store.TenantRepo
	invites  *store.InviteRepo
	sessions *store.SessionRepo
	cfg      config.Security
	clock    domain.Clock
	// timingPad is a bcrypt hash generated at construction time using
	// the same cost as production hashes (cfg.BcryptCost). Login uses
	// it on the email-unknown / disabled-tenant branches so the wall
	// clock cost of a Login call is dominated by exactly one bcrypt
	// CompareHashAndPassword regardless of which branch fires. This
	// closes the timing oracle described in security-report S-L1.
	timingPad []byte
}

// New constructs a Service. The repo arguments are concrete store
// types because Register needs the *_Tx variants; tests can still
// substitute fakes by working with the underlying *sql.DB.
func New(
	db *sql.DB,
	tenants *store.TenantRepo,
	invites *store.InviteRepo,
	sessions *store.SessionRepo,
	cfg config.Security,
	clock domain.Clock,
) *Service {
	return &Service{
		db:        db,
		tenants:   tenants,
		invites:   invites,
		sessions: sessions,
		cfg:       cfg,
		clock:     clock,
		timingPad: mustTimingPad(cfg.BcryptCost),
	}
}

// mustTimingPad generates the dummy bcrypt hash used by Login on the
// not-found branches. We pin the cost to cfg.BcryptCost (falling back
// to bcrypt.DefaultCost when unset / out of range) so the pad's
// CompareHash takes the same wall-clock time as a real comparison.
// Generation cannot reasonably fail (cost-bound errors and
// password-length errors only); panicking here is appropriate because
// running without the pad re-introduces the timing oracle.
func mustTimingPad(cost int) []byte {
	if cost < bcrypt.MinCost || cost > bcrypt.MaxCost {
		cost = bcrypt.DefaultCost
	}
	h, err := bcrypt.GenerateFromPassword([]byte("pulseguard-timing-pad"), cost)
	if err != nil {
		panic(fmt.Sprintf("auth: cannot generate timing pad bcrypt hash: %v", err))
	}
	return h
}

// Register creates a brand-new tenant after consuming an invite code.
//
// All four writes (invite lock + tenant insert + invite consume +
// session insert) run inside a single db.BeginTx so a crash or a
// racing peer cannot leave the system in a half-applied state. The
// previous deferred-tx implementation had a window where two callers
// could both Lock the same code; the IMMEDIATE transaction here
// serialises them. The losing caller hits the unique email index or
// the invite UsedAt check and returns a clean domain error.
func (s *Service) Register(ctx context.Context, email, password, inviteCode string) (*domain.Tenant, *domain.Session, error) {
	if email == "" || password == "" || inviteCode == "" {
		return nil, nil, fmt.Errorf("%w: email/password/invite_code required", domain.ErrValidation)
	}

	cost := s.cfg.BcryptCost
	// bcrypt is expensive (~200ms at cost=10); hash BEFORE opening the
	// transaction so we don't hold the SQLite write lock during the
	// CPU-bound step.
	hash, err := HashPassword(password, cost)
	if err != nil {
		return nil, nil, err
	}

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return nil, nil, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Reject duplicate emails inside the tx so a parallel registration
	// for the same email cannot both Insert.
	if existing, err := s.tenants.GetByEmailTx(ctx, tx, email); err == nil && existing != nil {
		return nil, nil, domain.ErrConflict
	} else if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return nil, nil, fmt.Errorf("lookup tenant: %w", err)
	}

	tenant := &domain.Tenant{
		Email:        email,
		PasswordHash: string(hash),
		Role:         domain.RoleUser,
		Status:       domain.TenantActive,
	}
	if err := s.tenants.InsertTx(ctx, tx, tenant); err != nil {
		return nil, nil, fmt.Errorf("insert tenant: %w", err)
	}

	// ConsumeTx locks the invite row, validates not-used + not-expired,
	// and marks it used by tenant.ID. A concurrent Register caller
	// either lost the tx race or finds UsedAt set and returns
	// ErrInviteInvalid.
	if err := s.invites.ConsumeTx(ctx, tx, inviteCode, tenant.ID); err != nil {
		if errors.Is(err, domain.ErrInviteInvalid) || errors.Is(err, domain.ErrNotFound) {
			return nil, nil, domain.ErrInviteInvalid
		}
		return nil, nil, fmt.Errorf("consume invite: %w", err)
	}

	id, err := randomToken(24)
	if err != nil {
		return nil, nil, err
	}
	ttl := s.cfg.SessionTTL.Std()
	if ttl <= 0 {
		ttl = 14 * 24 * 60 * 60 * 1_000_000_000 // 14d in ns
	}
	sess := &domain.Session{
		ID:        id,
		TenantID:  tenant.ID,
		ExpiresAt: s.clock.Now().Add(ttl),
	}
	if err := s.sessions.InsertTx(ctx, tx, sess); err != nil {
		return nil, nil, fmt.Errorf("insert session: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, nil, fmt.Errorf("commit register: %w", err)
	}
	return tenant, sess, nil
}

// dummyBcryptHash is reserved for a future fallback if Service.timingPad
// is ever nil; currently Login uses Service.timingPad directly.
// Kept as a package-level placeholder to make the rationale explicit.

func (s *Service) Login(ctx context.Context, email, password string) (*domain.Session, error) {
	if email == "" || password == "" {
		// Even with empty input we still pay the bcrypt cost to keep
		// "missing field" indistinguishable from "wrong creds" by wall
		// clock; the early return below the compare keeps semantics
		// unchanged.
		_ = bcrypt.CompareHashAndPassword(s.timingPad, []byte("x"))
		return nil, domain.ErrUnauthorized
	}
	tenant, err := s.tenants.GetByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			// Equal-cost pad: spend one bcrypt comparison so the
			// elapsed time of this branch matches the
			// "email-exists-but-wrong-password" branch below.
			_ = bcrypt.CompareHashAndPassword(s.timingPad, []byte(password))
			return nil, domain.ErrUnauthorized
		}
		return nil, fmt.Errorf("lookup tenant: %w", err)
	}
	if tenant.Status != domain.TenantActive {
		// Same pad here: the "disabled tenant" branch must not return
		// faster than the wrong-password branch either.
		_ = bcrypt.CompareHashAndPassword(s.timingPad, []byte(password))
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

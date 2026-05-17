package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/prowendi/PulseGuard/internal/domain"
)

// SessionRepo persists authentication sessions in the sessions table.
type SessionRepo struct {
	db    *sql.DB
	clock domain.Clock
}

// NewSessionRepo binds a SessionRepo to the given handle.
func NewSessionRepo(db *sql.DB, clock domain.Clock) *SessionRepo {
	return &SessionRepo{db: db, clock: clock}
}

// Insert writes a new session row. The session ID must be pre-generated
// by the caller (auth service controls entropy).
func (r *SessionRepo) Insert(ctx context.Context, s *domain.Session) error {
	return r.InsertTx(ctx, r.db, s)
}

// InsertTx is the explicit-transaction variant of Insert. Lets
// auth.Register share a single *sql.Tx with the tenant + invite
// consume so all three writes commit atomically.
func (r *SessionRepo) InsertTx(ctx context.Context, tx txExec, s *domain.Session) error {
	if s == nil || s.ID == "" {
		return fmt.Errorf("%w: session id is empty", domain.ErrValidation)
	}
	if s.TenantID == 0 {
		return fmt.Errorf("%w: session tenant_id is zero", domain.ErrValidation)
	}
	if s.ExpiresAt.IsZero() {
		return fmt.Errorf("%w: session expires_at is zero", domain.ErrValidation)
	}
	createdMs := nowMs(r.clock)
	s.CreatedAt = toTime(createdMs)
	_, err := tx.ExecContext(ctx, `
		INSERT INTO sessions (id, tenant_id, expires_at, created_at)
		VALUES (?, ?, ?, ?)`,
		s.ID, s.TenantID, s.ExpiresAt.UnixMilli(), createdMs)
	if err != nil {
		return fmt.Errorf("insert session: %w", err)
	}
	return nil
}

// GetByID looks up a session by id. Returns ErrNotFound if missing.
// Expired sessions are NOT auto-filtered; the auth layer compares
// ExpiresAt against the clock itself.
func (r *SessionRepo) GetByID(ctx context.Context, id string) (*domain.Session, error) {
	s := &domain.Session{}
	var expiresMs, createdMs int64
	err := r.db.QueryRowContext(ctx, `
		SELECT id, tenant_id, expires_at, created_at FROM sessions WHERE id = ?`, id,
	).Scan(&s.ID, &s.TenantID, &expiresMs, &createdMs)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan session: %w", err)
	}
	s.ExpiresAt = toTime(expiresMs)
	s.CreatedAt = toTime(createdMs)
	return s, nil
}

// Delete removes a session row. Missing rows are not an error.
func (r *SessionRepo) Delete(ctx context.Context, id string) error {
	if _, err := r.db.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, id); err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	return nil
}

// PurgeExpired deletes every session whose expires_at is strictly less
// than the supplied time and returns the number of rows removed.
func (r *SessionRepo) PurgeExpired(ctx context.Context, now time.Time) (int64, error) {
	res, err := r.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at < ?`, now.UnixMilli())
	if err != nil {
		return 0, fmt.Errorf("purge sessions: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("rows affected: %w", err)
	}
	return n, nil
}

// Ensure interface compliance at compile time.
var _ domain.SessionRepo = (*SessionRepo)(nil)

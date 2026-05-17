package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/prowendi/PulseGuard/internal/domain"
)

// InviteRepo persists invite codes. Lock + Consume are designed to run
// inside an outer transaction; consumers should obtain a tx-bound repo via
// (*InviteRepo).WithTx and call Lock then Consume in the same transaction
// to avoid two-tenant races on a single code.
//
// SQLite has no row-level lock, so Lock uses BEGIN IMMEDIATE (already
// acquired by WithTx) to obtain the database write lock for the duration
// of the transaction, which serialises all writers.
type InviteRepo struct {
	db    *sql.DB
	clock domain.Clock
}

// NewInviteRepo binds the repo to a DB handle and clock.
func NewInviteRepo(db *sql.DB, clock domain.Clock) *InviteRepo {
	return &InviteRepo{db: db, clock: clock}
}

// Insert creates a new invite_codes row. CreatedAt is taken from the clock.
func (r *InviteRepo) Insert(ctx context.Context, code *domain.InviteCode) error {
	if code == nil {
		return errors.New("invite is nil")
	}
	if code.Code == "" {
		return fmt.Errorf("%w: invite code is empty", domain.ErrValidation)
	}
	if code.CreatedBy == 0 {
		return fmt.Errorf("%w: invite created_by is zero", domain.ErrValidation)
	}
	now := nowMs(r.clock)
	code.CreatedAt = toTime(now)
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO invite_codes (code, created_by, used_by, expires_at, used_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		code.Code, code.CreatedBy, code.UsedBy,
		fromTimePtr(code.ExpiresAt), fromTimePtr(code.UsedAt), now)
	if err != nil {
		return fmt.Errorf("insert invite: %w", err)
	}
	return nil
}

// Lock reads an invite for update inside a write transaction. Use
// LockTx when you already have a *sql.Tx; the plain Lock opens its own
// IMMEDIATE transaction, returns the row, then leaves the transaction
// open via a deferred rollback — therefore tests that intend to mutate
// should prefer WithTx + Consume.
func (r *InviteRepo) Lock(ctx context.Context, code string) (*domain.InviteCode, error) {
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	inv, err := lockTx(ctx, tx, code)
	if err != nil {
		return nil, err
	}
	return inv, nil
}

// Consume marks the invite as used by tenantID. Returns ErrInviteInvalid
// when the code is missing, already consumed, or expired.
func (r *InviteRepo) Consume(ctx context.Context, code string, tenantID int64) error {
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := r.ConsumeTx(ctx, tx, code, tenantID); err != nil {
		return err
	}
	return tx.Commit()
}

// ConsumeTx is the explicit-transaction variant of Consume. The caller
// owns the *sql.Tx; on success the invite row is marked used inside
// that tx without an inner commit. Used by auth.Register to atomically
// claim the invite alongside tenant + session inserts.
func (r *InviteRepo) ConsumeTx(ctx context.Context, tx *sql.Tx, code string, tenantID int64) error {
	inv, err := lockTx(ctx, tx, code)
	if err != nil {
		return err
	}
	if inv.UsedAt != nil {
		return domain.ErrInviteInvalid
	}
	now := r.clock.Now()
	if inv.ExpiresAt != nil && !inv.ExpiresAt.After(now) {
		return domain.ErrInviteInvalid
	}
	nowMs := now.UnixMilli()
	if _, err := tx.ExecContext(ctx, `
		UPDATE invite_codes SET used_by = ?, used_at = ? WHERE code = ?`,
		tenantID, nowMs, code); err != nil {
		return fmt.Errorf("update invite: %w", err)
	}
	return nil
}

// ListByCreator returns invites issued by a given admin tenant.
func (r *InviteRepo) ListByCreator(ctx context.Context, adminID int64) ([]*domain.InviteCode, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT code, created_by, used_by, expires_at, used_at, created_at
		  FROM invite_codes WHERE created_by = ? ORDER BY created_at DESC`, adminID)
	if err != nil {
		return nil, fmt.Errorf("list invites: %w", err)
	}
	defer rows.Close()
	var out []*domain.InviteCode
	for rows.Next() {
		inv, err := scanInvite(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, inv)
	}
	return out, rows.Err()
}

// CountByCreatorSince counts invites created by adminID with
// created_at >= since (the boundary uses milliseconds, matching the
// other timestamp columns). Callers should round `since` to the start
// of the relevant window (e.g. midnight UTC for the daily cap).
func (r *InviteRepo) CountByCreatorSince(ctx context.Context, adminID int64, since time.Time) (int, error) {
	if adminID <= 0 {
		return 0, fmt.Errorf("%w: adminID must be > 0", domain.ErrValidation)
	}
	var n int
	err := r.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM invite_codes
		 WHERE created_by = ? AND created_at >= ?`,
		adminID, since.UnixMilli()).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count invites since: %w", err)
	}
	return n, nil
}

// Delete removes an unused invite code owned by adminID. Returns
// ErrNotFound when no matching row exists for the (code, created_by)
// pair, and ErrInviteInvalid when the code has already been consumed.
// The check + delete run in a single IMMEDIATE transaction so a
// concurrent Consume cannot squeeze in between the lookup and the
// delete.
func (r *InviteRepo) Delete(ctx context.Context, code string, adminID int64) error {
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var usedAt sql.NullInt64
	err = tx.QueryRowContext(ctx, `
		SELECT used_at FROM invite_codes WHERE code = ? AND created_by = ?`,
		code, adminID).Scan(&usedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("lookup invite: %w", err)
	}
	if usedAt.Valid {
		return domain.ErrInviteInvalid
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM invite_codes WHERE code = ? AND created_by = ?`,
		code, adminID); err != nil {
		return fmt.Errorf("delete invite: %w", err)
	}
	return tx.Commit()
}

func lockTx(ctx context.Context, tx *sql.Tx, code string) (*domain.InviteCode, error) {
	row := tx.QueryRowContext(ctx, `
		SELECT code, created_by, used_by, expires_at, used_at, created_at
		  FROM invite_codes WHERE code = ?`, code)
	inv, err := scanInvite(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrInviteInvalid
	}
	return inv, err
}

// scanInvite materialises a row whose columns are
// (code, created_by, used_by, expires_at, used_at, created_at).
func scanInvite(s interface {
	Scan(dest ...any) error
}) (*domain.InviteCode, error) {
	inv := &domain.InviteCode{}
	var usedBy sql.NullInt64
	var expiresAt, usedAt sql.NullInt64
	var createdAt int64
	if err := s.Scan(&inv.Code, &inv.CreatedBy, &usedBy, &expiresAt, &usedAt, &createdAt); err != nil {
		return nil, err
	}
	if usedBy.Valid {
		v := usedBy.Int64
		inv.UsedBy = &v
	}
	if expiresAt.Valid {
		t := toTime(expiresAt.Int64)
		inv.ExpiresAt = &t
	}
	if usedAt.Valid {
		t := toTime(usedAt.Int64)
		inv.UsedAt = &t
	}
	inv.CreatedAt = toTime(createdAt)
	return inv, nil
}

// Ensure interface compliance at compile time.
var _ domain.InviteRepo = (*InviteRepo)(nil)

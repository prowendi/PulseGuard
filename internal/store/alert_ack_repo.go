package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/wendi/pulseguard/internal/domain"
)

// AlertAckRepo persists per-tenant alert acknowledgements (V6-3).
//
// Insert is intentionally idempotent at the friendly-error level: when
// the (tenant_id, fingerprint) UNIQUE fires we return ErrAlreadyAcked
// so the listener can reply "已记录" without surfacing a raw SQLite
// constraint message. Hard SQL errors still propagate.
type AlertAckRepo struct {
	db    *sql.DB
	clock domain.Clock
}

// NewAlertAckRepo binds the repo to a DB handle and clock.
func NewAlertAckRepo(db *sql.DB, clock domain.Clock) *AlertAckRepo {
	return &AlertAckRepo{db: db, clock: clock}
}

// ErrAlreadyAcked is the sentinel returned by Insert when an ack row
// already exists for the (tenant_id, fingerprint) pair. The Telegram
// listener maps this to "已记录" so the user knows the fingerprint is
// already in the audit trail without leaking SQL internals.
var ErrAlreadyAcked = errors.New("alert_ack: already acked")

// Insert writes a new ack row. Returns ErrAlreadyAcked when the
// (tenant_id, fingerprint) UNIQUE collides; the listener treats that
// as a successful no-op with a slightly different user reply.
func (r *AlertAckRepo) Insert(ctx context.Context, a *domain.AlertAck) error {
	if a == nil {
		return errors.New("alert_ack: nil")
	}
	if a.TenantID == 0 {
		return fmt.Errorf("%w: tenant_id is zero", domain.ErrValidation)
	}
	a.Fingerprint = strings.TrimSpace(a.Fingerprint)
	if a.Fingerprint == "" {
		return fmt.Errorf("%w: fingerprint is empty", domain.ErrValidation)
	}
	if a.BotID == 0 {
		return fmt.Errorf("%w: bot_id is zero", domain.ErrValidation)
	}
	if strings.TrimSpace(a.ChatID) == "" {
		return fmt.Errorf("%w: chat_id is empty", domain.ErrValidation)
	}
	now := nowMs(r.clock)
	res, err := r.db.ExecContext(ctx, `
		INSERT INTO alert_acks (tenant_id, fingerprint, acked_by, acked_at, bot_id, chat_id)
		VALUES (?, ?, ?, ?, ?, ?)`,
		a.TenantID, a.Fingerprint, a.AckedBy, now, a.BotID, a.ChatID)
	if err != nil {
		if isUniqueConstraintErr(err) {
			return ErrAlreadyAcked
		}
		return fmt.Errorf("insert alert_ack: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return fmt.Errorf("last insert id: %w", err)
	}
	a.ID = id
	a.AckedAt = toTime(now)
	return nil
}

// GetByFingerprint fetches the ack row for a (tenant, fingerprint)
// pair. Returns ErrNotFound when no row matches.
func (r *AlertAckRepo) GetByFingerprint(ctx context.Context, tenantID int64, fingerprint string) (*domain.AlertAck, error) {
	row := r.db.QueryRowContext(ctx, `
		SELECT id, tenant_id, fingerprint, acked_by, acked_at, bot_id, chat_id
		  FROM alert_acks
		 WHERE tenant_id = ? AND fingerprint = ?`, tenantID, fingerprint)
	return scanAlertAck(row)
}

// ListByTenant returns every ack row for a tenant ordered by
// ack-time descending (newest first) — that order matches what the
// future UI will paginate.
func (r *AlertAckRepo) ListByTenant(ctx context.Context, tenantID int64) ([]*domain.AlertAck, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, tenant_id, fingerprint, acked_by, acked_at, bot_id, chat_id
		  FROM alert_acks
		 WHERE tenant_id = ?
		 ORDER BY acked_at DESC, id DESC`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list alert_acks: %w", err)
	}
	defer rows.Close()
	var out []*domain.AlertAck
	for rows.Next() {
		a, err := scanAlertAck(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func scanAlertAck(s interface {
	Scan(dest ...any) error
}) (*domain.AlertAck, error) {
	a := &domain.AlertAck{}
	var ackedMs int64
	err := s.Scan(&a.ID, &a.TenantID, &a.Fingerprint, &a.AckedBy, &ackedMs, &a.BotID, &a.ChatID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan alert_ack: %w", err)
	}
	a.AckedAt = toTime(ackedMs)
	return a, nil
}

// isUniqueConstraintErr detects SQLite UNIQUE violations by inspecting
// the error message. modernc.org/sqlite (the driver this project uses)
// returns errors whose Error() text always starts with "constraint
// failed: UNIQUE …" for this category, so a substring match is robust
// across versions. Kept in this file (rather than util.go) so the
// search stays narrow and easy to grep.
func isUniqueConstraintErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") ||
		strings.Contains(msg, "constraint failed: UNIQUE")
}

// Ensure interface compliance at compile time.
var _ domain.AlertAckRepo = (*AlertAckRepo)(nil)

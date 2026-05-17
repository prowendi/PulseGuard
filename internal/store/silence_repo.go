package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/prowendi/PulseGuard/internal/domain"
)

// SilenceRepo persists per-tenant alert silences (V7-3). The repo is
// deliberately small: it owns CRUD against the silences table and
// implements the prefix-match query the push worker hits before every
// outbound send.
type SilenceRepo struct {
	db    *sql.DB
	clock domain.Clock
}

// NewSilenceRepo binds the repo to a DB handle + clock. clock is used
// only for stamping created_at; ExpiresAt is supplied by the caller
// (which already knows the duration the operator typed into /silence).
func NewSilenceRepo(db *sql.DB, clock domain.Clock) *SilenceRepo {
	return &SilenceRepo{db: db, clock: clock}
}

// Insert writes a new silence row. The pattern is trimmed and rejected
// when empty so a stray "/silence  1h" cannot mute every alert by
// matching the empty-string prefix.
func (r *SilenceRepo) Insert(ctx context.Context, s *domain.Silence) error {
	if s == nil {
		return errors.New("silence: nil")
	}
	if s.TenantID == 0 {
		return fmt.Errorf("%w: tenant_id is zero", domain.ErrValidation)
	}
	s.Pattern = strings.TrimSpace(s.Pattern)
	if s.Pattern == "" {
		return fmt.Errorf("%w: pattern is empty", domain.ErrValidation)
	}
	s.CreatedBy = strings.TrimSpace(s.CreatedBy)
	if s.CreatedBy == "" {
		return fmt.Errorf("%w: created_by is empty", domain.ErrValidation)
	}
	if s.ExpiresAt.IsZero() {
		return fmt.Errorf("%w: expires_at is zero", domain.ErrValidation)
	}
	now := nowMs(r.clock)
	expMs := s.ExpiresAt.UnixMilli()
	res, err := r.db.ExecContext(ctx, `
		INSERT INTO silences (tenant_id, pattern, created_by, expires_at, created_at)
		VALUES (?, ?, ?, ?, ?)`,
		s.TenantID, s.Pattern, s.CreatedBy, expMs, now)
	if err != nil {
		return fmt.Errorf("insert silence: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return fmt.Errorf("last insert id: %w", err)
	}
	s.ID = id
	s.CreatedAt = toTime(now)
	return nil
}

// ListActive returns every silence row that has not yet expired at
// `now`, ordered by ExpiresAt ASC so the /silence_list output reads as
// "what will lift first". A silence whose expires_at == now is still
// active (inclusive boundary), matching Silence.Active.
func (r *SilenceRepo) ListActive(ctx context.Context, tenantID int64, now time.Time) ([]*domain.Silence, error) {
	nowMs := now.UnixMilli()
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, tenant_id, pattern, created_by, expires_at, created_at
		  FROM silences
		 WHERE tenant_id = ? AND expires_at >= ?
		 ORDER BY expires_at ASC, id ASC`, tenantID, nowMs)
	if err != nil {
		return nil, fmt.Errorf("list active silences: %w", err)
	}
	defer rows.Close()
	var out []*domain.Silence
	for rows.Next() {
		s, err := scanSilence(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Delete removes the silence row identified by (tenantID, id). Returns
// ErrNotFound when no row matches so the caller (Telegram listener) can
// reply with "未找到" rather than masking a typo as success.
func (r *SilenceRepo) Delete(ctx context.Context, tenantID, id int64) error {
	if tenantID == 0 || id == 0 {
		return fmt.Errorf("%w: tenant_id and id required", domain.ErrValidation)
	}
	res, err := r.db.ExecContext(ctx,
		`DELETE FROM silences WHERE tenant_id = ? AND id = ?`, tenantID, id)
	if err != nil {
		return fmt.Errorf("delete silence: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// DeleteByPattern removes every silence row whose pattern is an exact
// match of `pattern` (after the same TrimSpace normalization Insert
// applies). Returns the number of deleted rows. Pattern empty
// short-circuits to 0 — the listener will reply with the usage hint
// instead of nuking every silence row in the tenant.
func (r *SilenceRepo) DeleteByPattern(ctx context.Context, tenantID int64, pattern string) (int64, error) {
	if tenantID == 0 {
		return 0, fmt.Errorf("%w: tenant_id is zero", domain.ErrValidation)
	}
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return 0, nil
	}
	res, err := r.db.ExecContext(ctx,
		`DELETE FROM silences WHERE tenant_id = ? AND pattern = ?`, tenantID, pattern)
	if err != nil {
		return 0, fmt.Errorf("delete silence by pattern: %w", err)
	}
	return res.RowsAffected()
}

// Match is the hot-path call: it returns true when at least one active
// silence row for the tenant has a Pattern that is a prefix of the
// supplied fingerprint. Empty fingerprint short-circuits to false so
// the V7-2 "no _fingerprint" path is never accidentally silenced.
func (r *SilenceRepo) Match(ctx context.Context, tenantID int64, fingerprint string, now time.Time) (bool, error) {
	if tenantID == 0 {
		return false, nil
	}
	fp := strings.TrimSpace(fingerprint)
	if fp == "" {
		return false, nil
	}
	// SQLite's instr() returns 1 when needle is found at position 1 of
	// haystack (i.e. exact prefix). We push the prefix check into SQL
	// so the worker round-trips one row, not the entire silence list.
	row := r.db.QueryRowContext(ctx, `
		SELECT 1
		  FROM silences
		 WHERE tenant_id = ?
		   AND expires_at >= ?
		   AND instr(?, pattern) = 1
		 LIMIT 1`, tenantID, now.UnixMilli(), fp)
	var v int
	err := row.Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("match silence: %w", err)
	}
	return v == 1, nil
}

func scanSilence(s interface {
	Scan(dest ...any) error
}) (*domain.Silence, error) {
	out := &domain.Silence{}
	var expMs, createdMs int64
	err := s.Scan(&out.ID, &out.TenantID, &out.Pattern, &out.CreatedBy, &expMs, &createdMs)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan silence: %w", err)
	}
	out.ExpiresAt = toTime(expMs)
	out.CreatedAt = toTime(createdMs)
	return out, nil
}

// Ensure interface compliance at compile time.
var _ domain.SilenceRepo = (*SilenceRepo)(nil)

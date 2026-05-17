package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/prowendi/PulseGuard/internal/domain"
)

// DedupRepo implements the per-channel fingerprint window. The dedup_keys
// row is keyed by (channel_id, fingerprint) and carries an expires_at.
// SeenOrInsert is the single source of truth: it returns true iff a
// live row already existed.
type DedupRepo struct {
	db *sql.DB
}

// NewDedupRepo binds the repo to a DB handle.
func NewDedupRepo(db *sql.DB) *DedupRepo {
	return &DedupRepo{db: db}
}

// SeenOrInsert atomically checks whether channelID+fp is already live;
// if so, it bumps last_seen_at and hit_count and returns true. Otherwise
// it inserts a fresh row (or revives an expired one) and returns false.
// windowSec=0 disables dedup (returns false without persisting).
//
// We use a single UPSERT with a RETURNING clause whose value depends on
// whether the previous row had already expired at now. This collapses
// the prior SELECT-then-INSERT / SELECT-then-UPDATE flow into one SQL
// statement, eliminating the deferred-tx race window where two callers
// could both see "no rows" and both insert. SQLite serialises the
// UPSERT against (channel_id, fingerprint) via the unique constraint,
// so concurrent callers cannot double-count or both see "first sighting".
func (r *DedupRepo) SeenOrInsert(ctx context.Context, channelID int64, fp string, now time.Time, windowSec int) (bool, error) {
	if windowSec <= 0 {
		return false, nil
	}
	if channelID == 0 || fp == "" {
		return false, fmt.Errorf("%w: dedup requires channel_id and fingerprint", domain.ErrValidation)
	}
	nowMs := now.UnixMilli()
	expiresAt := now.Add(time.Duration(windowSec) * time.Second).UnixMilli()

	// The CASE WHEN expression returns 0 when the row is a brand-new
	// insertion OR an expired row being refreshed (treated as a first
	// sighting); 1 when an existing live row was bumped. RETURNING gives
	// us this value without an extra round trip.
	//
	// On conflict, we compare the old expires_at (accessible via the
	// dedup_keys alias) against nowMs to decide whether this is a refresh
	// (was expired) or a true hit (still live).
	const q = `
		INSERT INTO dedup_keys
		  (channel_id, fingerprint, first_seen_at, last_seen_at, hit_count, expires_at)
		VALUES (?, ?, ?, ?, 1, ?)
		ON CONFLICT(channel_id, fingerprint) DO UPDATE SET
		  first_seen_at = CASE WHEN dedup_keys.expires_at <= excluded.last_seen_at
		                       THEN excluded.last_seen_at
		                       ELSE dedup_keys.first_seen_at END,
		  last_seen_at  = excluded.last_seen_at,
		  hit_count     = CASE WHEN dedup_keys.expires_at <= excluded.last_seen_at
		                       THEN 1
		                       ELSE dedup_keys.hit_count + 1 END,
		  expires_at    = CASE WHEN dedup_keys.expires_at <= excluded.last_seen_at
		                       THEN excluded.expires_at
		                       ELSE dedup_keys.expires_at END
		RETURNING hit_count`

	var hitCount int
	err := r.db.QueryRowContext(ctx, q,
		channelID, fp, nowMs, nowMs, expiresAt,
	).Scan(&hitCount)
	if err != nil {
		return false, fmt.Errorf("upsert dedup: %w", err)
	}
	// hit_count == 1 means either fresh insert OR expired row refreshed;
	// in both cases this is a "first sighting" for the new window.
	return hitCount > 1, nil
}

// PurgeExpired drops every dedup_keys row whose expires_at <= now.
func (r *DedupRepo) PurgeExpired(ctx context.Context, now time.Time) (int64, error) {
	res, err := r.db.ExecContext(ctx,
		`DELETE FROM dedup_keys WHERE expires_at <= ?`, now.UnixMilli())
	if err != nil {
		return 0, fmt.Errorf("purge dedup: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("rows affected: %w", err)
	}
	return n, nil
}

// Ensure interface compliance at compile time.
var _ domain.DedupRepo = (*DedupRepo)(nil)

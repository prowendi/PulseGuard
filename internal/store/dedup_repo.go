package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/wendi/pulseguard/internal/domain"
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
// it inserts a fresh row and returns false. windowSec=0 disables dedup
// (returns false without persisting).
//
// We use UPSERT (ON CONFLICT) to make this race-free in SQLite.
func (r *DedupRepo) SeenOrInsert(ctx context.Context, channelID int64, fp string, now time.Time, windowSec int) (bool, error) {
	if windowSec <= 0 {
		return false, nil
	}
	if channelID == 0 || fp == "" {
		return false, fmt.Errorf("%w: dedup requires channel_id and fingerprint", domain.ErrValidation)
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var existingExpires int64
	var existingHits int
	err = tx.QueryRowContext(ctx,
		`SELECT expires_at, hit_count FROM dedup_keys WHERE channel_id = ? AND fingerprint = ?`,
		channelID, fp,
	).Scan(&existingExpires, &existingHits)
	nowMs := now.UnixMilli()
	expiresAt := now.Add(time.Duration(windowSec) * time.Second).UnixMilli()

	if errors.Is(err, sql.ErrNoRows) {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO dedup_keys (channel_id, fingerprint, first_seen_at,
			                       last_seen_at, hit_count, expires_at)
			VALUES (?, ?, ?, ?, 1, ?)`,
			channelID, fp, nowMs, nowMs, expiresAt); err != nil {
			return false, fmt.Errorf("insert dedup: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return false, fmt.Errorf("commit dedup: %w", err)
		}
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("select dedup: %w", err)
	}

	// Row exists; classify as live or expired.
	if existingExpires <= nowMs {
		// Expired — refresh window and treat as first sighting.
		if _, err := tx.ExecContext(ctx, `
			UPDATE dedup_keys
			   SET first_seen_at = ?, last_seen_at = ?, hit_count = 1, expires_at = ?
			 WHERE channel_id = ? AND fingerprint = ?`,
			nowMs, nowMs, expiresAt, channelID, fp); err != nil {
			return false, fmt.Errorf("refresh dedup: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return false, fmt.Errorf("commit dedup: %w", err)
		}
		return false, nil
	}

	// Live — bump counters; report already-seen.
	if _, err := tx.ExecContext(ctx, `
		UPDATE dedup_keys
		   SET last_seen_at = ?, hit_count = hit_count + 1
		 WHERE channel_id = ? AND fingerprint = ?`,
		nowMs, channelID, fp); err != nil {
		return false, fmt.Errorf("bump dedup: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit dedup: %w", err)
	}
	return true, nil
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

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/prowendi/PulseGuard/internal/domain"
)

// RateLimitRepo implements the per-channel token bucket described in
// spec §5.4. Each Allow call obtains a write-lock via "BEGIN IMMEDIATE"
// on a dedicated sql.Conn so two concurrent goroutines cannot both
// consume the last token (the SQLite locking protocol serialises
// IMMEDIATE writers in FIFO order under the configured busy_timeout).
type RateLimitRepo struct {
	db    *sql.DB
	clock domain.Clock
}

// NewRateLimitRepo binds the repo to a DB handle and clock.
func NewRateLimitRepo(db *sql.DB, clock domain.Clock) *RateLimitRepo {
	return &RateLimitRepo{db: db, clock: clock}
}

// Allow returns true if the channel may emit one more message under the
// supplied ratePerMin. A new bucket is auto-provisioned on first call.
// ratePerMin<=0 is treated as unlimited.
//
// The bucket math runs inside an IMMEDIATE transaction so SELECT and
// UPDATE on rate_buckets observe the same snapshot — without this two
// goroutines could both read tokens=1, both UPDATE -1, and the channel
// would burst past ratePerMin. database/sql does not expose a
// TxOptions.Immediate flag; we acquire a Conn and issue the SQL
// manually.
func (r *RateLimitRepo) Allow(ctx context.Context, channelID int64, ratePerMin int) (bool, error) {
	if ratePerMin <= 0 {
		return true, nil
	}
	if channelID == 0 {
		return false, fmt.Errorf("%w: rate limit channel_id is zero", domain.ErrValidation)
	}
	now := r.clock.Now()
	nowMs := now.UnixMilli()

	conn, err := r.db.Conn(ctx)
	if err != nil {
		return false, fmt.Errorf("rate-limit conn: %w", err)
	}
	defer func() { _ = conn.Close() }()

	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return false, fmt.Errorf("begin immediate: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()

	var tokens float64
	var updatedMs int64
	err = conn.QueryRowContext(ctx,
		`SELECT tokens, updated_at FROM rate_buckets WHERE channel_id = ?`, channelID,
	).Scan(&tokens, &updatedMs)
	if errors.Is(err, sql.ErrNoRows) {
		// Fresh bucket starts at full capacity, then consumes 1.
		tokens = float64(ratePerMin) - 1
		if _, err := conn.ExecContext(ctx, `
			INSERT INTO rate_buckets (channel_id, tokens, updated_at) VALUES (?, ?, ?)`,
			channelID, tokens, nowMs); err != nil {
			return false, fmt.Errorf("insert bucket: %w", err)
		}
		if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
			return false, fmt.Errorf("commit bucket: %w", err)
		}
		committed = true
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("select bucket: %w", err)
	}

	// Refill: elapsed milliseconds / 60000 * ratePerMin, capped at capacity.
	elapsedMs := nowMs - updatedMs
	if elapsedMs < 0 {
		elapsedMs = 0
	}
	refill := float64(elapsedMs) / 60000.0 * float64(ratePerMin)
	tokens = minFloat(float64(ratePerMin), tokens+refill)

	if tokens >= 1 {
		tokens -= 1
		if _, err := conn.ExecContext(ctx,
			`UPDATE rate_buckets SET tokens = ?, updated_at = ? WHERE channel_id = ?`,
			tokens, nowMs, channelID); err != nil {
			return false, fmt.Errorf("update bucket allow: %w", err)
		}
		if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
			return false, fmt.Errorf("commit allow: %w", err)
		}
		committed = true
		return true, nil
	}

	// Save the refilled tokens (we still update ts so subsequent calls do
	// not re-refill from the same baseline).
	if _, err := conn.ExecContext(ctx,
		`UPDATE rate_buckets SET tokens = ?, updated_at = ? WHERE channel_id = ?`,
		tokens, nowMs, channelID); err != nil {
		return false, fmt.Errorf("update bucket deny: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return false, fmt.Errorf("commit deny: %w", err)
	}
	committed = true
	return false, nil
}

func minFloat(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

// Ensure interface compliance at compile time.
var _ domain.RateLimiter = (*RateLimitRepo)(nil)

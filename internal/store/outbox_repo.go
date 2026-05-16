package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/wendi/pulseguard/internal/domain"
)

// OutboxRepo manages the push_outbox table — the heart of the push
// pipeline. ClaimNext implements a row-level lease via UPDATE...RETURNING
// so multiple worker goroutines can poll the table concurrently without
// double-dispatching.
type OutboxRepo struct {
	db    *sql.DB
	clock domain.Clock
}

// NewOutboxRepo binds the repo to a DB handle.
func NewOutboxRepo(db *sql.DB, clock domain.Clock) *OutboxRepo {
	return &OutboxRepo{db: db, clock: clock}
}

// Insert appends a row in the 'pending' state. The caller may pre-fill
// DedupKey, ChannelID, TenantID, PayloadJSON, and (optionally)
// NextAttemptAt; everything else is derived from the clock.
func (r *OutboxRepo) Insert(ctx context.Context, item *domain.PushOutbox) (int64, error) {
	if item == nil {
		return 0, errors.New("outbox item is nil")
	}
	if item.ChannelID == 0 {
		return 0, fmt.Errorf("%w: outbox channel_id is zero", domain.ErrValidation)
	}
	if item.TenantID == 0 {
		return 0, fmt.Errorf("%w: outbox tenant_id is zero", domain.ErrValidation)
	}
	if item.PayloadJSON == "" {
		return 0, fmt.Errorf("%w: outbox payload_json is empty", domain.ErrValidation)
	}
	now := r.clock.Now()
	nowMs := now.UnixMilli()
	nextMs := nowMs
	if !item.NextAttemptAt.IsZero() {
		nextMs = item.NextAttemptAt.UnixMilli()
	}
	if item.Status == "" {
		item.Status = domain.OutboxPending
	}
	res, err := r.db.ExecContext(ctx, `
		INSERT INTO push_outbox
		  (channel_id, tenant_id, payload_json, dedup_key, status,
		   attempts, next_attempt_at, last_error, worker_id, claimed_at,
		   created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, 0, ?, NULL, NULL, NULL, ?, ?)`,
		item.ChannelID, item.TenantID, item.PayloadJSON, item.DedupKey,
		string(item.Status), nextMs, nowMs, nowMs)
	if err != nil {
		return 0, fmt.Errorf("insert outbox: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("last insert id: %w", err)
	}
	item.ID = id
	item.CreatedAt = toTime(nowMs)
	item.UpdatedAt = toTime(nowMs)
	item.NextAttemptAt = toTime(nextMs)
	return id, nil
}

// ClaimNext atomically claims the next eligible row using
// UPDATE...RETURNING. It returns (nil, nil) when no rows are available
// — the worker is expected to sleep and retry.
func (r *OutboxRepo) ClaimNext(ctx context.Context, workerID string, now time.Time) (*domain.PushOutbox, error) {
	nowMs := now.UnixMilli()
	row := r.db.QueryRowContext(ctx, `
		UPDATE push_outbox
		   SET status = 'in_flight',
		       worker_id = ?,
		       claimed_at = ?,
		       updated_at = ?,
		       attempts = attempts + 1
		 WHERE id = (
		   SELECT id FROM push_outbox
		    WHERE status IN ('pending','retry')
		      AND next_attempt_at <= ?
		    ORDER BY next_attempt_at ASC, id ASC
		    LIMIT 1
		 )
		 RETURNING id, channel_id, tenant_id, payload_json, dedup_key,
		           status, attempts, next_attempt_at, last_error,
		           worker_id, claimed_at, created_at, updated_at`,
		workerID, nowMs, nowMs, nowMs)
	item, err := scanOutbox(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("claim outbox: %w", err)
	}
	return item, nil
}

// MarkSent transitions a row to status='sent' and clears the worker
// lease. The row is retained for observability.
func (r *OutboxRepo) MarkSent(ctx context.Context, id int64, now time.Time) error {
	nowMs := now.UnixMilli()
	_, err := r.db.ExecContext(ctx, `
		UPDATE push_outbox
		   SET status = 'sent',
		       worker_id = NULL,
		       claimed_at = NULL,
		       last_error = NULL,
		       updated_at = ?
		 WHERE id = ?`, nowMs, id)
	if err != nil {
		return fmt.Errorf("mark sent: %w", err)
	}
	return nil
}

// MarkRetry resets a row back to status='retry' with the next scheduled
// attempt and reason.
func (r *OutboxRepo) MarkRetry(ctx context.Context, id int64, nextAt time.Time, reason string) error {
	now := r.clock.Now().UnixMilli()
	_, err := r.db.ExecContext(ctx, `
		UPDATE push_outbox
		   SET status = 'retry',
		       worker_id = NULL,
		       claimed_at = NULL,
		       next_attempt_at = ?,
		       last_error = ?,
		       updated_at = ?
		 WHERE id = ?`, nextAt.UnixMilli(), reason, now, id)
	if err != nil {
		return fmt.Errorf("mark retry: %w", err)
	}
	return nil
}

// MarkDead terminates a row in status='dead' with the recorded reason.
func (r *OutboxRepo) MarkDead(ctx context.Context, id int64, reason string) error {
	now := r.clock.Now().UnixMilli()
	_, err := r.db.ExecContext(ctx, `
		UPDATE push_outbox
		   SET status = 'dead',
		       worker_id = NULL,
		       claimed_at = NULL,
		       last_error = ?,
		       updated_at = ?
		 WHERE id = ?`, reason, now, id)
	if err != nil {
		return fmt.Errorf("mark dead: %w", err)
	}
	return nil
}

// ReclaimInFlight resets stuck in_flight rows whose claim is older than
// olderThan back to 'retry' for a new worker to pick up. Used at startup
// and on a periodic timer to survive crashes.
func (r *OutboxRepo) ReclaimInFlight(ctx context.Context, olderThan time.Time) (int64, error) {
	now := r.clock.Now().UnixMilli()
	cutoff := olderThan.UnixMilli()
	res, err := r.db.ExecContext(ctx, `
		UPDATE push_outbox
		   SET status = 'retry',
		       worker_id = NULL,
		       claimed_at = NULL,
		       next_attempt_at = ?,
		       updated_at = ?
		 WHERE status = 'in_flight'
		   AND claimed_at IS NOT NULL
		   AND claimed_at < ?`, now, now, cutoff)
	if err != nil {
		return 0, fmt.Errorf("reclaim inflight: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("rows affected: %w", err)
	}
	return n, nil
}

func scanOutbox(s interface {
	Scan(dest ...any) error
}) (*domain.PushOutbox, error) {
	item := &domain.PushOutbox{}
	var dedup sql.NullString
	var status string
	var lastErr sql.NullString
	var workerID sql.NullString
	var claimedAt sql.NullInt64
	var nextAt, createdAt, updatedAt int64
	err := s.Scan(
		&item.ID, &item.ChannelID, &item.TenantID, &item.PayloadJSON, &dedup,
		&status, &item.Attempts, &nextAt, &lastErr,
		&workerID, &claimedAt, &createdAt, &updatedAt,
	)
	if err != nil {
		return nil, err
	}
	if dedup.Valid {
		v := dedup.String
		item.DedupKey = &v
	}
	if lastErr.Valid {
		v := lastErr.String
		item.LastError = &v
	}
	if workerID.Valid {
		v := workerID.String
		item.WorkerID = &v
	}
	if claimedAt.Valid {
		t := toTime(claimedAt.Int64)
		item.ClaimedAt = &t
	}
	item.Status = domain.OutboxStatus(status)
	item.NextAttemptAt = toTime(nextAt)
	item.CreatedAt = toTime(createdAt)
	item.UpdatedAt = toTime(updatedAt)
	return item, nil
}

// Ensure interface compliance at compile time.
var _ domain.OutboxRepo = (*OutboxRepo)(nil)

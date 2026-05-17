package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/prowendi/PulseGuard/internal/domain"
)

// DeadLetterRepo persists terminally-failed pushes for inspection / replay.
type DeadLetterRepo struct {
	db    *sql.DB
	clock domain.Clock
}

// NewDeadLetterRepo binds the repo to a DB handle.
func NewDeadLetterRepo(db *sql.DB, clock domain.Clock) *DeadLetterRepo {
	return &DeadLetterRepo{db: db, clock: clock}
}

// Insert writes a dead_letters row.
func (r *DeadLetterRepo) Insert(ctx context.Context, dl *domain.DeadLetter) error {
	if dl == nil {
		return errors.New("deadletter is nil")
	}
	if dl.ChannelID == 0 || dl.TenantID == 0 {
		return fmt.Errorf("%w: deadletter missing channel/tenant id", domain.ErrValidation)
	}
	if dl.LastError == "" {
		return fmt.Errorf("%w: deadletter last_error is empty", domain.ErrValidation)
	}
	now := nowMs(r.clock)
	dl.CreatedAt = toTime(now)

	var rendered sql.NullString
	if dl.RenderedText != nil {
		rendered = sql.NullString{String: *dl.RenderedText, Valid: true}
	}
	res, err := r.db.ExecContext(ctx, `
		INSERT INTO dead_letters
		  (outbox_id, channel_id, tenant_id, payload_json, rendered_text,
		   last_error, attempts, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		dl.OutboxID, dl.ChannelID, dl.TenantID, dl.PayloadJSON,
		rendered, dl.LastError, dl.Attempts, now)
	if err != nil {
		return fmt.Errorf("insert deadletter: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return fmt.Errorf("last insert id: %w", err)
	}
	dl.ID = id
	return nil
}

// ListByTenant returns DLQ rows newest-first, paginated.
func (r *DeadLetterRepo) ListByTenant(ctx context.Context, tenantID int64, page, perPage int) ([]*domain.DeadLetter, int, error) {
	limit, offset := paginate(page, perPage)
	var total int
	if err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM dead_letters WHERE tenant_id = ?`, tenantID,
	).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count deadletters: %w", err)
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, outbox_id, channel_id, tenant_id, payload_json, rendered_text,
		       last_error, attempts, created_at
		  FROM dead_letters WHERE tenant_id = ?
		 ORDER BY created_at DESC, id DESC LIMIT ? OFFSET ?`,
		tenantID, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("list deadletters: %w", err)
	}
	defer rows.Close()
	var out []*domain.DeadLetter
	for rows.Next() {
		dl, err := scanDeadLetter(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, dl)
	}
	return out, total, rows.Err()
}

// Replay copies the DLQ payload back into push_outbox as a brand-new
// pending row and returns its id. The DLQ row is retained for audit.
func (r *DeadLetterRepo) Replay(ctx context.Context, tenantID, id int64) (int64, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	row := tx.QueryRowContext(ctx, `
		SELECT id, outbox_id, channel_id, tenant_id, payload_json, rendered_text,
		       last_error, attempts, created_at
		  FROM dead_letters WHERE id = ? AND tenant_id = ?`, id, tenantID)
	dl, err := scanDeadLetter(row)
	if err != nil {
		return 0, err
	}

	now := nowMs(r.clock)
	res, err := tx.ExecContext(ctx, `
		INSERT INTO push_outbox
		  (channel_id, tenant_id, payload_json, dedup_key, status,
		   attempts, next_attempt_at, last_error, worker_id, claimed_at,
		   created_at, updated_at)
		VALUES (?, ?, ?, NULL, 'pending', 0, ?, NULL, NULL, NULL, ?, ?)`,
		dl.ChannelID, dl.TenantID, dl.PayloadJSON, now, now, now)
	if err != nil {
		return 0, fmt.Errorf("replay insert outbox: %w", err)
	}
	newID, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("last insert id: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit replay: %w", err)
	}
	return newID, nil
}

func scanDeadLetter(s interface {
	Scan(dest ...any) error
}) (*domain.DeadLetter, error) {
	dl := &domain.DeadLetter{}
	var rendered sql.NullString
	var createdMs int64
	err := s.Scan(
		&dl.ID, &dl.OutboxID, &dl.ChannelID, &dl.TenantID,
		&dl.PayloadJSON, &rendered, &dl.LastError, &dl.Attempts, &createdMs,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan deadletter: %w", err)
	}
	if rendered.Valid {
		v := rendered.String
		dl.RenderedText = &v
	}
	dl.CreatedAt = toTime(createdMs)
	return dl, nil
}

// Ensure interface compliance at compile time.
var _ domain.DeadLetterRepo = (*DeadLetterRepo)(nil)

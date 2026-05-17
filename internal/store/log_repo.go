package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/prowendi/PulseGuard/internal/domain"
)

// LogRepo persists push history rows for observability + auditing.
type LogRepo struct {
	db    *sql.DB
	clock domain.Clock
}

// NewLogRepo binds the repo to a DB handle and clock.
func NewLogRepo(db *sql.DB, clock domain.Clock) *LogRepo {
	return &LogRepo{db: db, clock: clock}
}

// Insert writes a push log row. Status drives whether tg_message_id and
// error columns are populated.
func (r *LogRepo) Insert(ctx context.Context, log *domain.PushLog) error {
	if log == nil {
		return errors.New("log is nil")
	}
	if log.ChannelID == 0 {
		return fmt.Errorf("%w: log channel_id is zero", domain.ErrValidation)
	}
	if log.TenantID == 0 {
		return fmt.Errorf("%w: log tenant_id is zero", domain.ErrValidation)
	}
	switch log.Status {
	case domain.LogSent, domain.LogFailed, domain.LogDead:
	default:
		return fmt.Errorf("%w: invalid log status %q", domain.ErrValidation, log.Status)
	}
	now := nowMs(r.clock)
	log.CreatedAt = toTime(now)

	var outboxID sql.NullInt64
	if log.OutboxID != nil {
		outboxID = sql.NullInt64{Int64: *log.OutboxID, Valid: true}
	}
	var msgID sql.NullInt64
	if log.TGMessageID != nil {
		msgID = sql.NullInt64{Int64: *log.TGMessageID, Valid: true}
	}
	var errStr sql.NullString
	if log.Error != nil {
		errStr = sql.NullString{String: *log.Error, Valid: true}
	}
	res, err := r.db.ExecContext(ctx, `
		INSERT INTO push_logs
		  (outbox_id, channel_id, tenant_id, payload_json, rendered_text,
		   tg_message_id, status, error, attempts, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		outboxID, log.ChannelID, log.TenantID, log.PayloadJSON,
		log.RenderedText, msgID, string(log.Status), errStr, log.Attempts, now)
	if err != nil {
		return fmt.Errorf("insert log: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return fmt.Errorf("last insert id: %w", err)
	}
	log.ID = id
	return nil
}

// ListByTenant returns logs in newest-first order; perPage is clamped to
// [1, 500] and page is 1-based.
func (r *LogRepo) ListByTenant(ctx context.Context, tenantID int64, page, perPage int) ([]*domain.PushLog, int, error) {
	return r.list(ctx, tenantID, 0, page, perPage)
}

// ListByChannel filters additionally on channel_id; tenantID is enforced
// for isolation.
func (r *LogRepo) ListByChannel(ctx context.Context, tenantID, channelID int64, page, perPage int) ([]*domain.PushLog, int, error) {
	return r.list(ctx, tenantID, channelID, page, perPage)
}

func (r *LogRepo) list(ctx context.Context, tenantID, channelID int64, page, perPage int) ([]*domain.PushLog, int, error) {
	limit, offset := paginate(page, perPage)
	var (
		rows    *sql.Rows
		err     error
		total   int
		whereCh = channelID > 0
	)
	if whereCh {
		err = r.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM push_logs WHERE tenant_id = ? AND channel_id = ?`,
			tenantID, channelID,
		).Scan(&total)
	} else {
		err = r.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM push_logs WHERE tenant_id = ?`, tenantID,
		).Scan(&total)
	}
	if err != nil {
		return nil, 0, fmt.Errorf("count logs: %w", err)
	}

	if whereCh {
		rows, err = r.db.QueryContext(ctx, `
			SELECT id, outbox_id, channel_id, tenant_id, payload_json, rendered_text,
			       tg_message_id, status, error, attempts, created_at
			  FROM push_logs WHERE tenant_id = ? AND channel_id = ?
			 ORDER BY created_at DESC, id DESC LIMIT ? OFFSET ?`,
			tenantID, channelID, limit, offset)
	} else {
		rows, err = r.db.QueryContext(ctx, `
			SELECT id, outbox_id, channel_id, tenant_id, payload_json, rendered_text,
			       tg_message_id, status, error, attempts, created_at
			  FROM push_logs WHERE tenant_id = ?
			 ORDER BY created_at DESC, id DESC LIMIT ? OFFSET ?`,
			tenantID, limit, offset)
	}
	if err != nil {
		return nil, 0, fmt.Errorf("list logs: %w", err)
	}
	defer rows.Close()

	var out []*domain.PushLog
	for rows.Next() {
		l, err := scanLog(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, l)
	}
	return out, total, rows.Err()
}

// PurgeOlderThan deletes push_logs older than t and returns the row count.
func (r *LogRepo) PurgeOlderThan(ctx context.Context, t time.Time) (int64, error) {
	res, err := r.db.ExecContext(ctx,
		`DELETE FROM push_logs WHERE created_at < ?`, t.UnixMilli())
	if err != nil {
		return 0, fmt.Errorf("purge logs: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("rows affected: %w", err)
	}
	return n, nil
}

func scanLog(s interface {
	Scan(dest ...any) error
}) (*domain.PushLog, error) {
	l := &domain.PushLog{}
	var outboxID sql.NullInt64
	var msgID sql.NullInt64
	var errStr sql.NullString
	var status string
	var createdMs int64
	err := s.Scan(
		&l.ID, &outboxID, &l.ChannelID, &l.TenantID,
		&l.PayloadJSON, &l.RenderedText, &msgID, &status, &errStr,
		&l.Attempts, &createdMs,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan log: %w", err)
	}
	if outboxID.Valid {
		v := outboxID.Int64
		l.OutboxID = &v
	}
	if msgID.Valid {
		v := msgID.Int64
		l.TGMessageID = &v
	}
	if errStr.Valid {
		v := errStr.String
		l.Error = &v
	}
	l.Status = domain.LogStatus(status)
	l.CreatedAt = toTime(createdMs)
	return l, nil
}

// paginate clamps page/perPage to sane bounds and returns (limit, offset).
func paginate(page, perPage int) (limit, offset int) {
	if perPage <= 0 {
		perPage = 20
	}
	if perPage > 500 {
		perPage = 500
	}
	if page <= 0 {
		page = 1
	}
	return perPage, (page - 1) * perPage
}

// Ensure interface compliance at compile time.
var _ domain.LogRepo = (*LogRepo)(nil)

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/wendi/pulseguard/internal/domain"
)

// ChannelRepo persists channels (1 push_token -> 1 bot + 1 chat + 1 template).
type ChannelRepo struct {
	db    *sql.DB
	clock domain.Clock
}

// NewChannelRepo binds the repo to a DB handle.
func NewChannelRepo(db *sql.DB, clock domain.Clock) *ChannelRepo {
	return &ChannelRepo{db: db, clock: clock}
}

// Insert writes a new channel row.
func (r *ChannelRepo) Insert(ctx context.Context, c *domain.Channel) error {
	if err := validateChannel(c); err != nil {
		return err
	}
	now := nowMs(r.clock)
	res, err := r.db.ExecContext(ctx, `
		INSERT INTO channels
		  (tenant_id, name, push_token, bot_id, template_id, chat_id,
		   rate_per_min, dedup_window_s, enabled, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.TenantID, c.Name, c.PushToken, c.BotID, c.TemplateID, c.ChatID,
		c.RatePerMin, c.DedupWindowS, boolToInt(c.Enabled), now, now)
	if err != nil {
		return fmt.Errorf("insert channel: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return fmt.Errorf("last insert id: %w", err)
	}
	c.ID = id
	c.CreatedAt = toTime(now)
	c.UpdatedAt = toTime(now)
	return nil
}

// Update mutates an existing channel.
func (r *ChannelRepo) Update(ctx context.Context, c *domain.Channel) error {
	if err := validateChannel(c); err != nil {
		return err
	}
	if c.ID == 0 {
		return fmt.Errorf("%w: channel id is zero", domain.ErrValidation)
	}
	now := nowMs(r.clock)
	res, err := r.db.ExecContext(ctx, `
		UPDATE channels SET name = ?, push_token = ?, bot_id = ?, template_id = ?,
		                    chat_id = ?, rate_per_min = ?, dedup_window_s = ?,
		                    enabled = ?, updated_at = ?
		 WHERE id = ? AND tenant_id = ?`,
		c.Name, c.PushToken, c.BotID, c.TemplateID, c.ChatID,
		c.RatePerMin, c.DedupWindowS, boolToInt(c.Enabled), now,
		c.ID, c.TenantID)
	if err != nil {
		return fmt.Errorf("update channel: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return domain.ErrNotFound
	}
	c.UpdatedAt = toTime(now)
	return nil
}

// Delete removes a channel owned by tenantID.
func (r *ChannelRepo) Delete(ctx context.Context, tenantID, id int64) error {
	res, err := r.db.ExecContext(ctx,
		`DELETE FROM channels WHERE id = ? AND tenant_id = ?`, id, tenantID)
	if err != nil {
		return fmt.Errorf("delete channel: %w", err)
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

// GetByID returns the channel id within the given tenant scope.
func (r *ChannelRepo) GetByID(ctx context.Context, tenantID, id int64) (*domain.Channel, error) {
	return scanChannel(r.db.QueryRowContext(ctx, channelSelect+
		` WHERE id = ? AND tenant_id = ?`, id, tenantID))
}

// GetByPushToken returns the channel matching the push_token (global index).
// Webhook handlers use this to map an incoming token to its owning channel.
func (r *ChannelRepo) GetByPushToken(ctx context.Context, pushToken string) (*domain.Channel, error) {
	return scanChannel(r.db.QueryRowContext(ctx, channelSelect+
		` WHERE push_token = ?`, pushToken))
}

// ListByTenant returns every channel owned by a tenant.
func (r *ChannelRepo) ListByTenant(ctx context.Context, tenantID int64) ([]*domain.Channel, error) {
	rows, err := r.db.QueryContext(ctx, channelSelect+
		` WHERE tenant_id = ? ORDER BY id ASC`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list channels: %w", err)
	}
	defer rows.Close()
	var out []*domain.Channel
	for rows.Next() {
		c, err := scanChannel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

const channelSelect = `
SELECT id, tenant_id, name, push_token, bot_id, template_id, chat_id,
       rate_per_min, dedup_window_s, enabled, created_at, updated_at
  FROM channels`

func scanChannel(s interface {
	Scan(dest ...any) error
}) (*domain.Channel, error) {
	c := &domain.Channel{}
	var enabled int
	var createdMs, updatedMs int64
	err := s.Scan(&c.ID, &c.TenantID, &c.Name, &c.PushToken,
		&c.BotID, &c.TemplateID, &c.ChatID,
		&c.RatePerMin, &c.DedupWindowS, &enabled, &createdMs, &updatedMs)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan channel: %w", err)
	}
	c.Enabled = enabled != 0
	c.CreatedAt = toTime(createdMs)
	c.UpdatedAt = toTime(updatedMs)
	return c, nil
}

func validateChannel(c *domain.Channel) error {
	if c == nil {
		return errors.New("channel is nil")
	}
	if c.TenantID == 0 {
		return fmt.Errorf("%w: channel tenant_id is zero", domain.ErrValidation)
	}
	if c.Name == "" {
		return fmt.Errorf("%w: channel name is empty", domain.ErrValidation)
	}
	if c.PushToken == "" {
		return fmt.Errorf("%w: channel push_token is empty", domain.ErrValidation)
	}
	if c.BotID == 0 {
		return fmt.Errorf("%w: channel bot_id is zero", domain.ErrValidation)
	}
	if c.TemplateID == 0 {
		return fmt.Errorf("%w: channel template_id is zero", domain.ErrValidation)
	}
	if c.ChatID == "" {
		return fmt.Errorf("%w: channel chat_id is empty", domain.ErrValidation)
	}
	if c.RatePerMin < 0 {
		return fmt.Errorf("%w: rate_per_min must be >= 0", domain.ErrValidation)
	}
	if c.DedupWindowS < 0 {
		return fmt.Errorf("%w: dedup_window_s must be >= 0", domain.ErrValidation)
	}
	return nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// Ensure interface compliance at compile time.
var _ domain.ChannelRepo = (*ChannelRepo)(nil)

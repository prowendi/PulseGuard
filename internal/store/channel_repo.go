package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/prowendi/PulseGuard/internal/domain"
)

// ChannelRepo persists channels (1 push_token -> 1 bot + 1 chat) plus
// each channel's many-to-many template bindings (channel_templates).
// Reads always hydrate Channel.Templates so callers can use
// DefaultTemplateID() / HasTemplate() without a second round-trip.
type ChannelRepo struct {
	db    *sql.DB
	clock domain.Clock
}

// NewChannelRepo binds the repo to a DB handle.
func NewChannelRepo(db *sql.DB, clock domain.Clock) *ChannelRepo {
	return &ChannelRepo{db: db, clock: clock}
}

// Insert writes a new channel row plus any pre-populated template
// bindings in c.Templates, transactionally. Exactly one binding may
// carry IsDefault=true; if none do and there is at least one binding,
// the first one is auto-promoted to default to satisfy "channel must
// have a default template at all times" invariant.
func (r *ChannelRepo) Insert(ctx context.Context, c *domain.Channel) error {
	if err := validateChannel(c); err != nil {
		return err
	}
	now := nowMs(r.clock)

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, `
		INSERT INTO channels
		  (tenant_id, name, push_token, bot_id, chat_id,
		   rate_per_min, dedup_window_s, enabled, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.TenantID, c.Name, c.PushToken, c.BotID, c.ChatID,
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

	if err := r.writeBindingsTx(ctx, tx, c.ID, c.Templates, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// Update mutates an existing channel. Template bindings in c.Templates
// fully replace the existing set: removed bindings are deleted, new
// ones inserted. Pass a nil/empty Templates slice to leave the
// existing bindings untouched (use ReplaceTemplates explicitly for
// the "wipe all" case).
func (r *ChannelRepo) Update(ctx context.Context, c *domain.Channel) error {
	if err := validateChannel(c); err != nil {
		return err
	}
	if c.ID == 0 {
		return fmt.Errorf("%w: channel id is zero", domain.ErrValidation)
	}
	now := nowMs(r.clock)

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, `
		UPDATE channels SET name = ?, push_token = ?, bot_id = ?,
		                    chat_id = ?, rate_per_min = ?, dedup_window_s = ?,
		                    enabled = ?, updated_at = ?
		 WHERE id = ? AND tenant_id = ?`,
		c.Name, c.PushToken, c.BotID, c.ChatID,
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

	if len(c.Templates) > 0 {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM channel_templates WHERE channel_id = ?`, c.ID); err != nil {
			return fmt.Errorf("clear bindings: %w", err)
		}
		if err := r.writeBindingsTx(ctx, tx, c.ID, c.Templates, now); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// ReplaceTemplates atomically swaps the channel's template bindings to
// the supplied list. Empty list clears the bindings. Used by the UI
// handler when the user updates only the "bound templates" form
// section, leaving the rest of the channel intact.
func (r *ChannelRepo) ReplaceTemplates(ctx context.Context, tenantID, channelID int64, bindings []*domain.ChannelTemplate) error {
	now := nowMs(r.clock)
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Verify ownership.
	var owner int64
	if err := tx.QueryRowContext(ctx,
		`SELECT tenant_id FROM channels WHERE id = ?`, channelID).Scan(&owner); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.ErrNotFound
		}
		return fmt.Errorf("verify channel owner: %w", err)
	}
	if owner != tenantID {
		return domain.ErrNotFound
	}

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM channel_templates WHERE channel_id = ?`, channelID); err != nil {
		return fmt.Errorf("clear bindings: %w", err)
	}
	if err := r.writeBindingsTx(ctx, tx, channelID, bindings, now); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *ChannelRepo) writeBindingsTx(ctx context.Context, tx *sql.Tx, channelID int64, bindings []*domain.ChannelTemplate, now int64) error {
	if len(bindings) == 0 {
		return nil
	}
	// Enforce exactly-one-default invariant. If nothing is flagged,
	// promote the first binding. If multiple are flagged, only the
	// first survives.
	defaultIdx := -1
	for i, b := range bindings {
		if b.IsDefault {
			if defaultIdx == -1 {
				defaultIdx = i
			} else {
				b.IsDefault = false
			}
		}
	}
	if defaultIdx == -1 {
		bindings[0].IsDefault = true
	}
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO channel_templates
		  (channel_id, template_id, is_default, sort_order, condition, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare binding insert: %w", err)
	}
	defer stmt.Close()
	for _, b := range bindings {
		if b.TemplateID == 0 {
			return fmt.Errorf("%w: binding template_id is zero", domain.ErrValidation)
		}
		if _, err := stmt.ExecContext(ctx, channelID, b.TemplateID,
			boolToInt(b.IsDefault), b.SortOrder, b.Condition, now); err != nil {
			return fmt.Errorf("insert binding: %w", err)
		}
		b.ChannelID = channelID
		b.CreatedAt = toTime(now)
	}
	return nil
}

// Delete removes a channel owned by tenantID. ON DELETE CASCADE on
// channel_templates handles the binding cleanup automatically.
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

// GetByID returns the channel id within the given tenant scope with
// its template bindings populated.
func (r *ChannelRepo) GetByID(ctx context.Context, tenantID, id int64) (*domain.Channel, error) {
	c, err := scanChannel(r.db.QueryRowContext(ctx, channelSelect+
		` WHERE id = ? AND tenant_id = ?`, id, tenantID))
	if err != nil {
		return nil, err
	}
	if err := r.loadBindings(ctx, c); err != nil {
		return nil, err
	}
	return c, nil
}

// GetByPushToken returns the channel matching the push_token plus its
// template bindings. Webhook handlers use this to map an incoming
// token to its owning channel.
func (r *ChannelRepo) GetByPushToken(ctx context.Context, pushToken string) (*domain.Channel, error) {
	c, err := scanChannel(r.db.QueryRowContext(ctx, channelSelect+
		` WHERE push_token = ?`, pushToken))
	if err != nil {
		return nil, err
	}
	if err := r.loadBindings(ctx, c); err != nil {
		return nil, err
	}
	return c, nil
}

// ListByTenant returns every channel owned by a tenant with bindings
// hydrated.
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
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, c := range out {
		if err := r.loadBindings(ctx, c); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (r *ChannelRepo) loadBindings(ctx context.Context, c *domain.Channel) error {
	if c == nil {
		return nil
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT channel_id, template_id, is_default, sort_order, condition, created_at
		  FROM channel_templates
		 WHERE channel_id = ?
		 ORDER BY sort_order ASC, template_id ASC`, c.ID)
	if err != nil {
		return fmt.Errorf("load bindings: %w", err)
	}
	defer rows.Close()
	var out []*domain.ChannelTemplate
	for rows.Next() {
		b := &domain.ChannelTemplate{}
		var isDefault int
		var createdMs int64
		if err := rows.Scan(&b.ChannelID, &b.TemplateID, &isDefault, &b.SortOrder, &b.Condition, &createdMs); err != nil {
			return fmt.Errorf("scan binding: %w", err)
		}
		b.IsDefault = isDefault != 0
		b.CreatedAt = toTime(createdMs)
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	c.Templates = out
	return nil
}

const channelSelect = `
SELECT id, tenant_id, name, push_token, bot_id, chat_id,
       rate_per_min, dedup_window_s, enabled, created_at, updated_at
  FROM channels`

func scanChannel(s interface {
	Scan(dest ...any) error
}) (*domain.Channel, error) {
	c := &domain.Channel{}
	var enabled int
	var createdMs, updatedMs int64
	err := s.Scan(&c.ID, &c.TenantID, &c.Name, &c.PushToken,
		&c.BotID, &c.ChatID,
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

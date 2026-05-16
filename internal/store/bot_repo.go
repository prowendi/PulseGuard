package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/wendi/pulseguard/internal/domain"
)

// BotRepo persists tenant-owned bots. The plaintext bot_token is
// encrypted on write and decrypted on read using the injected Cipher.
type BotRepo struct {
	db     *sql.DB
	clock  domain.Clock
	cipher *Cipher
}

// NewBotRepo binds the repo to a DB handle, clock, and cipher.
func NewBotRepo(db *sql.DB, clock domain.Clock, cipher *Cipher) *BotRepo {
	return &BotRepo{db: db, clock: clock, cipher: cipher}
}

// Insert writes a new bot row, encrypting the token. An empty Platform
// defaults to PlatformTelegram so callers (and existing tests) can omit
// the field; invalid values are rejected.
func (r *BotRepo) Insert(ctx context.Context, b *domain.Bot) error {
	if err := validateBot(b); err != nil {
		return err
	}
	enc, err := r.cipher.Encrypt([]byte(b.BotToken))
	if err != nil {
		return fmt.Errorf("encrypt bot token: %w", err)
	}
	now := nowMs(r.clock)
	res, err := r.db.ExecContext(ctx, `
		INSERT INTO bots (tenant_id, name, platform, bot_token_enc, description, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		b.TenantID, b.Name, b.Platform, enc, b.Description, now, now)
	if err != nil {
		return fmt.Errorf("insert bot: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return fmt.Errorf("last insert id: %w", err)
	}
	b.ID = id
	b.CreatedAt = toTime(now)
	b.UpdatedAt = toTime(now)
	return nil
}

// Update mutates an existing bot (name/platform/token/description). Tenant id
// is used as a guard to prevent cross-tenant edits.
func (r *BotRepo) Update(ctx context.Context, b *domain.Bot) error {
	if err := validateBot(b); err != nil {
		return err
	}
	if b.ID == 0 {
		return fmt.Errorf("%w: bot id is zero", domain.ErrValidation)
	}
	enc, err := r.cipher.Encrypt([]byte(b.BotToken))
	if err != nil {
		return fmt.Errorf("encrypt bot token: %w", err)
	}
	now := nowMs(r.clock)
	res, err := r.db.ExecContext(ctx, `
		UPDATE bots SET name = ?, platform = ?, bot_token_enc = ?, description = ?, updated_at = ?
		 WHERE id = ? AND tenant_id = ?`,
		b.Name, b.Platform, enc, b.Description, now, b.ID, b.TenantID)
	if err != nil {
		return fmt.Errorf("update bot: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return domain.ErrNotFound
	}
	b.UpdatedAt = toTime(now)
	return nil
}

// Delete removes a bot owned by tenantID. Missing rows return ErrNotFound.
func (r *BotRepo) Delete(ctx context.Context, tenantID, id int64) error {
	res, err := r.db.ExecContext(ctx,
		`DELETE FROM bots WHERE id = ? AND tenant_id = ?`, id, tenantID)
	if err != nil {
		return fmt.Errorf("delete bot: %w", err)
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

// GetByID fetches a bot owned by tenantID, decrypting the token.
func (r *BotRepo) GetByID(ctx context.Context, tenantID, id int64) (*domain.Bot, error) {
	row := r.db.QueryRowContext(ctx, `
		SELECT id, tenant_id, name, platform, bot_token_enc, description, created_at, updated_at
		  FROM bots WHERE id = ? AND tenant_id = ?`, id, tenantID)
	return r.scanBot(row)
}

// ListByTenant returns every bot of a tenant (smallest id first).
func (r *BotRepo) ListByTenant(ctx context.Context, tenantID int64) ([]*domain.Bot, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, tenant_id, name, platform, bot_token_enc, description, created_at, updated_at
		  FROM bots WHERE tenant_id = ? ORDER BY id ASC`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list bots: %w", err)
	}
	defer rows.Close()
	var out []*domain.Bot
	for rows.Next() {
		b, err := r.scanBot(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// ListAll returns every enabled bot across all tenants, smallest id first.
// Only the startup wire-up and admin tooling should call this — it
// deliberately skips the tenantID guard.
func (r *BotRepo) ListAll(ctx context.Context) ([]*domain.Bot, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, tenant_id, name, platform, bot_token_enc, description, created_at, updated_at
		  FROM bots ORDER BY id ASC`)
	if err != nil {
		return nil, fmt.Errorf("list all bots: %w", err)
	}
	defer rows.Close()
	var out []*domain.Bot
	for rows.Next() {
		b, err := r.scanBot(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (r *BotRepo) scanBot(s interface {
	Scan(dest ...any) error
}) (*domain.Bot, error) {
	b := &domain.Bot{}
	var enc []byte
	var createdMs, updatedMs int64
	err := s.Scan(&b.ID, &b.TenantID, &b.Name, &b.Platform, &enc, &b.Description, &createdMs, &updatedMs)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan bot: %w", err)
	}
	plain, err := r.cipher.Decrypt(enc)
	if err != nil {
		return nil, fmt.Errorf("decrypt bot token: %w", err)
	}
	b.BotToken = string(plain)
	b.CreatedAt = toTime(createdMs)
	b.UpdatedAt = toTime(updatedMs)
	return b, nil
}

func validateBot(b *domain.Bot) error {
	if b == nil {
		return errors.New("bot is nil")
	}
	if b.TenantID == 0 {
		return fmt.Errorf("%w: bot tenant_id is zero", domain.ErrValidation)
	}
	if b.Name == "" {
		return fmt.Errorf("%w: bot name is empty", domain.ErrValidation)
	}
	if b.BotToken == "" {
		return fmt.Errorf("%w: bot token is empty", domain.ErrValidation)
	}
	// Default empty Platform to telegram so existing callers/tests stay
	// compatible. Reject anything else so a typo can never silently
	// disable the listener.
	if b.Platform == "" {
		b.Platform = domain.PlatformTelegram
	}
	if !domain.IsValidPlatform(b.Platform) {
		return fmt.Errorf("%w: unknown bot platform %q", domain.ErrValidation, b.Platform)
	}
	return nil
}

// Ensure interface compliance at compile time.
var _ domain.BotRepo = (*BotRepo)(nil)

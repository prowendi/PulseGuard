package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/wendi/pulseguard/internal/domain"
)

// CommandRepo persists tenant-owned Starlark commands.
type CommandRepo struct {
	db    *sql.DB
	clock domain.Clock
}

// NewCommandRepo binds the repo to a DB handle and clock.
func NewCommandRepo(db *sql.DB, clock domain.Clock) *CommandRepo {
	return &CommandRepo{db: db, clock: clock}
}

// Insert writes a new command row. Returns the assigned id via *Command.
func (r *CommandRepo) Insert(ctx context.Context, c *domain.Command) error {
	if err := validateCommand(c); err != nil {
		return err
	}
	if err := r.ensureBotOwnership(ctx, c.TenantID, c.BotID); err != nil {
		return err
	}
	now := nowMs(r.clock)
	res, err := r.db.ExecContext(ctx, `
		INSERT INTO commands (tenant_id, bot_id, name, description, code, enabled, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		c.TenantID, c.BotID, c.Name, c.Description, c.Code, boolToIntCmd(c.Enabled), now, now)
	if err != nil {
		if isUniqueConstraintErr(err) {
			return fmt.Errorf("%w: command name %q already exists for this bot",
				domain.ErrConflict, c.Name)
		}
		return fmt.Errorf("insert command: %w", err)
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

// Update mutates an existing command owned by the same tenant. BotID
// is locked at insert time: an operator who wants to move a command
// to a different bot must delete + recreate, which is intentional
// because subscribers carry stale (command_id, bot_id) pairs that
// would silently break under a re-binding.
func (r *CommandRepo) Update(ctx context.Context, c *domain.Command) error {
	if err := validateCommand(c); err != nil {
		return err
	}
	if c.ID == 0 {
		return fmt.Errorf("%w: command id is zero", domain.ErrValidation)
	}
	now := nowMs(r.clock)
	res, err := r.db.ExecContext(ctx, `
		UPDATE commands
		   SET name = ?, description = ?, code = ?, enabled = ?, updated_at = ?
		 WHERE id = ? AND tenant_id = ?`,
		c.Name, c.Description, c.Code, boolToIntCmd(c.Enabled), now, c.ID, c.TenantID)
	if err != nil {
		if isUniqueConstraintErr(err) {
			return fmt.Errorf("%w: command name %q already exists for this bot",
				domain.ErrConflict, c.Name)
		}
		return fmt.Errorf("update command: %w", err)
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

// Delete removes a command owned by tenantID.
func (r *CommandRepo) Delete(ctx context.Context, tenantID, id int64) error {
	res, err := r.db.ExecContext(ctx,
		`DELETE FROM commands WHERE id = ? AND tenant_id = ?`, id, tenantID)
	if err != nil {
		return fmt.Errorf("delete command: %w", err)
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

// GetByID fetches a command scoped to a tenant.
func (r *CommandRepo) GetByID(ctx context.Context, tenantID, id int64) (*domain.Command, error) {
	row := r.db.QueryRowContext(ctx, `
		SELECT id, tenant_id, bot_id, name, description, code, enabled, created_at, updated_at
		  FROM commands WHERE id = ? AND tenant_id = ?`, id, tenantID)
	return scanCommand(row)
}

// GetByTenantAndName fetches a command by (tenant, name). Ambiguous
// under per-bot scoping (a tenant can have /name on two different
// bots), so this returns the lowest-id match — historical signature
// preserved for callers that don't yet know about bot_id. Prefer
// GetByBotAndName whenever the caller has a bot id in scope.
func (r *CommandRepo) GetByTenantAndName(ctx context.Context, tenantID int64, name string) (*domain.Command, error) {
	row := r.db.QueryRowContext(ctx, `
		SELECT id, tenant_id, bot_id, name, description, code, enabled, created_at, updated_at
		  FROM commands WHERE tenant_id = ? AND name = ?
		  ORDER BY id ASC LIMIT 1`, tenantID, name)
	return scanCommand(row)
}

// ListByTenant returns every command of a tenant (smallest id first).
func (r *CommandRepo) ListByTenant(ctx context.Context, tenantID int64) ([]*domain.Command, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, tenant_id, bot_id, name, description, code, enabled, created_at, updated_at
		  FROM commands WHERE tenant_id = ? ORDER BY id ASC`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list commands: %w", err)
	}
	defer rows.Close()
	var out []*domain.Command
	for rows.Next() {
		c, err := scanCommand(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetByBotAndName is the listener's hot-path resolver. Under per-bot
// scoping the lookup is now a direct match on the commands row —
// no JOIN to bots needed. Returns ErrNotFound on unknown bot/name and
// on disabled commands (disabled = absent for the dispatcher).
func (r *CommandRepo) GetByBotAndName(ctx context.Context, botID int64, name string) (*domain.Command, error) {
	row := r.db.QueryRowContext(ctx, `
		SELECT id, tenant_id, bot_id, name, description, code, enabled, created_at, updated_at
		  FROM commands
		 WHERE bot_id = ?
		   AND name = ?
		   AND enabled = 1
		 LIMIT 1`, botID, name)
	return scanCommand(row)
}

// ListByBot returns every ENABLED command bound to botID. Powers the
// Telegram listener's setMyCommands publisher and the /commands
// built-in: both publish a public catalog, so disabled rows MUST stay
// hidden (same "disabled = absent" rule the dispatcher uses).
//
// botID is the PulseGuard DB primary key (bots.id), NOT the Telegram
// numeric token prefix — same convention as GetByBotAndName.
func (r *CommandRepo) ListByBot(ctx context.Context, botID int64) ([]*domain.Command, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, tenant_id, bot_id, name, description, code, enabled, created_at, updated_at
		  FROM commands
		 WHERE bot_id = ?
		   AND enabled = 1
		 ORDER BY id ASC`, botID)
	if err != nil {
		return nil, fmt.Errorf("list commands by bot: %w", err)
	}
	defer rows.Close()
	var out []*domain.Command
	for rows.Next() {
		c, err := scanCommand(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func scanCommand(s interface {
	Scan(dest ...any) error
}) (*domain.Command, error) {
	c := &domain.Command{}
	var enabled int
	var createdMs, updatedMs int64
	err := s.Scan(&c.ID, &c.TenantID, &c.BotID, &c.Name, &c.Description, &c.Code, &enabled, &createdMs, &updatedMs)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan command: %w", err)
	}
	c.Enabled = enabled != 0
	c.CreatedAt = toTime(createdMs)
	c.UpdatedAt = toTime(updatedMs)
	return c, nil
}

// validateCommand enforces the minimal invariants. Stricter shape
// checks (e.g. Starlark parse) live in the web layer / executor.
func validateCommand(c *domain.Command) error {
	if c == nil {
		return errors.New("command is nil")
	}
	if c.TenantID == 0 {
		return fmt.Errorf("%w: command tenant_id is zero", domain.ErrValidation)
	}
	if c.BotID == 0 {
		return fmt.Errorf("%w: command bot_id is zero", domain.ErrValidation)
	}
	c.Name = strings.TrimSpace(c.Name)
	if c.Name == "" {
		return fmt.Errorf("%w: command name is empty", domain.ErrValidation)
	}
	if c.Code == "" {
		return fmt.Errorf("%w: command code is empty", domain.ErrValidation)
	}
	return nil
}

// ensureBotOwnership verifies that the (bot_id, tenant_id) pair is
// consistent: the bot must exist AND belong to the supplied tenant.
// This is a defence-in-depth check on top of the FK constraint —
// without it a tenant could attach a command to another tenant's bot
// by supplying its raw id, and the FK alone would happily allow it
// (FKs check bot existence, not tenant ownership).
//
// SEC-3 (2026-05): returns domain.ErrNotFound (NOT ErrValidation) for
// both "unknown id" and "cross-tenant" cases. The bot is invisible to
// the calling tenant either way, so the API maps it to a uniform 404
// and never echoes the bot id back to the client. Echoing the bot id
// would leak the existence of other tenants' resources to enumeration.
func (r *CommandRepo) ensureBotOwnership(ctx context.Context, tenantID, botID int64) error {
	var ownerTenant int64
	err := r.db.QueryRowContext(ctx,
		`SELECT tenant_id FROM bots WHERE id = ?`, botID).Scan(&ownerTenant)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("verify bot ownership: %w", err)
	}
	if ownerTenant != tenantID {
		return domain.ErrNotFound
	}
	return nil
}

func boolToIntCmd(b bool) int { return boolToInt(b) }

// Ensure interface compliance at compile time.
var _ domain.CommandRepo = (*CommandRepo)(nil)

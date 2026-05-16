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
	now := nowMs(r.clock)
	res, err := r.db.ExecContext(ctx, `
		INSERT INTO commands (tenant_id, name, description, code, enabled, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		c.TenantID, c.Name, c.Description, c.Code, boolToIntCmd(c.Enabled), now, now)
	if err != nil {
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

// Update mutates an existing command owned by the same tenant.
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
		SELECT id, tenant_id, name, description, code, enabled, created_at, updated_at
		  FROM commands WHERE id = ? AND tenant_id = ?`, id, tenantID)
	return scanCommand(row)
}

// GetByTenantAndName fetches a command by (tenant, name).
func (r *CommandRepo) GetByTenantAndName(ctx context.Context, tenantID int64, name string) (*domain.Command, error) {
	row := r.db.QueryRowContext(ctx, `
		SELECT id, tenant_id, name, description, code, enabled, created_at, updated_at
		  FROM commands WHERE tenant_id = ? AND name = ?`, tenantID, name)
	return scanCommand(row)
}

// ListByTenant returns every command of a tenant (smallest id first).
func (r *CommandRepo) ListByTenant(ctx context.Context, tenantID int64) ([]*domain.Command, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, tenant_id, name, description, code, enabled, created_at, updated_at
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

// GetByBotAndName joins commands → bots so the Telegram listener can
// resolve "command N triggered from bot M" without first looking up
// the tenant_id. Returns ErrNotFound on unknown bot or command, and on
// disabled commands (we treat disabled = absent for the dispatcher).
func (r *CommandRepo) GetByBotAndName(ctx context.Context, botID int64, name string) (*domain.Command, error) {
	row := r.db.QueryRowContext(ctx, `
		SELECT c.id, c.tenant_id, c.name, c.description, c.code, c.enabled, c.created_at, c.updated_at
		  FROM commands c
		  JOIN bots b ON b.tenant_id = c.tenant_id
		 WHERE b.id = ?
		   AND c.name = ?
		   AND c.enabled = 1
		 LIMIT 1`, botID, name)
	return scanCommand(row)
}

func scanCommand(s interface {
	Scan(dest ...any) error
}) (*domain.Command, error) {
	c := &domain.Command{}
	var enabled int
	var createdMs, updatedMs int64
	err := s.Scan(&c.ID, &c.TenantID, &c.Name, &c.Description, &c.Code, &enabled, &createdMs, &updatedMs)
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
	c.Name = strings.TrimSpace(c.Name)
	if c.Name == "" {
		return fmt.Errorf("%w: command name is empty", domain.ErrValidation)
	}
	if c.Code == "" {
		return fmt.Errorf("%w: command code is empty", domain.ErrValidation)
	}
	return nil
}

func boolToIntCmd(b bool) int { return boolToInt(b) }

// Ensure interface compliance at compile time.
var _ domain.CommandRepo = (*CommandRepo)(nil)

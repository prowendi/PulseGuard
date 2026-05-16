package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/wendi/pulseguard/internal/domain"
)

// TemplateRepo persists tenant message templates.
type TemplateRepo struct {
	db    *sql.DB
	clock domain.Clock
}

// NewTemplateRepo binds the repo to a DB handle and clock.
func NewTemplateRepo(db *sql.DB, clock domain.Clock) *TemplateRepo {
	return &TemplateRepo{db: db, clock: clock}
}

// Insert writes a new template row.
func (r *TemplateRepo) Insert(ctx context.Context, t *domain.Template) error {
	if err := validateTemplate(t); err != nil {
		return err
	}
	now := nowMs(r.clock)
	res, err := r.db.ExecContext(ctx, `
		INSERT INTO templates (tenant_id, name, parse_mode, body, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		t.TenantID, t.Name, string(t.ParseMode), t.Body, now, now)
	if err != nil {
		return fmt.Errorf("insert template: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return fmt.Errorf("last insert id: %w", err)
	}
	t.ID = id
	t.CreatedAt = toTime(now)
	t.UpdatedAt = toTime(now)
	return nil
}

// Update mutates an existing template owned by the same tenant.
func (r *TemplateRepo) Update(ctx context.Context, t *domain.Template) error {
	if err := validateTemplate(t); err != nil {
		return err
	}
	if t.ID == 0 {
		return fmt.Errorf("%w: template id is zero", domain.ErrValidation)
	}
	now := nowMs(r.clock)
	res, err := r.db.ExecContext(ctx, `
		UPDATE templates SET name = ?, parse_mode = ?, body = ?, updated_at = ?
		 WHERE id = ? AND tenant_id = ?`,
		t.Name, string(t.ParseMode), t.Body, now, t.ID, t.TenantID)
	if err != nil {
		return fmt.Errorf("update template: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return domain.ErrNotFound
	}
	t.UpdatedAt = toTime(now)
	return nil
}

// Delete removes a template owned by tenantID.
func (r *TemplateRepo) Delete(ctx context.Context, tenantID, id int64) error {
	res, err := r.db.ExecContext(ctx,
		`DELETE FROM templates WHERE id = ? AND tenant_id = ?`, id, tenantID)
	if err != nil {
		return fmt.Errorf("delete template: %w", err)
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

// GetByID fetches a template scoped to a tenant.
func (r *TemplateRepo) GetByID(ctx context.Context, tenantID, id int64) (*domain.Template, error) {
	row := r.db.QueryRowContext(ctx, `
		SELECT id, tenant_id, name, parse_mode, body, created_at, updated_at
		  FROM templates WHERE id = ? AND tenant_id = ?`, id, tenantID)
	return scanTemplate(row)
}

// ListByTenant returns every template of a tenant ordered by id.
func (r *TemplateRepo) ListByTenant(ctx context.Context, tenantID int64) ([]*domain.Template, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, tenant_id, name, parse_mode, body, created_at, updated_at
		  FROM templates WHERE tenant_id = ? ORDER BY id ASC`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list templates: %w", err)
	}
	defer rows.Close()
	var out []*domain.Template
	for rows.Next() {
		t, err := scanTemplate(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func scanTemplate(s interface {
	Scan(dest ...any) error
}) (*domain.Template, error) {
	t := &domain.Template{}
	var parseMode string
	var createdMs, updatedMs int64
	err := s.Scan(&t.ID, &t.TenantID, &t.Name, &parseMode, &t.Body, &createdMs, &updatedMs)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan template: %w", err)
	}
	t.ParseMode = domain.ParseMode(parseMode)
	t.CreatedAt = toTime(createdMs)
	t.UpdatedAt = toTime(updatedMs)
	return t, nil
}

func validateTemplate(t *domain.Template) error {
	if t == nil {
		return errors.New("template is nil")
	}
	if t.TenantID == 0 {
		return fmt.Errorf("%w: template tenant_id is zero", domain.ErrValidation)
	}
	if t.Name == "" {
		return fmt.Errorf("%w: template name is empty", domain.ErrValidation)
	}
	if t.Body == "" {
		return fmt.Errorf("%w: template body is empty", domain.ErrValidation)
	}
	if t.ParseMode == "" {
		t.ParseMode = domain.ParseMarkdownV2
	}
	switch t.ParseMode {
	case domain.ParseMarkdownV2, domain.ParseHTML, domain.ParseNone:
	default:
		return fmt.Errorf("%w: invalid parse_mode %q", domain.ErrValidation, t.ParseMode)
	}
	return nil
}

// Ensure interface compliance at compile time.
var _ domain.TemplateRepo = (*TemplateRepo)(nil)

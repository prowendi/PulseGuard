package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/wendi/pulseguard/internal/domain"
)

// TenantRepo persists tenants in SQLite.
type TenantRepo struct {
	db    *sql.DB
	clock domain.Clock
}

// NewTenantRepo returns a TenantRepo bound to the given DB and clock.
func NewTenantRepo(db *sql.DB, clock domain.Clock) *TenantRepo {
	return &TenantRepo{db: db, clock: clock}
}

// Insert writes a new tenant row. CreatedAt/UpdatedAt are taken from the
// clock; the inserted row's ID is written back into t.
func (r *TenantRepo) Insert(ctx context.Context, t *domain.Tenant) error {
	return r.InsertTx(ctx, r.db, t)
}

// txExec is the subset of *sql.Tx / *sql.DB / *sql.Conn that the Tx
// helpers rely on. Allows the Register flow to share a single
// transaction across repos.
type txExec interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// InsertTx is the explicit-transaction variant of Insert. Callers
// running multi-repo invariants (auth.Register uses tenant + invite +
// session inserts in one tx) pass in the active *sql.Tx so the rows
// commit or rollback atomically.
func (r *TenantRepo) InsertTx(ctx context.Context, tx txExec, t *domain.Tenant) error {
	if t == nil {
		return errors.New("tenant is nil")
	}
	if t.Email == "" {
		return fmt.Errorf("%w: tenant email is empty", domain.ErrValidation)
	}
	if t.PasswordHash == "" {
		return fmt.Errorf("%w: tenant password_hash is empty", domain.ErrValidation)
	}
	if t.Role == "" {
		t.Role = domain.RoleUser
	}
	if t.Status == "" {
		t.Status = domain.TenantActive
	}
	now := nowMs(r.clock)
	res, err := tx.ExecContext(ctx, `
		INSERT INTO tenants
		  (email, password_hash, display_name, role, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		t.Email, t.PasswordHash, t.DisplayName, string(t.Role), string(t.Status), now, now)
	if err != nil {
		return fmt.Errorf("insert tenant: %w", err)
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

// GetByEmail returns the tenant with the matching email or ErrNotFound.
func (r *TenantRepo) GetByEmail(ctx context.Context, email string) (*domain.Tenant, error) {
	return r.queryOneTx(ctx, r.db, `
		SELECT id, email, password_hash, display_name, role, status, created_at, updated_at
		  FROM tenants WHERE email = ?`, email)
}

// GetByEmailTx is the explicit-transaction variant — needed by
// auth.Register to check for duplicate emails inside the same write
// transaction as the insert.
func (r *TenantRepo) GetByEmailTx(ctx context.Context, tx txExec, email string) (*domain.Tenant, error) {
	return r.queryOneTx(ctx, tx, `
		SELECT id, email, password_hash, display_name, role, status, created_at, updated_at
		  FROM tenants WHERE email = ?`, email)
}

// GetByID returns the tenant with the matching id or ErrNotFound.
func (r *TenantRepo) GetByID(ctx context.Context, id int64) (*domain.Tenant, error) {
	return r.queryOneTx(ctx, r.db, `
		SELECT id, email, password_hash, display_name, role, status, created_at, updated_at
		  FROM tenants WHERE id = ?`, id)
}

// CountActive returns the number of tenants in the 'active' status.
func (r *TenantRepo) CountActive(ctx context.Context) (int, error) {
	var n int
	err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM tenants WHERE status = ?`, string(domain.TenantActive),
	).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count active: %w", err)
	}
	return n, nil
}

func (r *TenantRepo) queryOneTx(ctx context.Context, tx txExec, q string, args ...any) (*domain.Tenant, error) {
	t := &domain.Tenant{}
	var role, status string
	var createdMs, updatedMs int64
	err := tx.QueryRowContext(ctx, q, args...).Scan(
		&t.ID, &t.Email, &t.PasswordHash, &t.DisplayName,
		&role, &status, &createdMs, &updatedMs,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan tenant: %w", err)
	}
	t.Role = domain.Role(role)
	t.Status = domain.TenantStatus(status)
	t.CreatedAt = toTime(createdMs)
	t.UpdatedAt = toTime(updatedMs)
	return t, nil
}

// Ensure interface compliance at compile time.
var _ domain.TenantRepo = (*TenantRepo)(nil)

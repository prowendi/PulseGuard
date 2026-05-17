// Package bootstrap initialises the application's idempotent first-run
// state: when the tenants table is empty it creates the configured
// initial admin so the operator can log in and issue invites. It is a
// no-op on any database that already has tenants — never silently
// modifies an existing deployment.
package bootstrap

import (
	"context"
	"errors"
	"fmt"

	"github.com/prowendi/PulseGuard/internal/auth"
	"github.com/prowendi/PulseGuard/internal/config"
	"github.com/prowendi/PulseGuard/internal/domain"
)

// BootstrapRepos is the minimal repo slice required by EnsureInitialAdmin.
// We deliberately do NOT depend on the full repo bundle — bootstrap only
// touches tenants and never invite_codes or sessions.
type BootstrapRepos struct {
	Tenants domain.TenantRepo
}

// EnsureInitialAdmin ensures a usable admin tenant exists on first boot.
//
// Behaviour:
//   - tenants.CountActive > 0 → no-op (return nil). Bootstrap never
//     mutates a database that already has active tenants.
//   - tenants empty + cfg.InitialAdminEmail/Password both set →
//     bcrypt-hash the password (using security.BcryptCost) and INSERT
//     the admin tenant (Role=admin, Status=active).
//   - tenants empty + missing email or password → fail loud with a
//     dedicated error so the operator is forced to populate the config
//     before the first launch. We do NOT default any credentials.
//
// Calling this twice in a row is safe: the second call observes the
// previously-inserted admin via CountActive and returns nil.
func EnsureInitialAdmin(
	ctx context.Context,
	repos BootstrapRepos,
	cfg config.Bootstrap,
	security config.Security,
	clock domain.Clock,
) error {
	if repos.Tenants == nil {
		return errors.New("bootstrap: tenants repo is nil")
	}
	count, err := repos.Tenants.CountActive(ctx)
	if err != nil {
		return fmt.Errorf("bootstrap: count active tenants: %w", err)
	}
	if count > 0 {
		// Existing deployment: do not touch.
		return nil
	}
	if cfg.InitialAdminEmail == "" || cfg.InitialAdminPassword == "" {
		return fmt.Errorf("bootstrap: initial admin email/password required when database is empty")
	}

	hash, err := auth.HashPassword(cfg.InitialAdminPassword, security.BcryptCost)
	if err != nil {
		return fmt.Errorf("bootstrap: hash admin password: %w", err)
	}
	_ = clock // reserved: future audit field; tenant repo derives its
	// own CreatedAt from its bound clock.

	admin := &domain.Tenant{
		Email:        cfg.InitialAdminEmail,
		PasswordHash: string(hash),
		Role:         domain.RoleAdmin,
		Status:       domain.TenantActive,
	}
	if err := repos.Tenants.Insert(ctx, admin); err != nil {
		return fmt.Errorf("bootstrap: insert admin tenant: %w", err)
	}
	return nil
}

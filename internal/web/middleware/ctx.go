// Package middleware bundles the chi-compatible middleware used by the
// PulseGuard web layer (recover, logger, auth, admin, rate limit, ctx
// helpers).
package middleware

import (
	"context"

	"github.com/prowendi/PulseGuard/internal/domain"
)

type ctxKey int

const (
	ctxKeyTenant ctxKey = iota
	ctxKeySession
)

// WithTenant returns a child ctx carrying the authenticated tenant.
func WithTenant(ctx context.Context, t *domain.Tenant) context.Context {
	return context.WithValue(ctx, ctxKeyTenant, t)
}

// Tenant returns the tenant stored in ctx by the auth middleware, or nil
// when the ctx is unauthenticated. Handlers behind RequireAuth can rely
// on this being non-nil; public handlers must nil-check.
func Tenant(ctx context.Context) *domain.Tenant {
	t, _ := ctx.Value(ctxKeyTenant).(*domain.Tenant)
	return t
}

// WithSession attaches the active session record to ctx.
func WithSession(ctx context.Context, s *domain.Session) context.Context {
	return context.WithValue(ctx, ctxKeySession, s)
}

// Session returns the session stored in ctx, or nil when none.
func Session(ctx context.Context) *domain.Session {
	s, _ := ctx.Value(ctxKeySession).(*domain.Session)
	return s
}

package auth

import (
	"context"
	"fmt"
	"time"

	"github.com/wendi/pulseguard/internal/domain"
)

// GenerateInvite creates a new invite code owned by adminID, optionally
// expiring after ttl. ttl<=0 means the invite never expires.
//
// The returned code is 32 url-safe base64 characters (no padding) — the
// same alphabet used for session ids and push tokens.
func (s *Service) GenerateInvite(ctx context.Context, adminID int64, ttl time.Duration) (*domain.InviteCode, error) {
	if adminID == 0 {
		return nil, fmt.Errorf("%w: admin id is zero", domain.ErrValidation)
	}
	code, err := randomToken(24)
	if err != nil {
		return nil, err
	}
	inv := &domain.InviteCode{
		Code:      code,
		CreatedBy: adminID,
	}
	if ttl > 0 {
		exp := s.clock.Now().Add(ttl)
		inv.ExpiresAt = &exp
	}
	if err := s.invites.Insert(ctx, inv); err != nil {
		return nil, fmt.Errorf("insert invite: %w", err)
	}
	return inv, nil
}

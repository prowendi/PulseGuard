package domain

import "errors"

var (
	ErrNotFound        = errors.New("not found")
	ErrUnauthorized    = errors.New("unauthorized")
	ErrForbidden       = errors.New("forbidden")
	ErrConflict        = errors.New("conflict")
	ErrValidation      = errors.New("validation")
	ErrRateLimited     = errors.New("rate_limited")
	ErrChannelDisabled = errors.New("channel_disabled")
	ErrInviteInvalid   = errors.New("invite_invalid")
	ErrInternal        = errors.New("internal")
)

package domain

import "time"

type Role string

const (
	RoleUser  Role = "user"
	RoleAdmin Role = "admin"
)

type TenantStatus string

const (
	TenantActive   TenantStatus = "active"
	TenantDisabled TenantStatus = "disabled"
)

type Tenant struct {
	ID           int64
	Email        string
	PasswordHash string
	DisplayName  string
	Role         Role
	Status       TenantStatus
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

type InviteCode struct {
	Code      string
	CreatedBy int64
	UsedBy    *int64
	ExpiresAt *time.Time
	UsedAt    *time.Time
	CreatedAt time.Time
}

type Session struct {
	ID        string
	TenantID  int64
	ExpiresAt time.Time
	CreatedAt time.Time
}

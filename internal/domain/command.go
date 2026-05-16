package domain

import "time"

// Command is a tenant-owned Starlark script the user can invoke from
// any of that tenant's bots via `/<name>` in a Telegram chat. The
// runtime renders the result back into the same chat.
//
// Name is unique per tenant (the SQL UNIQUE(tenant_id, name)) and the
// listener resolves a typed slash command by (bot_id → tenant_id, name).
type Command struct {
	ID          int64
	TenantID    int64
	Name        string
	Description string
	Code        string
	Enabled     bool
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

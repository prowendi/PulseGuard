package domain

import "time"

// Command is a Starlark script bound to ONE specific bot. The script
// is invoked as `/<name> args...` in a chat with that bot; the runtime
// renders the result back into the same chat.
//
// Per-bot scoping (2026-05): commands used to be tenant-global, but
// operators wanted the same /name to behave differently on different
// bots (e.g. /查询 hitting bot A's database vs bot B's). The UNIQUE
// constraint is therefore (bot_id, name), and the listener resolves a
// typed slash command by (bot_id, name) without crossing into other
// bots of the same tenant. TenantID stays as a denormalised pointer
// for the UI list view; the load-bearing scope key is BotID.
type Command struct {
	ID          int64
	TenantID    int64
	BotID       int64
	Name        string
	Description string
	Code        string
	Enabled     bool
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

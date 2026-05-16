package domain

import "time"

// Subscriber records a (command, bot, chat_id) tuple that has invoked
// a custom command. Upsert keeps last_seen_at fresh so the UI can show
// "active chats" while never duplicating rows for the same chat.
//
// Platform defaults to "telegram" but stays explicit so a future
// Discord/Slack adapter can share the table without a migration.
type Subscriber struct {
	ID         int64
	TenantID   int64
	CommandID  int64
	BotID      int64
	ChatID     string
	Platform   string
	CreatedAt  time.Time
	LastSeenAt time.Time
}

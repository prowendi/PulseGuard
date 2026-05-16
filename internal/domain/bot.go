package domain

import "time"

type Bot struct {
	ID          int64
	TenantID    int64
	Name        string
	BotToken    string // plaintext (set after store-layer decryption)
	Description string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

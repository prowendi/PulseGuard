package domain

import "time"

type Channel struct {
	ID           int64
	TenantID     int64
	Name         string
	PushToken    string
	BotID        int64
	TemplateID   int64
	ChatID       string
	RatePerMin   int
	DedupWindowS int
	Enabled      bool
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

package domain

import "time"

type OutboxStatus string

const (
	OutboxPending  OutboxStatus = "pending"
	OutboxInFlight OutboxStatus = "in_flight"
	OutboxSent     OutboxStatus = "sent"
	OutboxRetry    OutboxStatus = "retry"
	OutboxDead     OutboxStatus = "dead"
)

type PushOutbox struct {
	ID            int64
	ChannelID     int64
	TenantID      int64
	PayloadJSON   string
	DedupKey      *string
	Status        OutboxStatus
	Attempts      int
	NextAttemptAt time.Time
	LastError     *string
	WorkerID      *string
	ClaimedAt     *time.Time
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

type LogStatus string

const (
	LogSent   LogStatus = "sent"
	LogFailed LogStatus = "failed"
	LogDead   LogStatus = "dead"
)

type PushLog struct {
	ID           int64
	OutboxID     *int64
	ChannelID    int64
	TenantID     int64
	PayloadJSON  string
	RenderedText string
	TGMessageID  *int64
	Status       LogStatus
	Error        *string
	Attempts     int
	CreatedAt    time.Time
}

type DeadLetter struct {
	ID           int64
	OutboxID     int64
	ChannelID    int64
	TenantID     int64
	PayloadJSON  string
	RenderedText *string
	LastError    string
	Attempts     int
	CreatedAt    time.Time
}

type PushRequest struct {
	ChannelID int64
	TenantID  int64
	Payload   map[string]any
	DedupKey  string // optional, from payload.dedup_key
}

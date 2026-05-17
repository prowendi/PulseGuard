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
	LogSent     LogStatus = "sent"
	LogFailed   LogStatus = "failed"
	LogDead     LogStatus = "dead"
	LogSilenced LogStatus = "silenced" // V7-3: matched an active silence
	LogEdited   LogStatus = "edited"   // V7-2: collapsed into an existing message_thread
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
	Buttons   []PushButton
}

// PushButton is the domain shape carried inside a PushRequest /
// pipeline payload via the `_buttons` JSON convention. The worker
// projects it into tg.InlineButton when calling SendWithOpts so the
// outbound Telegram message gets a single-row inline keyboard.
//
// Exactly one of Callback or URL should be set per button:
//   - Callback fires a Telegram callback_query update; V7-1 uses the
//     "ack:<fingerprint>" convention to drive the alert_acks insert
//     and the editMessageText echo from the listener.
//   - URL opens a browser tab in the user's Telegram client.
type PushButton struct {
	Text     string `json:"text"`
	Callback string `json:"callback,omitempty"`
	URL      string `json:"url,omitempty"`
}

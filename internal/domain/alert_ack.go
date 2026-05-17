package domain

import "time"

// AlertAck records that an operator acknowledged an alert (identified
// by its fingerprint) via the Telegram /ack built-in. The push
// pipeline will, in a future sprint, consult this table to skip
// already-acked alerts; for now V6-3 only ships the audit trail.
//
// (TenantID, Fingerprint) is unique — duplicate /ack on the same
// fingerprint collapses at the SQL layer.
type AlertAck struct {
	ID          int64
	TenantID    int64
	Fingerprint string
	AckedBy     string // TG username or chat_id when no username
	AckedAt     time.Time
	BotID       int64
	ChatID      string
}

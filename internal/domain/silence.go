package domain

import "time"

// Silence is a time-windowed mute rule for a tenant. While at least one
// active silence row has a `Pattern` that is a prefix of an inbound
// alert's fingerprint, the worker logs the push as `LogSilenced` and
// stops without invoking the Sender.
//
// CreatedBy records who set the silence (Telegram @username or the
// terminal "chat:<chat_id>" fallback), used for the /silence_list
// reply so operators can attribute their teammates' mutes.
type Silence struct {
	ID        int64
	TenantID  int64
	Pattern   string
	CreatedBy string
	ExpiresAt time.Time
	CreatedAt time.Time
}

// Active reports whether the silence is still in effect at `now`. The
// boundary is inclusive of `now == ExpiresAt` so a silence set with
// duration 0 (degenerate, but possible via /silence pattern 0s) does
// not unblock the very alert that prompted it.
func (s *Silence) Active(now time.Time) bool {
	return !now.After(s.ExpiresAt)
}

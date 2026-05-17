package domain

import "time"

// ChannelTemplate is the per-channel binding to a template. IsDefault
// means "use this template when push API does not pass ?template= and
// no Condition matches". Exactly one binding per channel may carry
// IsDefault=true; the unique partial index in migration 0003 enforces
// this at the database layer.
//
// Condition is the optional `field op value` expression evaluated by
// internal/condeval against the incoming payload. An empty Condition
// makes the binding "default-eligible only" — it never auto-routes a
// payload; it only fires when its IsDefault flag is set. A non-empty
// Condition lets the worker pick this binding when the payload field
// satisfies the operator (e.g. `level eq critical`).
type ChannelTemplate struct {
	ChannelID  int64
	TemplateID int64
	IsDefault  bool
	SortOrder  int
	Condition  string
	CreatedAt  time.Time
}

// Channel is a tenant-owned push target — one push_token → one bot
// → one chat — plus zero-or-more bound templates the worker can pick
// from when rendering. The single-template legacy field (TemplateID)
// was removed in migration 0003; readers go through Templates.
type Channel struct {
	ID           int64
	TenantID     int64
	Name         string
	PushToken    string
	BotID        int64
	ChatID       string
	RatePerMin   int
	DedupWindowS int
	Enabled      bool
	CreatedAt    time.Time
	UpdatedAt    time.Time

	// Templates is the populated list of templates bound to this
	// channel, ordered by SortOrder ASC then TemplateID ASC. Empty
	// slice means no templates are bound (push will fail with
	// VALIDATION until the user attaches at least one).
	Templates []*ChannelTemplate
}

// DefaultTemplateID returns the template ID flagged IsDefault, or 0 if
// none are flagged (caller must surface a VALIDATION error). Callers
// that hold a *Channel with Templates pre-populated should always
// use this helper rather than searching the slice inline.
func (c *Channel) DefaultTemplateID() int64 {
	if c == nil {
		return 0
	}
	for _, ct := range c.Templates {
		if ct.IsDefault {
			return ct.TemplateID
		}
	}
	return 0
}

// HasTemplate reports whether templateID is bound to this channel.
// Used by the push handler to validate ?template=<name> requests
// against the channel's pool.
func (c *Channel) HasTemplate(templateID int64) bool {
	if c == nil {
		return false
	}
	for _, ct := range c.Templates {
		if ct.TemplateID == templateID {
			return true
		}
	}
	return false
}

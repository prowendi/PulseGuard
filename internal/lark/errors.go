package lark

import (
	"errors"
	"fmt"
)

// ErrBadWebhook is returned by Client.Send when the supplied botToken
// does not match the canonical Lark custom-bot webhook URL shape:
//
//	https://open.feishu.cn/open-apis/bot/v2/hook/<key>
//
// The worker treats this as a permanent client error (no retry, DLQ)
// because no amount of waiting will fix a malformed URL.
var ErrBadWebhook = errors.New("lark: bot token is not a valid webhook URL")

// Class buckets every outcome into one of two retry-relevant categories
// for the worker. Lark has no equivalent to Telegram's "user blocked
// the bot" semantics so we omit a separate PermanentClient/Server
// split — every permanent failure on the Lark side is a webhook /
// payload misconfiguration the operator must fix.
type Class int

const (
	// Transient errors should be retried with backoff: 5xx, 429,
	// network timeouts, unparseable bodies.
	Transient Class = iota
	// PermanentClient errors will never succeed without operator
	// intervention (revoked webhook, malformed payload, app-level
	// non-zero code). Worker DLQs the row.
	PermanentClient
)

// String returns the canonical class name for logging.
func (c Class) String() string {
	switch c {
	case Transient:
		return "transient"
	case PermanentClient:
		return "permanent_client"
	default:
		return "unknown"
	}
}

// APIError is the typed error Client.Send returns when the Lark webhook
// endpoint rejects the call. Workers branch on .Class to decide
// retry-vs-DLQ exactly the way they do for *tg.APIError today.
type APIError struct {
	Class       Class
	Code        int    // HTTP status, or Lark app-level code (e.g. 9499)
	Description string // human-readable diagnostic
}

// Error implements error.
func (e *APIError) Error() string {
	if e == nil {
		return "<nil lark.APIError>"
	}
	return fmt.Sprintf("lark api %s code=%d: %s", e.Class, e.Code, e.Description)
}

// IsTransient is a convenience predicate so callers do not need to
// import the Class type just to branch on retryability.
func (e *APIError) IsTransient() bool { return e.Class == Transient }

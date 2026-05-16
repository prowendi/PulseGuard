// Package tg implements the Telegram Bot API outbound client and the
// error classification taxonomy consumed by the push worker.
package tg

import (
	"encoding/json"
	"fmt"
	"time"
)

// Class buckets every outcome into one of three retry-relevant categories.
type Class int

const (
	// Transient errors should be retried with backoff: 5xx, 429,
	// network timeouts, unknown / unparseable failures.
	Transient Class = iota
	// PermanentClient errors will never succeed without a payload/chat
	// change (e.g. chat not found, bot blocked). Send to DLQ.
	PermanentClient
	// PermanentServer errors signal a credentials problem (401, 403
	// outside the blocked-by-user case). Send to DLQ.
	PermanentServer
)

// String returns the canonical class name for logging.
func (c Class) String() string {
	switch c {
	case Transient:
		return "transient"
	case PermanentClient:
		return "permanent_client"
	case PermanentServer:
		return "permanent_server"
	default:
		return "unknown"
	}
}

// APIError is the typed error returned by Send when Telegram replies with
// a non-2xx status or an explicit {ok:false} body. Workers branch on
// .Class to decide retry vs DLQ.
type APIError struct {
	Class       Class
	Code        int
	Description string
	RetryAfter  time.Duration
}

// Error implements error.
func (e *APIError) Error() string {
	if e == nil {
		return "<nil tg.APIError>"
	}
	if e.RetryAfter > 0 {
		return fmt.Sprintf("tg api %s code=%d retry_after=%s: %s",
			e.Class, e.Code, e.RetryAfter, e.Description)
	}
	return fmt.Sprintf("tg api %s code=%d: %s", e.Class, e.Code, e.Description)
}

// IsTransient is a convenience predicate.
func (e *APIError) IsTransient() bool { return e.Class == Transient }

// tgRespEnvelope mirrors the Telegram error JSON layout:
//
//	{ "ok": false, "error_code": 400, "description": "...",
//	  "parameters": {"retry_after": 5} }
type tgRespEnvelope struct {
	OK          bool   `json:"ok"`
	ErrorCode   int    `json:"error_code"`
	Description string `json:"description"`
	Parameters  *struct {
		RetryAfter int `json:"retry_after"`
	} `json:"parameters,omitempty"`
}

// Classify maps an HTTP status + raw body to an *APIError. Pass body=nil
// for network errors where there is no response payload (caller should
// build a Transient APIError manually in that case).
//
// Rules:
//   - 2xx: returns nil (caller decides success).
//   - 429: Transient with RetryAfter populated.
//   - 5xx: Transient.
//   - 401: PermanentServer (token invalid).
//   - 403: PermanentClient when description hints at "blocked"; otherwise PermanentServer.
//   - 400: PermanentClient (chat not found, bad payload, etc.).
//   - other 4xx: PermanentClient (conservative).
//   - unparseable body: Transient (we will not give up on garbage we cannot read).
func Classify(httpStatus int, body []byte) error {
	if httpStatus >= 200 && httpStatus < 300 {
		return nil
	}
	env := tgRespEnvelope{}
	parsed := true
	if len(body) > 0 {
		if err := json.Unmarshal(body, &env); err != nil {
			parsed = false
		}
	} else {
		parsed = false
	}
	if !parsed {
		// Status known but body unreadable -> conservative Transient.
		return &APIError{
			Class:       Transient,
			Code:        httpStatus,
			Description: "unparseable telegram response",
		}
	}

	desc := env.Description
	code := env.ErrorCode
	if code == 0 {
		code = httpStatus
	}

	switch {
	case httpStatus == 429:
		ra := time.Duration(0)
		if env.Parameters != nil && env.Parameters.RetryAfter > 0 {
			ra = time.Duration(env.Parameters.RetryAfter) * time.Second
		}
		return &APIError{Class: Transient, Code: code, Description: desc, RetryAfter: ra}
	case httpStatus >= 500:
		return &APIError{Class: Transient, Code: code, Description: desc}
	case httpStatus == 401:
		return &APIError{Class: PermanentServer, Code: code, Description: desc}
	case httpStatus == 403:
		if containsBlockedHint(desc) {
			return &APIError{Class: PermanentClient, Code: code, Description: desc}
		}
		return &APIError{Class: PermanentServer, Code: code, Description: desc}
	case httpStatus >= 400 && httpStatus < 500:
		return &APIError{Class: PermanentClient, Code: code, Description: desc}
	default:
		// Should not happen (1xx/3xx are caller-handled), but stay safe.
		return &APIError{Class: Transient, Code: code, Description: desc}
	}
}

func containsBlockedHint(s string) bool {
	// Telegram 403 message variants we treat as client-side permanent.
	for _, needle := range []string{
		"bot was blocked by the user",
		"bot was kicked",
		"user is deactivated",
		"chat not found",
	} {
		if substringIgnoreCase(s, needle) {
			return true
		}
	}
	return false
}

func substringIgnoreCase(s, sub string) bool {
	if len(sub) == 0 {
		return true
	}
	if len(s) < len(sub) {
		return false
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		match := true
		for j := 0; j < len(sub); j++ {
			a := s[i+j]
			b := sub[j]
			if a >= 'A' && a <= 'Z' {
				a += 'a' - 'A'
			}
			if b >= 'A' && b <= 'Z' {
				b += 'a' - 'A'
			}
			if a != b {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

package pipeline

import "time"

// Backoff holds a fixed-length retry schedule and the maximum number of
// attempts before a push is sent to the dead-letter queue.
//
// The schedule is consulted by attempt number — NextDelay(1) returns the
// first entry, NextDelay(2) returns the second, etc. Once attempt >=
// MaxAttempts the worker stops retrying and marks the row dead.
type Backoff struct {
	Schedule    []time.Duration
	MaxAttempts int
}

// DefaultBackoff matches spec §4.1: [1s, 5s, 15s, 60s, 5m, 15m] with
// max_attempts=6.
func DefaultBackoff() Backoff {
	return Backoff{
		Schedule: []time.Duration{
			1 * time.Second,
			5 * time.Second,
			15 * time.Second,
			60 * time.Second,
			5 * time.Minute,
			15 * time.Minute,
		},
		MaxAttempts: 6,
	}
}

// NextDelay returns the delay to wait before attempt #attempt (1-based:
// attempt=1 is the first retry after the initial send).
//
// The second return value is true when attempt has reached MaxAttempts —
// the worker should stop retrying and move the push to the dead-letter
// queue. In that case the delay is zero.
//
// If the schedule is shorter than MaxAttempts, the final entry is reused
// for every "missing" slot (clamped to the tail).
func (b Backoff) NextDelay(attempt int) (time.Duration, bool) {
	if attempt < 1 {
		attempt = 1
	}
	max := b.MaxAttempts
	if max <= 0 {
		max = len(b.Schedule)
	}
	if attempt >= max {
		return 0, true
	}
	if len(b.Schedule) == 0 {
		return 0, false
	}
	idx := attempt - 1
	if idx >= len(b.Schedule) {
		idx = len(b.Schedule) - 1
	}
	return b.Schedule[idx], false
}

package store

import (
	"time"

	"github.com/prowendi/PulseGuard/internal/domain"
)

// nowMs returns the current millisecond Unix timestamp from the clock.
func nowMs(c domain.Clock) int64 { return c.Now().UnixMilli() }

// toTime turns a millisecond Unix timestamp into a UTC time.Time.
func toTime(ms int64) time.Time { return time.UnixMilli(ms).UTC() }

// toTimePtr nil-safe variant for nullable INTEGER columns.
func toTimePtr(ms *int64) *time.Time {
	if ms == nil {
		return nil
	}
	t := toTime(*ms)
	return &t
}

// fromTimePtr converts an optional time.Time to its millisecond pointer.
func fromTimePtr(t *time.Time) *int64 {
	if t == nil {
		return nil
	}
	v := t.UnixMilli()
	return &v
}

package middleware

import (
	"net/http"
	"time"

	"github.com/go-chi/httprate"
)

// RateLimit applies an IP-level limit of N requests per second. It is
// intended for the /api/* surface only; the UI is exempt because it is
// already gated by sessions. Pass 0 to disable.
func RateLimit(rps int) func(http.Handler) http.Handler {
	if rps <= 0 {
		return func(next http.Handler) http.Handler { return next }
	}
	return httprate.LimitByIP(rps, time.Second)
}

// AuthRateLimit returns a per-IP limiter sized for credential-bearing
// endpoints (login, register). The defaults are deliberately tight —
// 5 requests / minute — so a brute-force attacker hits a 429 long
// before they can sweep a meaningful password dictionary. The httprate
// limiter emits a Retry-After header on 429 responses.
//
// Pass non-positive values to fall back to the documented defaults
// (5 reqs / minute) instead of disabling the limiter — a disabled
// auth limiter is never the right answer.
//
// Refs: round2-security-report S-L4.
func AuthRateLimit(limit int, window time.Duration) func(http.Handler) http.Handler {
	if limit <= 0 {
		limit = 5
	}
	if window <= 0 {
		window = time.Minute
	}
	return httprate.LimitByIP(limit, window)
}

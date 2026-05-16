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

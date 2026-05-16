package middleware

import (
	"log/slog"
	"net/http"
	"runtime/debug"
)

// Recover converts panics into 500 responses + slog records. It MUST sit
// at the top of every chi router so subsequent middleware and handlers
// can panic without crashing the process.
func Recover(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rv := recover(); rv != nil {
					if logger != nil {
						logger.Error("panic recovered",
							"err", rv,
							"path", r.URL.Path,
							"stack", string(debug.Stack()))
					}
					http.Error(w, "internal server error", http.StatusInternalServerError)
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

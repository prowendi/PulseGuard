package middleware

import (
	"log/slog"
	"net/http"
	"time"
)

// statusRecorder wraps ResponseWriter to capture the status code.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(p []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(p)
	s.bytes += n
	return n, err
}

// Logger emits one slog record per HTTP request with status, duration,
// method, path, and tenant id (when authenticated). Sensitive headers
// (auth, cookie) are intentionally not logged.
func Logger(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w}
			next.ServeHTTP(rec, r)
			if logger == nil {
				return
			}
			attrs := []any{
				"method", r.Method,
				"path", r.URL.Path,
				"status", rec.status,
				"bytes", rec.bytes,
				"dur_ms", time.Since(start).Milliseconds(),
				"remote", r.RemoteAddr,
			}
			if t := Tenant(r.Context()); t != nil {
				attrs = append(attrs, "tenant_id", t.ID)
			}
			logger.Info("http", attrs...)
		})
	}
}

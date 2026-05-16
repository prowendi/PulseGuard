package middleware

import (
	"log/slog"
	"net/http"
	"time"

	chimid "github.com/go-chi/chi/v5/middleware"
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
// method, path, tenant id (when authenticated), and request_id. The
// request_id is harvested from the response header set by chi's
// RequestID middleware (which must run before us) so operators can
// correlate access-log lines with the JSON error envelope's
// request_id field. Sensitive headers (auth, cookie) are intentionally
// not logged.
//
// Refs: code-review-report C-I8.
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
			// chi.RequestID stores the id in ctx (NOT the response
			// header); chimid.GetReqID is the canonical accessor. Fall
			// back to the X-Request-Id response header in case some
			// other middleware put it there.
			rid := chimid.GetReqID(r.Context())
			if rid == "" {
				rid = w.Header().Get("X-Request-Id")
			}
			if rid != "" {
				attrs = append(attrs, "request_id", rid)
			}
			if t := Tenant(r.Context()); t != nil {
				attrs = append(attrs, "tenant_id", t.ID)
			}
			logger.Info("http", attrs...)
		})
	}
}

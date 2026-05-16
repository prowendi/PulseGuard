package middleware

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	chimid "github.com/go-chi/chi/v5/middleware"
)

// TestLoggerEmitsRequestID asserts that the slog record produced for
// each request includes the chi RequestID middleware's id so operators
// can correlate access-log lines with the JSON error envelope's
// request_id field.
//
// Refs: code-review-report C-I8.
func TestLoggerEmitsRequestID(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	// capture the request id chi assigned by reading it from ctx inside
	// the leaf handler.
	var captured string
	leaf := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = chimid.GetReqID(r.Context())
		w.WriteHeader(http.StatusTeapot)
	})

	handler := chimid.RequestID(Logger(logger)(leaf))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/some/path", nil)
	handler.ServeHTTP(rec, req)

	if captured == "" {
		t.Fatalf("chi RequestID did not produce an id")
	}
	out := buf.String()
	if !strings.Contains(out, "request_id="+captured) {
		t.Fatalf("logger output missing request_id=%s; got: %s", captured, out)
	}
	if !strings.Contains(out, "status=418") {
		t.Fatalf("logger output missing status: %s", out)
	}
}

// TestLoggerOmitsRequestIDWhenAbsent guards against the regression
// where we accidentally emit `request_id=""` when chi RequestID is
// not installed (e.g. on the /healthz path that runs outside the
// middleware stack).
func TestLoggerOmitsRequestIDWhenAbsent(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	handler := Logger(logger)(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		},
	))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/no-rid", nil)
	handler.ServeHTTP(rec, req)

	out := buf.String()
	if strings.Contains(out, "request_id=") {
		t.Fatalf("expected no request_id attr when chi not present, got: %s", out)
	}
}

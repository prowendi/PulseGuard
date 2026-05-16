package logging

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
)

var sensitiveKeys = map[string]struct{}{
	"password":       {},
	"bot_token":      {},
	"push_token":     {},
	"master_key":     {},
	"master_key_b64": {},
	"session":        {},
	"cookie":         {},
	"secret":         {},
	"authorization":  {},
}

type redactHandler struct{ inner slog.Handler }

func (r redactHandler) Enabled(ctx context.Context, lvl slog.Level) bool {
	return r.inner.Enabled(ctx, lvl)
}

func (r redactHandler) Handle(ctx context.Context, rec slog.Record) error {
	newRec := slog.NewRecord(rec.Time, rec.Level, rec.Message, rec.PC)
	rec.Attrs(func(a slog.Attr) bool {
		newRec.AddAttrs(redact(a))
		return true
	})
	return r.inner.Handle(ctx, newRec)
}

func (r redactHandler) WithAttrs(as []slog.Attr) slog.Handler {
	out := make([]slog.Attr, len(as))
	for i, a := range as {
		out[i] = redact(a)
	}
	return redactHandler{inner: r.inner.WithAttrs(out)}
}

func (r redactHandler) WithGroup(name string) slog.Handler {
	return redactHandler{inner: r.inner.WithGroup(name)}
}

func redact(a slog.Attr) slog.Attr {
	if _, ok := sensitiveKeys[strings.ToLower(a.Key)]; ok {
		return slog.String(a.Key, "REDACTED")
	}
	return a
}

// New configures and returns the application logger writing to stdout.
func New(level, format string) *slog.Logger { return newForTest(os.Stdout, level, format) }

func newForTest(w io.Writer, level, format string) *slog.Logger {
	lvl := slog.LevelInfo
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	}
	opts := &slog.HandlerOptions{Level: lvl}
	var inner slog.Handler
	if strings.ToLower(format) == "text" {
		inner = slog.NewTextHandler(w, opts)
	} else {
		inner = slog.NewJSONHandler(w, opts)
	}
	return slog.New(redactHandler{inner: inner})
}

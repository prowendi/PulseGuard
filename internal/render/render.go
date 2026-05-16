package render

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"text/template"
	"time"

	"github.com/wendi/pulseguard/internal/domain"
)

// FuncMap is the set of helper functions exposed to every template. It is
// public so the web layer can also reuse it for UI previews.
var FuncMap = template.FuncMap{
	"escMD":   EscapeMarkdownV2,
	"escHTML": EscapeHTML,
	"upper":   strings.ToUpper,
	"lower":   strings.ToLower,
	// default returns d when v is nil/empty, otherwise v.
	// Usage: {{ default "n/a" .maybeMissing }}
	"default": func(d, v any) any {
		if v == nil {
			return d
		}
		if s, ok := v.(string); ok && s == "" {
			return d
		}
		return v
	},
}

// MaxOutputBytes caps the rendered output of a single template execution
// at 64 KiB. Templates that produce more are stopped mid-write and an
// error returned so a malicious template (e.g. nested-range explosion)
// cannot OOM the worker. Far exceeds any reasonable Telegram message
// (which is 4096 chars / ~12 KiB UTF-8).
const MaxOutputBytes = 64 * 1024

// DefaultTimeout is the wall-clock budget for a single Render call.
// Long-running templates are abandoned and the worker dispatches DLQ.
const DefaultTimeout = 2 * time.Second

// ErrOutputTooLarge is returned when a template's rendered output
// exceeds MaxOutputBytes. Permanent failure — the worker should DLQ.
var ErrOutputTooLarge = errors.New("template output exceeds limit")

// limitedWriter is an io.Writer that aborts Write with ErrOutputTooLarge
// once written-bytes exceeds Max. text/template.Execute checks Write's
// return value, so a short Write terminates execution promptly even
// inside a deeply nested {{range}}.
type limitedWriter struct {
	buf     strings.Builder
	written int
	max     int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.written+len(p) > l.max {
		// Soak up just enough to fill the cap so the partial output is
		// still inspectable in test diagnostics, then refuse.
		room := l.max - l.written
		if room > 0 {
			l.buf.Write(p[:room])
			l.written += room
		}
		return 0, ErrOutputTooLarge
	}
	l.buf.Write(p)
	l.written += len(p)
	return len(p), nil
}

// Render parses tpl.Body (text/template syntax — never html/template) with
// the standard FuncMap and executes it against payload. The returned text
// is suitable for direct Telegram sendMessage with tpl.ParseMode.
//
// The render runs with three production-safety guards:
//   - Option("missingkey=error") so a typo in a payload key returns an
//     error rather than rendering "<no value>" into a user-facing
//     message.
//   - A 64 KiB output cap via limitedWriter — defends against
//     accidentally exponential templates like nested {{range}}.
//   - A 2-second timeout governed by ctx; if the supplied ctx lacks a
//     deadline we layer DefaultTimeout on top. Execution runs in a
//     goroutine so we can abandon a runaway template without leaking
//     the writer (its output is already capped).
func Render(ctx context.Context, tpl *domain.Template, payload map[string]any) (string, error) {
	if tpl == nil {
		return "", fmt.Errorf("template is nil")
	}
	name := tpl.Name
	if name == "" {
		name = "anonymous"
	}
	t, err := template.New(name).Funcs(FuncMap).Option("missingkey=error").Parse(tpl.Body)
	if err != nil {
		return "", fmt.Errorf("parse template %q: %w", name, err)
	}

	if ctx == nil {
		ctx = context.Background()
	}
	execCtx, cancel := context.WithTimeout(ctx, DefaultTimeout)
	defer cancel()

	type result struct {
		out string
		err error
	}
	done := make(chan result, 1)
	lw := &limitedWriter{max: MaxOutputBytes}
	go func() {
		err := t.Execute(lw, payload)
		done <- result{out: lw.buf.String(), err: err}
	}()

	select {
	case r := <-done:
		if r.err != nil {
			if errors.Is(r.err, ErrOutputTooLarge) {
				return "", fmt.Errorf("execute template %q: %w", name, ErrOutputTooLarge)
			}
			return "", fmt.Errorf("execute template %q: %w", name, r.err)
		}
		return r.out, nil
	case <-execCtx.Done():
		// Goroutine will eventually run to completion or hit the cap; we
		// abandon it and return the timeout. The writer is bounded so it
		// cannot consume unbounded memory.
		return "", fmt.Errorf("execute template %q: %w", name, execCtx.Err())
	}
}

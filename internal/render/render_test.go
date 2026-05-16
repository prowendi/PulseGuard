package render

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wendi/pulseguard/internal/domain"
)

func TestEscapeMarkdownV2(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"hello", "hello"},
		{"a_b", `a\_b`},
		{"*bold*", `\*bold\*`},
		{"a.b", `a\.b`},
		{"a!b#c-d", `a\!b\#c\-d`},
		{"price=$10", `price\=$10`},
		{"a[b](c)", `a\[b\]\(c\)`},
		{"line\\back", `line\\back`},
		{"normal漢字", "normal漢字"},
	}
	for _, c := range cases {
		got := EscapeMarkdownV2(c.in)
		if got != c.want {
			t.Errorf("EscapeMarkdownV2(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestEscapeHTML(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"hello", "hello"},
		{"a<b>c", "a&lt;b&gt;c"},
		{"a&b", "a&amp;b"},
		{"<&>", "&lt;&amp;&gt;"},
		{`"quote'`, `"quote'`}, // quotes not escaped
	}
	for _, c := range cases {
		got := EscapeHTML(c.in)
		if got != c.want {
			t.Errorf("EscapeHTML(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRenderSimpleVariable(t *testing.T) {
	tpl := &domain.Template{Name: "t1", Body: `Hello {{ .name }}!`}
	out, err := Render(context.Background(), tpl, map[string]any{"name": "world"})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if out != "Hello world!" {
		t.Fatalf("got %q", out)
	}
}

func TestRenderConditional(t *testing.T) {
	body := `{{ if eq .level "critical" }}CRIT{{ else }}warn{{ end }} {{ .title }}`
	tpl := &domain.Template{Body: body}
	out, err := Render(context.Background(), tpl, map[string]any{"level": "critical", "title": "down"})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if out != "CRIT down" {
		t.Fatalf("got %q", out)
	}
	out, err = Render(context.Background(), tpl, map[string]any{"level": "info", "title": "ok"})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if out != "warn ok" {
		t.Fatalf("got %q", out)
	}
}

func TestRenderRange(t *testing.T) {
	tpl := &domain.Template{Body: `{{ range .items }}[{{ . }}]{{ end }}`}
	out, err := Render(context.Background(), tpl, map[string]any{"items": []any{"a", "b", "c"}})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if out != "[a][b][c]" {
		t.Fatalf("got %q", out)
	}
}

func TestRenderEscapePipe(t *testing.T) {
	tpl := &domain.Template{Body: `*{{ .title | escMD }}*`}
	out, err := Render(context.Background(), tpl, map[string]any{"title": "CPU (95%) is high!"})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	// '(', ')' and '!' are all MarkdownV2 reserved characters; '%' is not.
	if !strings.Contains(out, `\(95%\)`) {
		t.Fatalf("parens not escaped: %q", out)
	}
	if !strings.Contains(out, `high\!`) {
		t.Fatalf("bang not escaped: %q", out)
	}
}

func TestRenderHTMLPipe(t *testing.T) {
	tpl := &domain.Template{Body: `<b>{{ .title | escHTML }}</b>`}
	out, err := Render(context.Background(), tpl, map[string]any{"title": "a<b>c"})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if out != `<b>a&lt;b&gt;c</b>` {
		t.Fatalf("got %q", out)
	}
}

func TestRenderDefaultFunc(t *testing.T) {
	tpl := &domain.Template{Body: `{{ default "n/a" .maybe }}`}
	// With missingkey=error we must pre-declare the key as nil so the
	// default helper can do its job.
	out, err := Render(context.Background(), tpl, map[string]any{"maybe": nil})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if out != "n/a" {
		t.Fatalf("got %q", out)
	}
	out, err = Render(context.Background(), tpl, map[string]any{"maybe": "real"})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if out != "real" {
		t.Fatalf("got %q", out)
	}
}

func TestRenderUpperLower(t *testing.T) {
	tpl := &domain.Template{Body: `{{ .x | upper }} {{ .y | lower }}`}
	out, err := Render(context.Background(), tpl, map[string]any{"x": "abc", "y": "DEF"})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if out != "ABC def" {
		t.Fatalf("got %q", out)
	}
}

func TestRenderMissingFieldNowErrors(t *testing.T) {
	// With Option("missingkey=error") (added by R4) a typo in a payload
	// key is surfaced as an execution error instead of silently rendering
	// "<no value>" — the worker DLQs the row, the operator sees the bug.
	tpl := &domain.Template{Body: `Hello {{ .missing }}`}
	_, err := Render(context.Background(), tpl, map[string]any{})
	if err == nil {
		t.Fatalf("expected error for missing key")
	}
	if !strings.Contains(err.Error(), "missing") && !strings.Contains(err.Error(), "no entry") {
		t.Fatalf("unexpected error %v", err)
	}
}

func TestRenderParseError(t *testing.T) {
	tpl := &domain.Template{Body: `{{ if `}
	if _, err := Render(context.Background(), tpl, nil); err == nil {
		t.Fatalf("expected parse error")
	}
}

func TestRenderExecError(t *testing.T) {
	// Calling a method on a nil value triggers an execution error.
	tpl := &domain.Template{Body: `{{ .foo.bar }}`}
	_, err := Render(context.Background(), tpl, map[string]any{"foo": nil})
	if err == nil {
		t.Fatalf("expected execution error")
	}
}

func TestRenderNilTemplate(t *testing.T) {
	if _, err := Render(context.Background(), nil, nil); err == nil {
		t.Fatalf("expected error on nil template")
	}
}

func TestRenderCriticalAlertSample(t *testing.T) {
	body := `{{ if eq .level "critical" }}CRIT{{ else }}WARN{{ end }} *{{ .title | escMD }}*

Host: ` + "`{{ .host | escMD }}`" + `
Value: *{{ .value | escMD }}*`
	tpl := &domain.Template{ParseMode: domain.ParseMarkdownV2, Body: body}
	out, err := Render(context.Background(), tpl, map[string]any{
		"level": "critical",
		"title": "CPU High!",
		"host":  "db01.prod",
		"value": "95%",
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.HasPrefix(out, "CRIT *CPU High\\!*") {
		t.Fatalf("bad output:\n%s", out)
	}
	if !strings.Contains(out, "db01\\.prod") {
		t.Fatalf("host not escaped:\n%s", out)
	}
}

// TestRenderOutputCapTerminatesExponentialTemplate is the regression
// guard for S-H1: a maliciously crafted template that explodes via
// nested ranges must be aborted within the 64KiB output cap, NOT allowed
// to OOM the worker.
func TestRenderOutputCapTerminatesExponentialTemplate(t *testing.T) {
	// Each outer iteration concatenates 100 inner copies of "x". With
	// 1000 outer items that's ~100 KiB of output — well above 64 KiB.
	body := `{{ range .outer }}{{ range $i := $.inner }}{{ $i }}{{ end }}{{ end }}`
	tpl := &domain.Template{Body: body}
	outer := make([]any, 1000)
	inner := make([]any, 100)
	for i := range outer {
		outer[i] = i
	}
	for i := range inner {
		inner[i] = "x"
	}
	_, err := Render(context.Background(), tpl, map[string]any{"outer": outer, "inner": inner})
	if err == nil {
		t.Fatalf("expected ErrOutputTooLarge for explosive template")
	}
	if !errors.Is(err, ErrOutputTooLarge) {
		t.Fatalf("expected ErrOutputTooLarge, got %v", err)
	}
}

// TestRenderRespectsCallerTimeout proves the deadline-driven exit path.
// A template using {{range}} over a huge slice with a small per-item
// payload eventually completes — but if the caller's ctx is cancelled
// first, Render must return ctx.Err.
func TestRenderRespectsCallerTimeout(t *testing.T) {
	tpl := &domain.Template{Body: `{{ range .items }}{{ . }}{{ end }}`}
	items := make([]any, 100)
	for i := range items {
		items[i] = "ok"
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	// Give the deadline a moment to elapse before invoking Render so the
	// goroutine select sees a Done channel immediately.
	time.Sleep(time.Millisecond)
	_, err := Render(ctx, tpl, map[string]any{"items": items})
	if err == nil {
		t.Fatalf("expected timeout error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected DeadlineExceeded, got %v", err)
	}
}

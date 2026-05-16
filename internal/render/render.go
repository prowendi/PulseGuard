package render

import (
	"bytes"
	"fmt"
	"strings"
	"text/template"

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

// Render parses tpl.Body (text/template syntax — never html/template) with
// the standard FuncMap and executes it against payload. The returned text
// is suitable for direct Telegram sendMessage with tpl.ParseMode.
func Render(tpl *domain.Template, payload map[string]any) (string, error) {
	if tpl == nil {
		return "", fmt.Errorf("template is nil")
	}
	name := tpl.Name
	if name == "" {
		name = "anonymous"
	}
	t, err := template.New(name).Funcs(FuncMap).Parse(tpl.Body)
	if err != nil {
		return "", fmt.Errorf("parse template %q: %w", name, err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, payload); err != nil {
		return "", fmt.Errorf("execute template %q: %w", name, err)
	}
	return buf.String(), nil
}

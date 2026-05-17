package web

import (
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"reflect"
	"strings"
	"sync"

	pulseguard "github.com/wendi/pulseguard"
	"github.com/wendi/pulseguard/internal/domain"
)

// templatesFS returns the templates sub-tree of the embedded WebFS.
func templatesFS() fs.FS {
	sub, err := fs.Sub(pulseguard.WebFS, "web/templates")
	if err != nil {
		panic(fmt.Errorf("templates fs.Sub: %w", err))
	}
	return sub
}

// staticFS returns the static assets sub-tree.
func staticFS() fs.FS {
	sub, err := fs.Sub(pulseguard.WebFS, "web/static")
	if err != nil {
		panic(fmt.Errorf("static fs.Sub: %w", err))
	}
	return sub
}

var (
	tmplOnce sync.Once
	tmplSet  *template.Template
	tmplErr  error
)

// templates lazily parses every HTML file under web/templates (recursively).
// We use html/template (not text/template) here because UI output is
// HTML — the message-template engine in internal/render handles raw TG
// payloads separately.
//
// Naming convention: each page file declares a single {{ define "<slug>-page" }}
// block that opens the shared layout, drops its content, then closes the
// layout. Partials live in partials/ and are defined under their own names
// (e.g. {{ define "bot-row" }}). Render and RenderPartial select between
// the two by template name.
func templates() (*template.Template, error) {
	tmplOnce.Do(func() {
		root := template.New("__pulseguard__").Funcs(uiFuncs)
		fsys := templatesFS()
		var paths []string
		err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if d.IsDir() {
				return nil
			}
			paths = append(paths, p)
			return nil
		})
		if err != nil {
			tmplErr = fmt.Errorf("walk templates: %w", err)
			return
		}
		for _, p := range paths {
			body, err := fs.ReadFile(fsys, p)
			if err != nil {
				tmplErr = fmt.Errorf("read %s: %w", p, err)
				return
			}
			if _, err := root.Parse(string(body)); err != nil {
				tmplErr = fmt.Errorf("parse %s: %w", p, err)
				return
			}
		}
		tmplSet = root
	})
	return tmplSet, tmplErr
}

// navItem is a small projection passed to the sidebar's {{ template
// "navlink" }} partial. We build it via mkNavItem so the active state can
// be derived from pageData.Active without each call site doing the
// string comparison itself.
type navItem struct {
	Href   string
	Label  string
	Icon   string // raw SVG path "d" attribute
	Active bool
}

// kpiCard is the projection consumed by the dashboard's {{ template "kpi" }}.
type kpiCard struct {
	Label string
	Value int
	Hint  string
	Color string
	Icon  string
}

// emptyState is the projection for the empty-list illustration block.
//
// Two CTA shapes are supported and the template chooses based on
// CTAAction:
//   - CTAAction == ""   → plain <a href="{{.CTAHref}}">{{.CTALabel}}</a>
//   - CTAAction != ""   → <button data-action="…" data-target="…">…</button>
//     for opening drawers without resorting to javascript: URLs (which
//     are blocked by the strict CSP that disallows 'unsafe-inline').
type emptyState struct {
	Title     string
	Hint      string
	CTAHref   string
	CTALabel  string
	CTAAction string // e.g. "drawer-open"
	CTATarget string // e.g. "drawer-new-bot"
}

// uiFuncs is the helper set available inside HTMX templates.
var uiFuncs = template.FuncMap{
	"masked": func(s string) string {
		if len(s) <= 4 {
			return "***"
		}
		return "***" + s[len(s)-4:]
	},
	"add": func(a, b int) int { return a + b },
	"sub": func(a, b int) int { return a - b },
	// hasMore reports whether another page exists given the current
	// page (1-based), per-page size, and total count.
	"hasMore": func(page, perPage, total int) bool {
		return page*perPage < total
	},
	// mkNavItem builds a navItem the sidebar's {{ template "navlink" }}
	// partial consumes. pd carries the .Active slug so we can compare
	// once in Go rather than re-evaluating eq for every link inline.
	"mkNavItem": func(pd any, slug, href, label, iconPath string) navItem {
		active := activeFromAny(pd) == slug
		return navItem{Href: href, Label: label, Icon: iconPath, Active: active}
	},
	// avatarInitial returns the upper-case first rune of an email's
	// local part so the sidebar/topbar can render a coloured circle
	// without ever loading a Gravatar (privacy + offline-first).
	"avatarInitial": func(email string) string {
		s := strings.TrimSpace(email)
		if s == "" {
			return "?"
		}
		for _, r := range s {
			if r == '@' {
				return "?"
			}
			return strings.ToUpper(string(r))
		}
		return "?"
	},
	"mkKPI": func(label string, value int, hint, color, icon string) kpiCard {
		return kpiCard{Label: label, Value: value, Hint: hint, Color: color, Icon: icon}
	},
	"mkEmpty": func(title, hint, href, label string) emptyState {
		// CSP-strict shim: legacy callers pass
		// "javascript:psgOpenDrawer('drawer-x')" when the CTA should
		// open a drawer instead of navigating. That inline javascript:
		// URL is blocked under strict script-src; auto-detect it and
		// emit a button + data-action so the partial stays self-hosted.
		const prefix = "javascript:psgOpenDrawer('"
		if strings.HasPrefix(href, prefix) {
			rest := strings.TrimPrefix(href, prefix)
			if end := strings.Index(rest, "'"); end >= 0 {
				return emptyState{
					Title:     title,
					Hint:      hint,
					CTAAction: "drawer-open",
					CTATarget: rest[:end],
					CTALabel:  label,
				}
			}
		}
		return emptyState{Title: title, Hint: hint, CTAHref: href, CTALabel: label}
	},
	// trendHeights produces 7 deterministic but visually pleasant bar
	// percentages (10..100) seeded by the total push count so the chart
	// is not a flat zero-bar wall on quiet tenants while still being
	// reproducible across renders.
	"trendHeights": func(total int) []int {
		// Static "shape" weights — picked to look like a real time
		// series; the seed shifts amplitude proportional to traffic.
		shape := []int{55, 35, 70, 45, 80, 60, 95}
		amp := 0.35
		if total > 0 {
			amp = 0.55
			if total > 100 {
				amp = 0.85
			}
		}
		out := make([]int, 7)
		for i, w := range shape {
			h := int(float64(w) * amp)
			if h < 10 {
				h = 10
			}
			if h > 100 {
				h = 100
			}
			out[i] = h
		}
		return out
	},
	// channelBindingsJSON serialises a slice of *domain.ChannelTemplate
	// down to compact JSON for the channels edit drawer. The HTML
	// attribute escape that html/template applies makes this safe to
	// embed verbatim in data-bindings="…"; the edit handler in app.js
	// JSON.parse()s it back to pre-check the right rows / fill the
	// condition inputs of the edit drawer.
	"channelBindingsJSON": func(bs []*domain.ChannelTemplate) string {
		if len(bs) == 0 {
			return "[]"
		}
		type wire struct {
			TemplateID int64  `json:"template_id"`
			IsDefault  bool   `json:"is_default"`
			SortOrder  int    `json:"sort_order"`
			Condition  string `json:"condition"`
		}
		out := make([]wire, 0, len(bs))
		for _, b := range bs {
			if b == nil {
				continue
			}
			out = append(out, wire{
				TemplateID: b.TemplateID,
				IsDefault:  b.IsDefault,
				SortOrder:  b.SortOrder,
				Condition:  b.Condition,
			})
		}
		raw, err := json.Marshal(out)
		if err != nil {
			return "[]"
		}
		return string(raw)
	},
	// successRate counts how many of the recent log slice are "sent".
	// Returns an integer percentage rounded down. Empty slice → "100"
	// (vacuously healthy) so the KPI does not render "NaN".
	"successRate": func(rows any) int {
		rv := reflect.ValueOf(rows)
		if !rv.IsValid() || rv.Kind() != reflect.Slice || rv.Len() == 0 {
			return 100
		}
		total := rv.Len()
		ok := 0
		for i := 0; i < total; i++ {
			el := rv.Index(i)
			f := el.FieldByName("Status")
			if f.IsValid() && f.Kind() == reflect.String && f.String() == "sent" {
				ok++
			}
		}
		return ok * 100 / total
	},
}

// activeFromAny is a small reflective fallback used by mkNavItem when
// the caller's struct does not implement activeSlug(). Returns "" on
// any failure — that just means no link is highlighted, which is a
// graceful no-op rather than a render panic.
func activeFromAny(v any) string {
	rv := reflect.ValueOf(v)
	for rv.Kind() == reflect.Ptr {
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		return ""
	}
	f := rv.FieldByName("Active")
	if !f.IsValid() || f.Kind() != reflect.String {
		// Try the embedded pageData.Active path.
		for i := 0; i < rv.NumField(); i++ {
			ef := rv.Field(i)
			if ef.Kind() != reflect.Struct {
				continue
			}
			af := ef.FieldByName("Active")
			if af.IsValid() && af.Kind() == reflect.String {
				return af.String()
			}
		}
		return ""
	}
	return f.String()
}

// Render writes the named full-page template (typically "<slug>-page") with
// the supplied data. The data MUST embed the same fields the layout/nav
// templates consume (Title, Tenant, Active, CSRF, Flash) — usually via
// the pageData helper defined in pagedata.go.
func Render(w http.ResponseWriter, status int, pageName string, data any) error {
	t, err := templates()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return err
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	return t.ExecuteTemplate(w, pageName, data)
}

// RenderPartial writes a single defined block (e.g. partials/bot_row.html).
// HTMX handlers use this for hx-swap fragments.
func RenderPartial(w http.ResponseWriter, status int, name string, data any) error {
	t, err := templates()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return err
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	return t.ExecuteTemplate(w, name, data)
}

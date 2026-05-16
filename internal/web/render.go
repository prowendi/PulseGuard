package web

import (
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"sync"

	pulseguard "github.com/wendi/pulseguard"
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

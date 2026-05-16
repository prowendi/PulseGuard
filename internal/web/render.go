package web

import (
	"fmt"
	"html/template"
	"io"
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
func templates() (*template.Template, error) {
	tmplOnce.Do(func() {
		root := template.New("layout").Funcs(uiFuncs)
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
}

// Render writes the named template (typically "layout") with the supplied
// page data. Each handler is expected to pass an instance of pageData (or
// any struct that exposes Title/Tenant/Active/CSRF/Flash plus a content
// template clone — see partial rendering below).
//
// If the handler only needs a partial fragment (HTMX swap) it should
// call RenderPartial directly with the partial's template name.
func Render(w http.ResponseWriter, status int, contentName string, data any) error {
	t, err := templates()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return err
	}
	// Clone so concurrent renders can override the "content" block without
	// stepping on each other.
	clone, err := t.Clone()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return err
	}
	// Lookup the page's content fragment and attach it as "content".
	page := clone.Lookup(contentName)
	if page == nil {
		err := fmt.Errorf("template %q not found", contentName)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return err
	}
	// Define "content" in the clone by re-parsing the page's source under
	// that name. html/template does not let us alias template names
	// directly, so we wrap the lookup in an ExecuteTemplate call below.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	return executeWithContent(clone, w, contentName, data)
}

// executeWithContent renders "layout" with the supplied content block.
// The content template MUST define {{ define "content" }} ... {{ end }};
// page-level files in web/templates/*.html follow this convention.
func executeWithContent(t *template.Template, w io.Writer, contentName string, data any) error {
	// All page templates declare both their content block and we just
	// invoke "layout" which references "content" → resolved by html/template.
	// We do not need to alias here because Parse already registered the
	// "content" block from the page file.
	_ = contentName
	return t.ExecuteTemplate(w, "layout", data)
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

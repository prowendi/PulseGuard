// Package pulseguard exposes the embedded web assets (templates + static
// files) to the internal/web package via a parent-relative go:embed
// declaration. Living at the module root is the only way to embed the
// adjacent top-level web/ tree without duplicating it.
package pulseguard

import "embed"

// WebFS holds every published file under web/. The directives below
// enumerate templates and static assets explicitly so dotfiles,
// editor swap files (.env / .DS_Store / *~ / *.swp), and accidental
// secret drops never become reachable over /static/*.
//
// When adding a new template or static asset, add it here too — go:embed
// without "all:" is opt-in by design.
//
// Refs: security-report S-M3.
//
//go:embed web/templates/*.html
//go:embed web/templates/partials/*.html
//go:embed web/static/htmx.min.js web/static/app.css web/static/app.js
//go:embed web/static/tailwind.min.css web/static/template-editor.js
var WebFS embed.FS

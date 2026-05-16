// Package pulseguard exposes the embedded web assets (templates + static
// files) to the internal/web package via a parent-relative go:embed
// declaration. Living at the module root is the only way to embed the
// adjacent top-level web/ tree without duplicating it.
package pulseguard

import "embed"

// WebFS holds every file under web/ (templates and static assets). The
// internal/web package builds template and static sub-filesystems by
// calling fs.Sub on this root.
//
//go:embed all:web
var WebFS embed.FS

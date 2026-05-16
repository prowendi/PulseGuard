package web

import (
	"github.com/wendi/pulseguard/internal/domain"
)

// pageData is the shared shell every UI page passes to the templates.
// Auth/nav/flash templates all key off these fields; handlers compose
// page-specific data by embedding pageData into a wrapper struct.
type pageData struct {
	Title  string
	Tenant *domain.Tenant
	Active string
	CSRF   string
	Flash  *flash
}

type flash struct {
	Level   string // success / error / info
	Message string
}

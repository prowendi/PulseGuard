package web

import (
	"net/http"

	"github.com/wendi/pulseguard/internal/domain"
)

// pageData is the shared shell every UI page passes to the templates.
// Auth/nav/flash templates all key off these fields; handlers compose
// page-specific data by embedding pageData into a wrapper struct.
//
// Theme carries the user's explicit dark/light preference read from the
// psg-theme cookie (see themeFromRequest). It is "" when the user has
// not made a choice — in that case the page falls back to the OS
// preference via the @media (prefers-color-scheme: dark) rule shipped in
// app.css. When set to "dark" the layout opens <html class="dark"> so
// the per-utility overrides in app.css can paint the page on first
// paint, eliminating the legacy light→dark flash.
type pageData struct {
	Title  string
	Tenant *domain.Tenant
	Active string
	CSRF   string
	Flash  *flash
	Theme  string // "", "light", "dark"
}

type flash struct {
	Level   string // success / error / info
	Message string
}

// themeCookieName is the public cookie the user (via app.js theme-cycle
// handler) writes their explicit dark/light preference into. Kept
// un-prefixed (no __Host-) and SameSite=Lax so we can mutate it from
// JavaScript on both HTTP dev and HTTPS prod without coordinating with
// the cookie_secure flag — this cookie carries no security state.
const themeCookieName = "psg-theme"

// themeFromRequest reads psg-theme and normalises the value. Any value
// other than "light" / "dark" (including missing or malformed) returns
// "" so the caller treats the request as auto/system-follows. We never
// trust the cookie blindly — only the two known constants are allowed
// to influence the <html class="..."> attribute, defending against a
// hostile JS injection that tries to smuggle markup through the class
// list.
func themeFromRequest(r *http.Request) string {
	if r == nil {
		return ""
	}
	c, err := r.Cookie(themeCookieName)
	if err != nil || c == nil {
		return ""
	}
	switch c.Value {
	case "light", "dark":
		return c.Value
	default:
		return ""
	}
}

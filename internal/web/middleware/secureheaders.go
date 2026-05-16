package middleware

import "net/http"

// SecureHeaders sets a defensive baseline of response headers on every
// response. Defends against clickjacking, MIME sniffing, and Referer
// leakage; ships a strict-ish CSP that matches our self-hosted /static
// surface (htmx + app.css + app.js).
//
// HSTS is only set when secure=true so dev-mode HTTP does not pin
// browsers to https://localhost.
//
// Refs: security-report S-M1.
func SecureHeaders(secure bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("X-Frame-Options", "DENY")
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
			h.Set("Content-Security-Policy",
				"default-src 'self'; "+
					"script-src 'self'; "+
					"style-src 'self' 'unsafe-inline'; "+
					"img-src 'self' data:; "+
					"connect-src 'self'; "+
					"frame-ancestors 'none'; "+
					"base-uri 'self'; "+
					"form-action 'self'")
			if secure {
				h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
			}
			next.ServeHTTP(w, r)
		})
	}
}

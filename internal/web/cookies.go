package web

// cookies.go centralises the names PulseGuard uses for its session and
// CSRF cookies. The names are deliberately a function of the deployment
// posture: when the server is serving over HTTPS (cookie_secure == true)
// we prepend the `__Host-` prefix so browsers enforce the strict
// invariants documented in RFC 6265bis §4.1.3.2:
//
//   1. Secure attribute MUST be set (we always set it when CookieSecure
//      is on).
//   2. Path MUST be "/" (we always use "/").
//   3. Domain MUST NOT be set (we never set it).
//
// Browsers reject any Set-Cookie violating these constraints AND refuse
// to send a `__Host-` prefixed cookie from a sibling subdomain, closing
// the cookie-toss/cookie-fixation vector where a co-tenant on
// `*.example.com` plants a forged psg_session/psg_csrf cookie at the
// parent domain.
//
// In dev/test (CookieSecure == false) we keep the legacy plain names so
// browsers send them over plain HTTP — `__Host-` cookies are silently
// dropped by every major browser when delivered without Secure.
//
// Refs: round2-security-report S2-L2.

const (
	// sessionCookieBase is the suffix used regardless of prefix.
	sessionCookieBase = "psg_session"
	// csrfCookieBase is the suffix used regardless of prefix.
	csrfCookieBase = "psg_csrf"
	// hostPrefix is the browser-enforced cookie prefix.
	hostPrefix = "__Host-"
)

// SessionCookieName returns the session cookie name appropriate for the
// deployment. When secure is true the `__Host-` prefix is added; the
// caller MUST also set Secure=true + Path="/" + omit Domain or the
// browser will reject the Set-Cookie.
func SessionCookieName(secure bool) string {
	if secure {
		return hostPrefix + sessionCookieBase
	}
	return sessionCookieBase
}

// CSRFCookieName returns the CSRF cookie name appropriate for the
// deployment. Same constraints as SessionCookieName apply when secure
// is true.
func CSRFCookieName(secure bool) string {
	if secure {
		return hostPrefix + csrfCookieBase
	}
	return csrfCookieBase
}

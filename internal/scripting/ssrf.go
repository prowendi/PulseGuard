// Package scripting hosts the Starlark-based custom command executor.
// The package is intentionally small: an Executor wraps the Starlark
// interpreter with a tenant-safe sandbox, and an HTTPClient module is
// exposed when configured. The ssrf.go file isolates the network-policy
// surface so the safety claims have a single test target.
package scripting

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// ErrUnsafeHost is returned when a script tries to reach a host that
// falls under the SSRF deny list (loopback / link-local / private
// RFC1918 / RFC4193 / cloud metadata).
var ErrUnsafeHost = errors.New("scripting: unsafe host (SSRF guard)")

// ErrUnsupportedScheme is returned for URL schemes outside the
// HTTPClient.AllowedScheme allow-list (default http/https).
var ErrUnsupportedScheme = errors.New("scripting: unsupported URL scheme")

// metadataIPv4 is the AWS/GCP/Azure shared metadata endpoint. We single
// it out only for clearer telemetry — the link-local check already
// covers it.
var metadataIPv4 = net.ParseIP("169.254.169.254")

// CheckURL parses raw, validates its scheme, resolves the host, and
// returns the resolved IP set if every IP passes the SSRF policy.
// When denyPrivate is false the resolver still runs but no IP is
// rejected (test/dev only — production callers MUST pass true).
//
// Returns ErrUnsupportedScheme when the scheme is not in
// allowedSchemes, ErrUnsafeHost when resolution fails or any resolved
// IP is on the deny list, and the raw parse error otherwise.
func CheckURL(raw string, allowedSchemes []string, denyPrivate bool) (*url.URL, []net.IP, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, nil, fmt.Errorf("parse url: %w", err)
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, nil, fmt.Errorf("%w: %q", ErrUnsupportedScheme, raw)
	}
	if !schemeAllowed(u.Scheme, allowedSchemes) {
		return nil, nil, fmt.Errorf("%w: %q", ErrUnsupportedScheme, u.Scheme)
	}

	hostname := u.Hostname()
	if hostname == "" {
		return nil, nil, fmt.Errorf("%w: empty host", ErrUnsafeHost)
	}

	// Reject every form of unbracketed literal that resolves to a
	// denied IP. We resolve via net.LookupIP so the policy applies
	// to DNS-based bypasses too.
	ips, err := lookupIPs(hostname)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: lookup %s: %v", ErrUnsafeHost, hostname, err)
	}
	if denyPrivate {
		for _, ip := range ips {
			if isBlockedIP(ip) {
				return nil, nil, fmt.Errorf("%w: %s resolves to %s",
					ErrUnsafeHost, hostname, ip.String())
			}
		}
	}
	return u, ips, nil
}

// lookupIPs is a package-level indirection so tests can stub out DNS.
var lookupIPs = func(host string) ([]net.IP, error) {
	// If the host is already an IP literal, skip the resolver so the
	// caller cannot smuggle a denied IP through DNS hijacking races.
	if ip := net.ParseIP(host); ip != nil {
		return []net.IP{ip}, nil
	}
	return net.LookupIP(host)
}

func schemeAllowed(s string, allowed []string) bool {
	s = strings.ToLower(s)
	if len(allowed) == 0 {
		return s == "http" || s == "https"
	}
	for _, a := range allowed {
		if strings.EqualFold(a, s) {
			return true
		}
	}
	return false
}

// isBlockedIP returns true when ip should be denied. Decision tree:
//   - any unspecified address (0.0.0.0 / ::)              → deny
//   - loopback (127.0.0.0/8, ::1)                         → deny
//   - link-local (169.254.0.0/16, fe80::/10)              → deny
//   - private IPv4 (10/8, 172.16/12, 192.168/16) and the
//     IPv6 unique-local equivalent (fc00::/7)             → deny
//   - cloud metadata 169.254.169.254 is already covered
//     by the link-local rule but called out explicitly.
func isBlockedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsUnspecified() {
		return true
	}
	if ip.IsLoopback() {
		return true
	}
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return true
	}
	if ip.IsPrivate() {
		return true
	}
	if ip.IsInterfaceLocalMulticast() || ip.IsMulticast() {
		return true
	}
	// Defensive: explicit comparison to the AWS metadata address in
	// case some future stdlib loosens IsLinkLocalUnicast.
	if metadataIPv4 != nil && ip.Equal(metadataIPv4) {
		return true
	}
	return false
}

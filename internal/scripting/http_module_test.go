package scripting

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func newTestHTTPClient(allowPrivate bool) *HTTPClient {
	return &HTTPClient{
		BaseClient: &http.Client{
			Timeout:       3 * time.Second,
			CheckRedirect: blockRedirect,
		},
		MaxBodyBytes:    1 << 16,
		AllowedScheme:   []string{"http", "https"},
		DenyPrivateNets: !allowPrivate,
	}
}

func TestHTTP_GetMockedServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	// httptest.Server binds 127.0.0.1; pass allowPrivate=true so the
	// SSRF guard does not block the test traffic.
	c := newTestHTTPClient(true)
	e := &Executor{HTTP: c}
	res, err := e.Execute(context.Background(), `
def handle(args):
    r = http.get(args[0])
    return str(r["status"]) + ":" + r["body"]
`, []string{srv.URL})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.HasPrefix(res.Return, "200:") {
		t.Fatalf("Return = %q, want prefix 200:", res.Return)
	}
	if !strings.Contains(res.Return, `"ok":true`) {
		t.Fatalf("Return missing body, got %q", res.Return)
	}
}

func TestHTTP_PostMockedServer(t *testing.T) {
	var receivedBody string
	var receivedCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bs := make([]byte, 1024)
		n, _ := r.Body.Read(bs)
		receivedBody = string(bs[:n])
		receivedCT = r.Header.Get("Content-Type")
		w.WriteHeader(201)
		_, _ = w.Write([]byte("created"))
	}))
	defer srv.Close()

	c := newTestHTTPClient(true)
	e := &Executor{HTTP: c}
	res, err := e.Execute(context.Background(), `
def handle(args):
    r = http.post(args[0], "payload=1", {"Content-Type": "application/x-www-form-urlencoded"})
    return str(r["status"])
`, []string{srv.URL})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Return != "201" {
		t.Fatalf("Return = %q", res.Return)
	}
	if receivedBody != "payload=1" {
		t.Fatalf("server got body %q, want payload=1", receivedBody)
	}
	if receivedCT != "application/x-www-form-urlencoded" {
		t.Fatalf("server got Content-Type %q", receivedCT)
	}
}

// ─── SSRF — every blocked class fails ──────────────────────────────────
//
// Each subtest reaches an IP literal so DNS races cannot leak through.

func TestHTTP_SSRF_LoopbackIPv4Blocked(t *testing.T) {
	c := newTestHTTPClient(false)
	e := &Executor{HTTP: c}
	_, err := e.Execute(context.Background(), `
def handle(args):
    return http.get("http://127.0.0.1:9999")
`, nil)
	if err == nil || !errors.Is(err, ErrUnsafeHost) {
		t.Fatalf("err = %v, want ErrUnsafeHost", err)
	}
}

func TestHTTP_SSRF_LoopbackIPv6Blocked(t *testing.T) {
	c := newTestHTTPClient(false)
	e := &Executor{HTTP: c}
	_, err := e.Execute(context.Background(), `
def handle(args):
    return http.get("http://[::1]:9999")
`, nil)
	if err == nil || !errors.Is(err, ErrUnsafeHost) {
		t.Fatalf("err = %v, want ErrUnsafeHost", err)
	}
}

func TestHTTP_SSRF_LinkLocalBlocked(t *testing.T) {
	c := newTestHTTPClient(false)
	e := &Executor{HTTP: c}
	_, err := e.Execute(context.Background(), `
def handle(args):
    return http.get("http://169.254.169.254/latest/meta-data/")
`, nil)
	if err == nil || !errors.Is(err, ErrUnsafeHost) {
		t.Fatalf("err = %v, want ErrUnsafeHost", err)
	}
}

func TestHTTP_SSRF_Private10Blocked(t *testing.T) {
	c := newTestHTTPClient(false)
	e := &Executor{HTTP: c}
	_, err := e.Execute(context.Background(), `
def handle(args):
    return http.get("http://10.0.0.1")
`, nil)
	if err == nil || !errors.Is(err, ErrUnsafeHost) {
		t.Fatalf("err = %v, want ErrUnsafeHost", err)
	}
}

func TestHTTP_SSRF_Private172Blocked(t *testing.T) {
	c := newTestHTTPClient(false)
	e := &Executor{HTTP: c}
	_, err := e.Execute(context.Background(), `
def handle(args):
    return http.get("http://172.16.1.1")
`, nil)
	if err == nil || !errors.Is(err, ErrUnsafeHost) {
		t.Fatalf("err = %v, want ErrUnsafeHost", err)
	}
}

func TestHTTP_SSRF_Private192Blocked(t *testing.T) {
	c := newTestHTTPClient(false)
	e := &Executor{HTTP: c}
	_, err := e.Execute(context.Background(), `
def handle(args):
    return http.get("http://192.168.1.1")
`, nil)
	if err == nil || !errors.Is(err, ErrUnsafeHost) {
		t.Fatalf("err = %v, want ErrUnsafeHost", err)
	}
}

func TestHTTP_SSRF_DNSRebindingBlocked(t *testing.T) {
	// Stub DNS so localhost.attacker.example resolves to 10.0.0.1.
	originalLookup := lookupIPs
	defer func() { lookupIPs = originalLookup }()
	lookupIPs = func(host string) ([]net.IP, error) {
		if host == "rebound.example" {
			return []net.IP{net.ParseIP("10.0.0.1")}, nil
		}
		return originalLookup(host)
	}

	c := newTestHTTPClient(false)
	e := &Executor{HTTP: c}
	_, err := e.Execute(context.Background(), `
def handle(args):
    return http.get("http://rebound.example/")
`, nil)
	if err == nil || !errors.Is(err, ErrUnsafeHost) {
		t.Fatalf("err = %v, want ErrUnsafeHost", err)
	}
}

func TestHTTP_SSRF_UnsupportedSchemeBlocked(t *testing.T) {
	c := newTestHTTPClient(false)
	e := &Executor{HTTP: c}
	_, err := e.Execute(context.Background(), `
def handle(args):
    return http.get("file:///etc/passwd")
`, nil)
	if err == nil || !errors.Is(err, ErrUnsupportedScheme) {
		t.Fatalf("err = %v, want ErrUnsupportedScheme", err)
	}
}

func TestHTTP_ResponseBodyCapped(t *testing.T) {
	// Server streams 2 MiB; client caps at 4 KiB.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		chunk := strings.Repeat("a", 1<<16)
		for i := 0; i < 32; i++ {
			_, _ = w.Write([]byte(chunk))
		}
	}))
	defer srv.Close()

	c := newTestHTTPClient(true)
	c.MaxBodyBytes = 4096
	e := &Executor{HTTP: c}
	res, err := e.Execute(context.Background(), `
def handle(args):
    r = http.get(args[0])
    return str(len(r["body"]))
`, []string{srv.URL})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Return != "4096" {
		t.Fatalf("body length = %s, want 4096", res.Return)
	}
}

// ─── SSRF — redirect follow disabled ──────────────────────────────────
//
// http.Client default follows up to 10 redirects. Without a
// CheckRedirect policy, an attacker-controlled server can return
// `301 Location: http://10.0.0.1/` and the transport silently chases
// it — bypassing CheckURL which only saw the original (public) URL.
//
// PulseGuard's HTTPClient hard-disables redirect follow so 3xx
// responses are returned verbatim to the script. The script must
// re-issue any redirect target through http.get, which routes through
// CheckURL again.

// TestHTTP_RedirectToPrivateIPNotFollowed proves the SSRF bypass
// vector is closed: an external server returns 301 to a private IP;
// the client surfaces the 301 response (status + Location header) and
// does NOT touch the private host. We assert by checking the rendered
// status string is exactly "301" and the response Location header
// matches what the attacker sent.
func TestHTTP_RedirectToPrivateIPNotFollowed(t *testing.T) {
	var hitCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hitCount, 1)
		w.Header().Set("Location", "http://127.0.0.1:1/should-not-be-fetched")
		w.WriteHeader(http.StatusMovedPermanently)
	}))
	defer srv.Close()

	// allowPrivate=true is required so the httptest.Server (bound to
	// 127.0.0.1) itself is reachable; the redirect target lands on a
	// closed port — if redirect WERE followed the script would observe
	// a connection-refused error, not a 301 status.
	c := newTestHTTPClient(true)
	e := &Executor{HTTP: c}
	res, err := e.Execute(context.Background(), `
def handle(args):
    r = http.get(args[0])
    return str(r["status"]) + "|" + r["headers"].get("Location", "")
`, []string{srv.URL})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.HasPrefix(res.Return, "301|") {
		t.Fatalf("status not 301; got %q", res.Return)
	}
	if !strings.Contains(res.Return, "http://127.0.0.1:1/should-not-be-fetched") {
		t.Fatalf("Location header missing from response: %q", res.Return)
	}
	if got := atomic.LoadInt32(&hitCount); got != 1 {
		t.Fatalf("redirect appears to have been followed: server hit %d times, want 1", got)
	}
}

// TestHTTP_RedirectToPublicHostNotFollowed proves the policy is
// global: even when the redirect target is itself a benign public
// hostname we still surface the 3xx so the script (and operators
// reviewing audit logs) can see the indirection.
func TestHTTP_RedirectToPublicHostNotFollowed(t *testing.T) {
	var hitCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hitCount, 1)
		w.Header().Set("Location", "http://example.com/safe")
		w.WriteHeader(http.StatusFound)
	}))
	defer srv.Close()

	c := newTestHTTPClient(true)
	e := &Executor{HTTP: c}
	res, err := e.Execute(context.Background(), `
def handle(args):
    r = http.get(args[0])
    return str(r["status"]) + "|" + r["headers"].get("Location", "")
`, []string{srv.URL})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.HasPrefix(res.Return, "302|") {
		t.Fatalf("status not 302; got %q", res.Return)
	}
	if !strings.Contains(res.Return, "http://example.com/safe") {
		t.Fatalf("Location header missing from response: %q", res.Return)
	}
	if got := atomic.LoadInt32(&hitCount); got != 1 {
		t.Fatalf("redirect appears to have been followed: server hit %d times, want 1", got)
	}
}

// ─── SSRF — IPv6 / metadata end-to-end coverage ────────────────────────
//
// `isBlockedIP` unit tests cover the deny tables; these tests prove the
// scripting → CheckURL → BaseClient chain still rejects the canonical
// SSRF targets when reached via the Starlark `http.get` builtin. They
// complement the existing IPv4 deny tests above.

// TestHTTP_SSRF_IPv6Private covers fc00::/7 (Unique Local Address, the
// IPv6 equivalent of RFC1918). Without an end-to-end test a regression
// in CheckURL's IPv6-literal parsing could quietly open this class.
func TestHTTP_SSRF_IPv6Private(t *testing.T) {
	c := newTestHTTPClient(false)
	e := &Executor{HTTP: c}
	_, err := e.Execute(context.Background(), `
def handle(args):
    return http.get("http://[fc00::1]/")
`, nil)
	if err == nil || !errors.Is(err, ErrUnsafeHost) {
		t.Fatalf("err = %v, want ErrUnsafeHost", err)
	}
}

// TestHTTP_SSRF_IPv6Loopback covers ::1. Same rationale as the IPv4
// 127.0.0.0/8 test, but for bracketed IPv6 literals so we exercise
// the SplitHostPort code path that strips the brackets.
func TestHTTP_SSRF_IPv6Loopback(t *testing.T) {
	c := newTestHTTPClient(false)
	e := &Executor{HTTP: c}
	_, err := e.Execute(context.Background(), `
def handle(args):
    return http.get("http://[::1]/")
`, nil)
	if err == nil || !errors.Is(err, ErrUnsafeHost) {
		t.Fatalf("err = %v, want ErrUnsafeHost", err)
	}
}

// TestHTTP_SSRF_MetadataEndpoint covers the cloud-metadata IP
// (169.254.169.254). The path test (TestHTTP_SSRF_LinkLocalBlocked)
// already hits a metadata sub-path; this one targets the bare host so
// regression in link-local detection cannot hide behind path parsing.
func TestHTTP_SSRF_MetadataEndpoint(t *testing.T) {
	c := newTestHTTPClient(false)
	e := &Executor{HTTP: c}
	_, err := e.Execute(context.Background(), `
def handle(args):
    return http.get("http://169.254.169.254/")
`, nil)
	if err == nil || !errors.Is(err, ErrUnsafeHost) {
		t.Fatalf("err = %v, want ErrUnsafeHost", err)
	}
}

// TestHTTP_BodyCapZeroFallback verifies the fallback at
// http_module.go:162-164. When MaxBodyBytes is left at its zero
// value the do() path must apply the 1 MiB hard-coded cap, not read
// unbounded — otherwise a malicious upstream could exhaust worker
// memory with a chunked endless body. We feed 1.25 MiB and assert the
// script sees exactly 1 MiB.
func TestHTTP_BodyCapZeroFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		chunk := strings.Repeat("b", 1<<16)
		// 20 × 64 KiB = 1.25 MiB total
		for i := 0; i < 20; i++ {
			_, _ = w.Write([]byte(chunk))
		}
	}))
	defer srv.Close()

	c := newTestHTTPClient(true)
	c.MaxBodyBytes = 0 // exercise the fallback branch
	e := &Executor{HTTP: c}
	res, err := e.Execute(context.Background(), `
def handle(args):
    r = http.get(args[0])
    return str(len(r["body"]))
`, []string{srv.URL})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	const oneMiB = 1 << 20
	if res.Return != strconv.Itoa(oneMiB) {
		t.Fatalf("body length = %s, want %d (1 MiB fallback)", res.Return, oneMiB)
	}
}

// ─── DNS rebinding TOCTOU — pinned DialContext closes the window ──────
//
// The threat model is the classic DNS rebinding attack: an attacker
// controls the authoritative DNS for `evil.example`. They serve a
// public answer (8.8.8.8) on the first resolution so CheckURL passes,
// then swap the A record to an internal address (e.g. 169.254.169.254
// or 127.0.0.1) before the transport performs its own DNS lookup. The
// classic stdlib http.Client.Do would re-resolve the host and connect
// to the attacker-chosen internal IP, exposing internal services.
//
// PulseGuard closes the window by giving each request a one-shot
// transport whose DialContext is pinned to the IP CheckURL just
// validated. The transport never consults DNS again — even if the
// attacker's resolver flips A records mid-flight, the dial target
// stays the IP that passed the SSRF check.
//
// These tests prove the property at two layers:
//
//  1. buildPinnedClient unit test — the transport's DialContext, when
//     invoked with `evil.example:80`, dials a specific IP we control
//     (not whatever the host name would resolve to at dial time).
//  2. End-to-end test — a stubbed lookupIPs flips the A record between
//     CheckURL and Do; httptest provides a server bound to the FIRST
//     (validated) IP; the request still lands on the validated server.

// TestPinnedDialContext_IgnoresHost is the unit-level evidence: feed
// a pinned IP and the dialer always targets that IP, never the host
// portion of `addr`. We listen on 127.0.0.1, pin to it, then ask the
// dialer to connect to "evil.example:<port>". If the dialer respected
// the hostname it would NXDOMAIN or hit an external IP; instead it
// must reach the listener.
func TestPinnedDialContext_IgnoresHost(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	pinned := net.ParseIP("127.0.0.1")
	dial := newPinnedDialContext(pinned, "0", 2*time.Second)

	// addr uses an unresolvable hostname; if DialContext honoured it
	// rather than the pinned IP, this would fail with a DNS error.
	conn, err := dial(context.Background(), "tcp",
		net.JoinHostPort("nonexistent.invalid.example", strconv.Itoa(port)))
	if err != nil {
		t.Fatalf("pinned dial: %v", err)
	}
	conn.Close()
}

// TestHTTP_DNSRebinding_DialUsesValidatedIP is the end-to-end proof
// that the TOCTOU window is closed. lookupIPs is stubbed to return a
// loopback address (the only one we can actually listen on inside the
// test) for the first call — that's the validated IP. The httptest
// server is bound to that loopback IP. We then mutate the stub so any
// subsequent lookup of the same host would return 169.254.169.254
// (the cloud metadata endpoint) — exactly the rebinding payload.
//
// If the transport re-queried DNS (the bug we're closing) it would
// dial 169.254.169.254, time out or connection-refuse, and the script
// would surface an error. Because the dialer is pinned to ips[0] from
// CheckURL, it dials the loopback target and the request returns 200.
//
// The test runs with DenyPrivateNets=false because httptest binds to
// 127.0.0.1; the rebinding payload itself is link-local, which the
// guard would normally reject if the second resolution were honoured.
func TestHTTP_DNSRebinding_DialUsesValidatedIP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte("validated"))
	}))
	defer srv.Close()

	// Parse the httptest URL so we know the port/IP it bound to.
	hostPort := strings.TrimPrefix(srv.URL, "http://")
	host, _, err := net.SplitHostPort(hostPort)
	if err != nil {
		t.Fatalf("split server URL: %v", err)
	}
	validated := net.ParseIP(host)
	if validated == nil {
		t.Fatalf("expected IP literal in httptest URL, got %q", host)
	}

	// Two-call stub: first lookup returns the validated IP (so
	// CheckURL passes for our "public" hostname); subsequent lookups
	// would return the rebinding payload — but if the fix works the
	// transport never asks.
	var lookups int32
	originalLookup := lookupIPs
	defer func() { lookupIPs = originalLookup }()
	lookupIPs = func(host string) ([]net.IP, error) {
		if host == "rebind-toctou.example" {
			n := atomic.AddInt32(&lookups, 1)
			if n == 1 {
				return []net.IP{validated}, nil
			}
			return []net.IP{net.ParseIP("169.254.169.254")}, nil
		}
		return originalLookup(host)
	}

	// allowPrivate=true so the loopback validated IP is reachable; the
	// rebinding payload (169.254.169.254) would still be blocked by
	// isBlockedIP if CheckURL ran a second time — but the dialer must
	// not even reach that code path.
	c := newTestHTTPClient(true)
	e := &Executor{HTTP: c}

	// We can't change the Host header that net/http sends without
	// rewriting the URL, so we ask the script to fetch
	// http://rebind-toctou.example:<port>/ — the hostname is what
	// CheckURL+pinned-dial together must protect.
	_, port, _ := net.SplitHostPort(hostPort)
	url := "http://rebind-toctou.example:" + port + "/"
	res, err := e.Execute(context.Background(), `
def handle(args):
    r = http.get(args[0])
    return str(r["status"]) + ":" + r["body"]
`, []string{url})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.HasPrefix(res.Return, "200:") || !strings.Contains(res.Return, "validated") {
		t.Fatalf("Return = %q, want 200:validated (got rebinding-vulnerable answer?)", res.Return)
	}

	// Property assertion: only ONE DNS lookup happened — the one
	// CheckURL performed. The transport must have used the pinned IP
	// for the dial, never reissuing a lookup. If lookups > 1 the fix
	// has regressed.
	if got := atomic.LoadInt32(&lookups); got != 1 {
		t.Fatalf("lookupIPs called %d times; pinned dial must do exactly 1 lookup (DNS rebinding window reopened)", got)
	}
}

// TestBuildPinnedClient_TLSConfigPreservesHostname is the unit
// evidence that the per-request *http.Client preserves the original
// hostname for TLS SNI / cert verification while pinning the TCP
// destination IP. The Host portion of the URL feeds ServerName so
// the TLS handshake still validates against the public certificate;
// only Dial is hijacked.
func TestBuildPinnedClient_TLSConfigPreservesHostname(t *testing.T) {
	u, _ := url.Parse("https://api.example.com/v1/whatever")
	pinned := net.ParseIP("203.0.113.10")
	base := &http.Client{Timeout: 7 * time.Second}

	client := buildPinnedClient(base, u, pinned)
	if client == nil {
		t.Fatal("buildPinnedClient returned nil")
	}
	if client.Timeout != 7*time.Second {
		t.Fatalf("timeout copy: got %s, want 7s", client.Timeout)
	}
	tr, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport type = %T, want *http.Transport", client.Transport)
	}
	if tr.TLSClientConfig == nil || tr.TLSClientConfig.ServerName != "api.example.com" {
		t.Fatalf("TLS ServerName = %q, want api.example.com (cert validation must keep original host)",
			tr.TLSClientConfig.ServerName)
	}
	if tr.DialContext == nil {
		t.Fatal("DialContext must be set; otherwise dial falls back to stdlib DNS lookup")
	}
}


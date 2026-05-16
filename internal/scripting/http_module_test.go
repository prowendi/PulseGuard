package scripting

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
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

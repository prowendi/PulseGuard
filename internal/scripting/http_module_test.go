package scripting

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestHTTPClient(allowPrivate bool) *HTTPClient {
	return &HTTPClient{
		BaseClient:      &http.Client{Timeout: 3 * time.Second},
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

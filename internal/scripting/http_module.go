package scripting

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go.starlark.net/starlark"
)

// HTTPClient is the policy-bound HTTP transport exposed to scripts as
// the global `http` Starlark module. It pairs a stdlib http.Client with
// the SSRF guard and a response-body cap.
//
// Construct via NewHTTPClient or fill the fields directly — both
// surfaces are stable.
type HTTPClient struct {
	// BaseClient is the underlying http.Client used for round-trips.
	// MUST have a finite Timeout; NewHTTPClient enforces 5 s when zero.
	//
	// NOTE: the transport of BaseClient is intentionally NOT used for
	// the actual round-trip — do() builds a per-request *http.Client with
	// a transport whose DialContext is pinned to the IP that CheckURL
	// just validated. BaseClient.Timeout / CheckRedirect are still
	// copied onto every per-request client so the policy stays uniform.
	BaseClient *http.Client
	// MaxBodyBytes caps the response body read into memory before the
	// connection is closed. Defaults to 1 MB.
	MaxBodyBytes int64
	// AllowedScheme is the URL scheme allow-list. Defaults to
	// {"http", "https"}.
	AllowedScheme []string
	// DenyPrivateNets toggles the SSRF guard. Production callers MUST
	// pass true; tests that hit httptest.Server may temporarily flip
	// this to false (see DefaultTestHTTPClient).
	DenyPrivateNets bool
}

// NewHTTPClient fills in safe defaults: 5 s timeout, 1 MiB body cap,
// http+https schemes only, SSRF guard ON, and redirects disabled.
//
// Disabling redirect follow-through is part of the SSRF guarantee:
// CheckURL is applied only to the URL the script supplied, so the
// transport must not silently chase 3xx responses to attacker-chosen
// hosts (e.g. an external server returning `Location: http://10.0.0.1`).
// 3xx responses are returned verbatim to the script — the script can
// inspect status / headers and decide whether to re-issue the request
// (which then goes through CheckURL again).
func NewHTTPClient() *HTTPClient {
	return &HTTPClient{
		BaseClient: &http.Client{
			Timeout:       5 * time.Second,
			CheckRedirect: blockRedirect,
		},
		MaxBodyBytes:    1 << 20,
		AllowedScheme:   []string{"http", "https"},
		DenyPrivateNets: true,
	}
}

// blockRedirect tells net/http to treat the most recent 3xx response as
// the final response (no follow). Returning ErrUseLastResponse is the
// stdlib-documented way to disable redirect chasing without faking an
// error in user code (see net/http.Client.CheckRedirect doc).
func blockRedirect(req *http.Request, via []*http.Request) error {
	return http.ErrUseLastResponse
}

// Module returns the Starlark "http" module value. Pass the same
// context.Context that drives the script's lifetime so HTTP calls are
// cancelled along with the script (timeout / shutdown / etc.).
//
// The returned value is a *httpModule which implements
// starlark.HasAttrs so script code resolves `http.get` / `http.post`
// through Attr.
func (c *HTTPClient) Module(ctx context.Context) starlark.Value {
	return &httpModule{ctx: ctx, c: c}
}

// httpModule is the receiver bound to the `http` builtin so the dotted
// access (http.get / http.post) resolves through Attr.
type httpModule struct {
	ctx context.Context
	c   *HTTPClient
}

// freeze/hash/truth/type satisfy the starlark.Value interface. The
// module is immutable so Freeze is a no-op.
func (m *httpModule) String() string        { return "<module http>" }
func (m *httpModule) Type() string          { return "http_module" }
func (m *httpModule) Freeze()               {}
func (m *httpModule) Truth() starlark.Bool  { return starlark.True }
func (m *httpModule) Hash() (uint32, error) { return 0, fmt.Errorf("unhashable: http_module") }

// Attr lets Starlark resolve `http.get` / `http.post` to builtins.
func (m *httpModule) Attr(name string) (starlark.Value, error) {
	switch name {
	case "get":
		return starlark.NewBuiltin("http.get", m.callGet), nil
	case "post":
		return starlark.NewBuiltin("http.post", m.callPost), nil
	}
	return nil, nil // attribute not found
}

// AttrNames lists the available attributes for introspection.
func (m *httpModule) AttrNames() []string { return []string{"get", "post"} }

func (m *httpModule) callGet(thread *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var urlV starlark.String
	var headers *starlark.Dict
	if err := starlark.UnpackArgs("get", args, kwargs,
		"url", &urlV, "headers?", &headers); err != nil {
		return nil, err
	}
	return m.do(http.MethodGet, urlV.GoString(), nil, headers)
}

func (m *httpModule) callPost(thread *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var urlV starlark.String
	var body starlark.Value
	var headers *starlark.Dict
	if err := starlark.UnpackArgs("post", args, kwargs,
		"url", &urlV, "body?", &body, "headers?", &headers); err != nil {
		return nil, err
	}
	var reader io.Reader
	if body != nil {
		switch v := body.(type) {
		case starlark.String:
			reader = strings.NewReader(v.GoString())
		case starlark.Bytes:
			reader = strings.NewReader(string(v))
		case starlark.NoneType:
			// no body
		default:
			return nil, fmt.Errorf("post: body must be string or bytes, got %s", body.Type())
		}
	}
	return m.do(http.MethodPost, urlV.GoString(), reader, headers)
}

// pinnedDialContextFn is the signature of the DialContext closure
// produced by newPinnedDialContext. Exposed as a named type so tests
// can spy on the dial target via a transport-wrapping helper.
type pinnedDialContextFn = func(ctx context.Context, network, addr string) (net.Conn, error)

// newPinnedDialContext returns a DialContext that ignores the host
// portion of `addr` and dials the pre-validated `pinnedIP` instead.
// The port comes from `addr` (the transport synthesises the right
// default for http/https before invoking the dialer); fallbackPort is
// used only if SplitHostPort cannot parse `addr` (defensive, should not
// happen in practice — net/http always provides host:port).
func newPinnedDialContext(pinnedIP net.IP, fallbackPort string, dialTimeout time.Duration) pinnedDialContextFn {
	d := &net.Dialer{Timeout: dialTimeout}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		_, port, err := net.SplitHostPort(addr)
		if err != nil || port == "" {
			port = fallbackPort
		}
		// Pin the connection to the IP CheckURL just validated. Using
		// the literal IP here means the TCP layer never re-queries DNS,
		// closing the TOCTOU window where an attacker-controlled DNS
		// server could swap the public answer for an internal IP between
		// CheckURL and Do (the classic DNS rebinding bypass).
		return d.DialContext(ctx, network, net.JoinHostPort(pinnedIP.String(), port))
	}
}

// defaultPortFor returns the conventional TCP port for the supplied
// URL scheme. Only http/https are ever reached here because CheckURL
// enforces the scheme allow-list upstream.
func defaultPortFor(scheme string) string {
	switch strings.ToLower(scheme) {
	case "https":
		return "443"
	default:
		return "80"
	}
}

// buildPinnedClient assembles a one-shot *http.Client whose transport
// dials pinnedIP directly. The TLS ServerName is left untouched so the
// TLS handshake (and the cert validation that protects against active
// MITM) still uses the original hostname; only the underlying TCP
// connection is forced to the pre-validated IP.
//
// Each request gets its own transport so connection pools cannot leak
// across script invocations (and across tenants).
func buildPinnedClient(base *http.Client, u *url.URL, pinnedIP net.IP) *http.Client {
	dialTimeout := 5 * time.Second
	if base != nil && base.Timeout > 0 && base.Timeout < dialTimeout {
		dialTimeout = base.Timeout
	}
	fallback := defaultPortFor(u.Scheme)
	tr := &http.Transport{
		DialContext:           newPinnedDialContext(pinnedIP, fallback, dialTimeout),
		TLSHandshakeTimeout:   dialTimeout,
		ResponseHeaderTimeout: dialTimeout,
		IdleConnTimeout:       1 * time.Second,
		DisableKeepAlives:     true,
		// Explicit empty TLSClientConfig means the transport uses
		// sensible defaults AND derives ServerName from u.Hostname()
		// (the standard library does this when ServerName is empty),
		// so TLS SNI / cert verification still target the original
		// hostname — only the TCP destination IP is pinned.
		TLSClientConfig: &tls.Config{ServerName: u.Hostname()},
	}
	client := &http.Client{
		Transport:     tr,
		CheckRedirect: blockRedirect,
	}
	if base != nil {
		if base.CheckRedirect != nil {
			client.CheckRedirect = base.CheckRedirect
		}
		if base.Timeout > 0 {
			client.Timeout = base.Timeout
		} else {
			client.Timeout = 5 * time.Second
		}
	} else {
		client.Timeout = 5 * time.Second
	}
	return client
}

// do executes the HTTP request after applying the SSRF guard and the
// body cap. Errors are returned as Starlark errors so user code can
// catch them via try/except (Starlark does not have try, but the
// executor surfaces them as command-output failures).
func (m *httpModule) do(method, rawURL string, body io.Reader, headers *starlark.Dict) (starlark.Value, error) {
	if m.c == nil || m.c.BaseClient == nil {
		return nil, fmt.Errorf("http: client not configured")
	}
	u, ips, err := CheckURL(rawURL, m.c.AllowedScheme, m.c.DenyPrivateNets)
	if err != nil {
		return nil, err
	}
	if len(ips) == 0 {
		// CheckURL guarantees at least one IP on success, but be paranoid.
		return nil, fmt.Errorf("%w: no IPs resolved", ErrUnsafeHost)
	}
	ctx := m.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return nil, fmt.Errorf("http: build request: %w", err)
	}
	if headers != nil {
		for _, item := range headers.Items() {
			k, ok := item[0].(starlark.String)
			if !ok {
				return nil, fmt.Errorf("http: header key must be string, got %s", item[0].Type())
			}
			v, ok := item[1].(starlark.String)
			if !ok {
				return nil, fmt.Errorf("http: header value must be string, got %s", item[1].Type())
			}
			req.Header.Set(k.GoString(), v.GoString())
		}
	}

	// Build a per-request client whose transport pins the dial target
	// to the IP just validated by CheckURL. This closes the DNS
	// rebinding TOCTOU window: a malicious resolver cannot swap A
	// records between CheckURL and Do because the dialer never queries
	// DNS again — it goes straight to ips[0].
	client := buildPinnedClient(m.c.BaseClient, u, ips[0])
	defer client.CloseIdleConnections()

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http: request: %w", err)
	}
	defer resp.Body.Close()

	cap := m.c.MaxBodyBytes
	if cap <= 0 {
		cap = 1 << 20
	}
	buf := &bytes.Buffer{}
	if _, err := io.Copy(buf, io.LimitReader(resp.Body, cap)); err != nil {
		return nil, fmt.Errorf("http: read body: %w", err)
	}

	out := starlark.NewDict(3)
	_ = out.SetKey(starlark.String("status"), starlark.MakeInt(resp.StatusCode))
	_ = out.SetKey(starlark.String("body"), starlark.String(buf.String()))

	hdr := starlark.NewDict(len(resp.Header))
	for k, vs := range resp.Header {
		if len(vs) == 0 {
			continue
		}
		_ = hdr.SetKey(starlark.String(k), starlark.String(strings.Join(vs, ", ")))
	}
	_ = out.SetKey(starlark.String("headers"), hdr)
	return out, nil
}

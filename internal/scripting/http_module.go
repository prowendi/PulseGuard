package scripting

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
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
// http+https schemes only, SSRF guard ON.
func NewHTTPClient() *HTTPClient {
	return &HTTPClient{
		BaseClient:      &http.Client{Timeout: 5 * time.Second},
		MaxBodyBytes:    1 << 20,
		AllowedScheme:   []string{"http", "https"},
		DenyPrivateNets: true,
	}
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

// do executes the HTTP request after applying the SSRF guard and the
// body cap. Errors are returned as Starlark errors so user code can
// catch them via try/except (Starlark does not have try, but the
// executor surfaces them as command-output failures).
func (m *httpModule) do(method, rawURL string, body io.Reader, headers *starlark.Dict) (starlark.Value, error) {
	if m.c == nil || m.c.BaseClient == nil {
		return nil, fmt.Errorf("http: client not configured")
	}
	if _, _, err := CheckURL(rawURL, m.c.AllowedScheme, m.c.DenyPrivateNets); err != nil {
		return nil, err
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

	resp, err := m.c.BaseClient.Do(req)
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

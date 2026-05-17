package lark

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// oauthFakeServer wraps httptest.Server with the bookkeeping each test
// needs: it counts how many times the OAuth endpoint was hit and lets
// the test rewrite the handler per case.
type oauthFakeServer struct {
	*httptest.Server
	callCount  int32
	handler    atomic.Pointer[http.HandlerFunc]
	lastBody   []byte
	lastMethod string
	lastPath   string
	mu         sync.Mutex
}

func newOAuthFakeServer(h http.HandlerFunc) *oauthFakeServer {
	fs := &oauthFakeServer{}
	fs.handler.Store(&h)
	fs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		fs.mu.Lock()
		fs.lastBody = body
		fs.lastMethod = r.Method
		fs.lastPath = r.URL.Path
		fs.mu.Unlock()
		atomic.AddInt32(&fs.callCount, 1)
		(*fs.handler.Load())(w, r)
	}))
	return fs
}

func (fs *oauthFakeServer) calls() int { return int(atomic.LoadInt32(&fs.callCount)) }

func (fs *oauthFakeServer) setHandler(h http.HandlerFunc) {
	fs.handler.Store(&h)
}

// TestOAuthClient_TokenSuccess pins the happy path: a valid response
// is parsed correctly, the wire shape is what Lark expects, and the
// token is cached so a second call within the TTL never re-hits the
// network.
func TestOAuthClient_TokenSuccess(t *testing.T) {
	srv := newOAuthFakeServer(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","tenant_access_token":"t-abc123","expire":7200}`))
	})
	defer srv.Close()

	c := newOAuthClientWithBase(srv.URL, 2*time.Second, time.Now)
	tok, err := c.Token(context.Background(), "cli_app_x", "secret-x")
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if tok != "t-abc123" {
		t.Fatalf("token = %q", tok)
	}

	// Wire-shape assertions.
	if srv.lastMethod != http.MethodPost {
		t.Fatalf("method = %s", srv.lastMethod)
	}
	if srv.lastPath != "/open-apis/auth/v3/tenant_access_token/internal" {
		t.Fatalf("path = %s", srv.lastPath)
	}
	var sent tokenReq
	if err := json.Unmarshal(srv.lastBody, &sent); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, srv.lastBody)
	}
	if sent.AppID != "cli_app_x" || sent.AppSecret != "secret-x" {
		t.Fatalf("body mismatch: %+v", sent)
	}

	// Second call MUST hit the cache.
	tok2, err := c.Token(context.Background(), "cli_app_x", "secret-x")
	if err != nil {
		t.Fatalf("second Token: %v", err)
	}
	if tok2 != "t-abc123" {
		t.Fatalf("cached token = %q", tok2)
	}
	if got := srv.calls(); got != 1 {
		t.Fatalf("oauth endpoint hit %d times; cache miss after first fetch", got)
	}
}

// TestOAuthClient_RefreshOnExpiry advances a controllable clock past
// the cached expiry and verifies the next Token() call refreshes.
func TestOAuthClient_RefreshOnExpiry(t *testing.T) {
	var responseCounter int32
	srv := newOAuthFakeServer(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&responseCounter, 1)
		_, _ = w.Write([]byte(`{"code":0,"tenant_access_token":"t-` + string(rune('0'+n)) + `","expire":7200}`))
	})
	defer srv.Close()

	// FakeClock so the test owns wall-clock advancement.
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	clk := &fakeClock{now: now}
	c := newOAuthClientWithBase(srv.URL, 2*time.Second, clk.Now)

	first, err := c.Token(context.Background(), "app", "sec")
	if err != nil {
		t.Fatalf("Token 1: %v", err)
	}
	if first != "t-1" {
		t.Fatalf("first = %q", first)
	}

	// Within the TTL → still cached.
	clk.advance(1 * time.Hour)
	again, err := c.Token(context.Background(), "app", "sec")
	if err != nil {
		t.Fatalf("Token 2: %v", err)
	}
	if again != "t-1" {
		t.Fatalf("expected cached t-1, got %q", again)
	}
	if got := srv.calls(); got != 1 {
		t.Fatalf("refresh happened too early; calls = %d", got)
	}

	// Push the clock beyond the (7200s - 5min) safety window. 7200s -
	// 300s = 6900s ≈ 1h55m; jumping to +1h56m forces a refresh.
	clk.advance(56 * time.Minute)
	third, err := c.Token(context.Background(), "app", "sec")
	if err != nil {
		t.Fatalf("Token 3: %v", err)
	}
	if third != "t-2" {
		t.Fatalf("refresh returned %q, want t-2 (incremented response)", third)
	}
	if got := srv.calls(); got != 2 {
		t.Fatalf("expected 2 oauth calls, got %d", got)
	}
}

// TestOAuthClient_NonZeroCode covers the Lark app-level rejection
// (wrong app_id/secret → code != 0). Must be a PermanentClient
// *APIError so the worker DLQs the row instead of retrying.
func TestOAuthClient_NonZeroCode(t *testing.T) {
	srv := newOAuthFakeServer(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":10003,"msg":"invalid app_secret"}`))
	})
	defer srv.Close()

	c := newOAuthClientWithBase(srv.URL, 2*time.Second, time.Now)
	_, err := c.Token(context.Background(), "x", "y")
	if err == nil {
		t.Fatal("expected error for code=10003")
	}
	ae, ok := AsAPIError(err)
	if !ok {
		t.Fatalf("got %T %v want *APIError", err, err)
	}
	if ae.Class != PermanentClient {
		t.Fatalf("class = %v want PermanentClient", ae.Class)
	}
	if ae.Code != 10003 {
		t.Fatalf("code = %d", ae.Code)
	}
}

// TestOAuthClient_HTTP5xx → transient.
func TestOAuthClient_HTTP5xx(t *testing.T) {
	srv := newOAuthFakeServer(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
	})
	defer srv.Close()
	c := newOAuthClientWithBase(srv.URL, 2*time.Second, time.Now)
	_, err := c.Token(context.Background(), "x", "y")
	ae, ok := AsAPIError(err)
	if !ok || ae.Class != Transient || ae.Code != 503 {
		t.Fatalf("got %v", err)
	}
}

// TestOAuthClient_HTTP429 → transient (rate limited).
func TestOAuthClient_HTTP429(t *testing.T) {
	srv := newOAuthFakeServer(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
	})
	defer srv.Close()
	c := newOAuthClientWithBase(srv.URL, 2*time.Second, time.Now)
	_, err := c.Token(context.Background(), "x", "y")
	ae, ok := AsAPIError(err)
	if !ok || ae.Class != Transient {
		t.Fatalf("got %v", err)
	}
}

// TestOAuthClient_FailedResponseNotCached: after a transient failure
// the next call MUST re-fetch (don't poison the cache).
func TestOAuthClient_FailedResponseNotCached(t *testing.T) {
	var n int32
	srv := newOAuthFakeServer(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&n, 1) == 1 {
			w.WriteHeader(503)
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"tenant_access_token":"t-ok","expire":7200}`))
	})
	defer srv.Close()
	c := newOAuthClientWithBase(srv.URL, 2*time.Second, time.Now)
	if _, err := c.Token(context.Background(), "a", "b"); err == nil {
		t.Fatal("expected first call to fail")
	}
	tok, err := c.Token(context.Background(), "a", "b")
	if err != nil {
		t.Fatalf("second Token: %v", err)
	}
	if tok != "t-ok" {
		t.Fatalf("tok = %q", tok)
	}
	if got := srv.calls(); got != 2 {
		t.Fatalf("expected 2 calls, got %d (failed call was cached?)", got)
	}
}

// TestOAuthClient_RotatingSecretEvictsCache exercises the (appID,
// secret) cache key: rotating the secret bypasses the prior cached
// token even within its TTL.
func TestOAuthClient_RotatingSecretEvictsCache(t *testing.T) {
	bodies := []string{
		`{"code":0,"tenant_access_token":"t-old","expire":7200}`,
		`{"code":0,"tenant_access_token":"t-new","expire":7200}`,
	}
	var idx int32
	srv := newOAuthFakeServer(func(w http.ResponseWriter, r *http.Request) {
		i := atomic.AddInt32(&idx, 1) - 1
		if int(i) >= len(bodies) {
			i = int32(len(bodies)) - 1
		}
		_, _ = w.Write([]byte(bodies[i]))
	})
	defer srv.Close()
	c := newOAuthClientWithBase(srv.URL, 2*time.Second, time.Now)
	if tok, _ := c.Token(context.Background(), "app", "old"); tok != "t-old" {
		t.Fatalf("first = %s", tok)
	}
	if tok, _ := c.Token(context.Background(), "app", "new"); tok != "t-new" {
		t.Fatalf("second = %s want t-new (cache should not hit on rotated secret)", tok)
	}
}

// TestOAuthClient_ForgetEvicts manual eviction works for 401-from-IM-API
// scenarios where the cached token has been invalidated server-side.
func TestOAuthClient_ForgetEvicts(t *testing.T) {
	srv := newOAuthFakeServer(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"tenant_access_token":"t-x","expire":7200}`))
	})
	defer srv.Close()
	c := newOAuthClientWithBase(srv.URL, 2*time.Second, time.Now)
	if _, err := c.Token(context.Background(), "a", "b"); err != nil {
		t.Fatal(err)
	}
	if !c.Forget("a", "b") {
		t.Fatalf("Forget reported nothing evicted")
	}
	if _, err := c.Token(context.Background(), "a", "b"); err != nil {
		t.Fatal(err)
	}
	if got := srv.calls(); got != 2 {
		t.Fatalf("after Forget expected refresh; calls = %d", got)
	}
}

// TestOAuthClient_EmptyCreds returns ErrBadAppCreds before any network.
func TestOAuthClient_EmptyCreds(t *testing.T) {
	srv := newOAuthFakeServer(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("must not hit network for empty creds")
	})
	defer srv.Close()
	c := newOAuthClientWithBase(srv.URL, 2*time.Second, time.Now)
	if _, err := c.Token(context.Background(), "", "x"); !errors.Is(err, ErrBadAppCreds) {
		t.Fatalf("empty appID err = %v", err)
	}
	if _, err := c.Token(context.Background(), "x", ""); !errors.Is(err, ErrBadAppCreds) {
		t.Fatalf("empty secret err = %v", err)
	}
}

// TestOAuthClient_UnparseableSuccessTreatedTransient: a 2xx with
// junk body MUST surface as transient so a one-off CDN/proxy hiccup
// doesn't poison cache or DLQ the row.
func TestOAuthClient_UnparseableSuccessTreatedTransient(t *testing.T) {
	srv := newOAuthFakeServer(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<html>oops</html>`))
	})
	defer srv.Close()
	c := newOAuthClientWithBase(srv.URL, 2*time.Second, time.Now)
	_, err := c.Token(context.Background(), "a", "b")
	ae, ok := AsAPIError(err)
	if !ok || ae.Class != Transient {
		t.Fatalf("got %v", err)
	}
}

// TestOAuthClient_EmptyTokenIn2xxIsTransient guards against a server
// returning code=0 but no token (theoretical Lark bug / proxy
// stripping). The cache must NOT poison.
func TestOAuthClient_EmptyTokenIn2xxIsTransient(t *testing.T) {
	srv := newOAuthFakeServer(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"tenant_access_token":"","expire":7200}`))
	})
	defer srv.Close()
	c := newOAuthClientWithBase(srv.URL, 2*time.Second, time.Now)
	_, err := c.Token(context.Background(), "a", "b")
	if err == nil {
		t.Fatal("expected error for empty token")
	}
	if ae, ok := AsAPIError(err); !ok || ae.Class != Transient {
		t.Fatalf("got %v", err)
	}
	// Issuing a second call should re-hit (cache must not have been
	// populated with an empty token).
	srv.setHandler(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"tenant_access_token":"t-ok","expire":7200}`))
	})
	tok, err := c.Token(context.Background(), "a", "b")
	if err != nil {
		t.Fatal(err)
	}
	if tok != "t-ok" {
		t.Fatalf("got %q", tok)
	}
}

// TestOAuthClient_ConcurrentFirstFetchSerialised confirms the lock
// ordering: N goroutines racing for the same (appID, secret) all see
// exactly one underlying network call.
func TestOAuthClient_ConcurrentFirstFetchSerialised(t *testing.T) {
	srv := newOAuthFakeServer(func(w http.ResponseWriter, r *http.Request) {
		// Pause so racing goroutines actually contend.
		time.Sleep(50 * time.Millisecond)
		_, _ = w.Write([]byte(`{"code":0,"tenant_access_token":"t-shared","expire":7200}`))
	})
	defer srv.Close()
	c := newOAuthClientWithBase(srv.URL, 5*time.Second, time.Now)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tok, err := c.Token(context.Background(), "a", "b")
			if err != nil {
				t.Errorf("Token: %v", err)
				return
			}
			if !strings.HasPrefix(tok, "t-shared") {
				t.Errorf("tok = %q", tok)
			}
		}()
	}
	wg.Wait()
	if got := srv.calls(); got != 1 {
		t.Fatalf("expected 1 fetch under contention, got %d", got)
	}
}

// fakeClock is a non-monotonic test clock; advance() is the only way
// time moves forward.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (fc *fakeClock) Now() time.Time {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	return fc.now
}

func (fc *fakeClock) advance(d time.Duration) {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	fc.now = fc.now.Add(d)
}

// TestOAuth_M1_DifferentKeysDoNotBlock proves the M-1 fix: a slow
// Lark response for app_id A must NOT block a concurrent Token() call
// for app_id B. Before the singleflight refactor the call would have
// queued behind the global mutex; with per-key dedup it runs in
// parallel.
func TestOAuth_M1_DifferentKeysDoNotBlock(t *testing.T) {
	const slowDelay = 500 * time.Millisecond

	srv := newOAuthFakeServer(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		// Differentiate per app_id payload: "slow_app" sleeps before
		// responding; everyone else responds immediately. Both still
		// return a valid token + expire.
		if strings.Contains(string(body), `"app_id":"slow_app"`) {
			time.Sleep(slowDelay)
			_, _ = w.Write([]byte(`{"code":0,"tenant_access_token":"t-slow","expire":7200}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"tenant_access_token":"t-fast","expire":7200}`))
	})
	defer srv.Close()
	c := newOAuthClientWithBase(srv.URL, 5*time.Second, nil)

	// Launch the slow caller; give it a moment to enter the http.Do
	// inside fetchToken. With the M-1 refactor the singleflight call
	// for "slow_app" should now be the only barrier — "fast_app" runs
	// through its own singleflight Group entry concurrently.
	slowDone := make(chan struct{})
	go func() {
		defer close(slowDone)
		_, _ = c.Token(context.Background(), "slow_app", "s")
	}()
	time.Sleep(50 * time.Millisecond) // let the goroutine reach http.Do

	start := time.Now()
	tok, err := c.Token(context.Background(), "fast_app", "f")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("fast Token err: %v", err)
	}
	if tok != "t-fast" {
		t.Fatalf("fast token = %q", tok)
	}
	// "fast_app" should resolve well under the slow window. We allow a
	// generous 200ms ceiling for scheduling slack on shared CI; the
	// pre-M-1 behaviour would have blocked for the full 500ms.
	if elapsed > 200*time.Millisecond {
		t.Fatalf("fast Token waited %v — slow tenant blocked us (M-1 regression)", elapsed)
	}

	// Let the slow call finish so the test cleans up tidily.
	<-slowDone
}

// TestOAuth_M1_SameKeyCoalesces proves the dedup invariant still
// holds: 10 concurrent callers for the SAME (app_id, app_secret) pair
// must collapse to a single HTTP fetch.
func TestOAuth_M1_SameKeyCoalesces(t *testing.T) {
	srv := newOAuthFakeServer(func(w http.ResponseWriter, r *http.Request) {
		// Small artificial delay so the callers have time to pile up
		// behind the singleflight Group before the first one returns.
		time.Sleep(100 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"tenant_access_token":"t-shared","expire":7200}`))
	})
	defer srv.Close()
	c := newOAuthClientWithBase(srv.URL, 5*time.Second, nil)

	const callers = 10
	var wg sync.WaitGroup
	wg.Add(callers)
	tokens := make([]string, callers)
	errs := make([]error, callers)
	for i := 0; i < callers; i++ {
		i := i
		go func() {
			defer wg.Done()
			tokens[i], errs[i] = c.Token(context.Background(), "shared_app", "s")
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d err: %v", i, err)
		}
		if tokens[i] != "t-shared" {
			t.Fatalf("caller %d token = %q", i, tokens[i])
		}
	}
	// 10 callers, but at most 1 network call (thundering-herd
	// prevention). The cached-after-first-hit case might also produce
	// 1 — anything > 1 means the dedup is broken.
	if got := srv.calls(); got != 1 {
		t.Fatalf("server hit %d times, want exactly 1 (singleflight dedup broken)", got)
	}
}

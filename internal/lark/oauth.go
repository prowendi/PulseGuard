package lark

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// canonicalAPIBase is the production Lark Open Platform host. Tests
// override apiBase on OAuthClient/AppClient via the newOAuthClientWithBase
// helper so an httptest.Server can capture the OAuth + IM API calls.
const canonicalAPIBase = "https://open.feishu.cn"

// tokenExpirySafetyMargin keeps a fresh token cached only until 5
// minutes before Lark's stated expiry. The minimum cache window is
// 1 minute — Lark documented expire is 7200s so this default never
// kicks in for healthy responses, but it bounds the worst-case
// thundering-herd window if Lark ever shortens the TTL drastically.
const tokenExpirySafetyMargin = 5 * time.Minute

// TokenSource is the narrow interface the AppClient (and the future
// events sender) consume to acquire a tenant_access_token for an
// (app_id, app_secret) pair. The dispatcher injects *OAuthClient in
// production; tests can substitute a stub that returns a stable string.
type TokenSource interface {
	// Token returns a non-empty tenant_access_token if the credentials
	// are valid. Implementations cache responses keyed by appID and
	// transparently refresh near expiry. Errors propagate Lark's
	// app-level non-zero codes as *APIError so callers can branch on
	// PermanentClient (invalid credentials) vs Transient (5xx / parse).
	Token(ctx context.Context, appID, appSecret string) (string, error)
}

// cachedToken is the in-memory record per appID. Token is the bearer
// value Lark returned; expiresAt is wall-clock (now()+expire-safetyMargin)
// after which Token() refreshes.
type cachedToken struct {
	token     string
	expiresAt time.Time
}

// OAuthClient implements TokenSource against the Lark Open Platform
// internal-app endpoint:
//
//	POST /open-apis/auth/v3/tenant_access_token/internal
//	body: {"app_id":"...","app_secret":"..."}
//	→    {"code":0,"tenant_access_token":"...","expire":7200,"msg":"..."}
//
// Cache keys are (appID, appSecret) so rotating the secret invalidates
// the cached token automatically — a misconfig that points two
// distinct secrets at the same appID never accidentally reuses a
// stale bearer.
//
// Concurrency (M-1, 2026-05): the mutex now only protects the cache
// map (sub-microsecond critical sections). The actual HTTP fetch is
// deduplicated by golang.org/x/sync/singleflight keyed on cacheKey,
// so:
//   - Two concurrent callers for the SAME (app_id, app_secret) collapse
//     to a single network call (thundering-herd prevention).
//   - Concurrent callers for DIFFERENT app_ids fetch in parallel — a
//     slow Lark response for tenant A no longer blocks tenant B's send.
//
// apiBase is empty in production (canonicalAPIBase is used). Tests
// override it via newOAuthClientWithBase so an httptest.Server can
// observe the POST without exposing the field on the public type.
type OAuthClient struct {
	httpC   *http.Client
	apiBase string
	clock   func() time.Time

	mu    sync.Mutex
	cache map[string]cachedToken

	// fetch coalesces concurrent refreshes for the same key. Kept as a
	// value (not pointer) so the zero value of OAuthClient is also
	// usable in tests that build the struct field-by-field — the only
	// constructors today (NewOAuthClient + newOAuthClientWithBase)
	// initialise it explicitly anyway.
	fetch singleflight.Group
}

// NewOAuthClient constructs a production-ready client. timeout caps
// the HTTP call; a zero/negative value falls back to 10s (same default
// as the webhook Client) so a misconfig never leaks a hung goroutine
// into the worker pool.
func NewOAuthClient(timeout time.Duration) *OAuthClient {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &OAuthClient{
		httpC: &http.Client{Timeout: timeout},
		clock: time.Now,
		cache: map[string]cachedToken{},
	}
}

// newOAuthClientWithBase is a test-only ctor that points the client at
// an httptest server. Keeping it package-private (lowercase) prevents
// production wire-up from accidentally bypassing the canonical host.
func newOAuthClientWithBase(base string, timeout time.Duration, clk func() time.Time) *OAuthClient {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	if clk == nil {
		clk = time.Now
	}
	return &OAuthClient{
		httpC:   &http.Client{Timeout: timeout},
		apiBase: base,
		clock:   clk,
		cache:   map[string]cachedToken{},
	}
}

// cacheKey hashes the (appID, appSecret) pair into the map key. We
// concatenate with a NUL separator so two adjacent appIDs whose
// secrets happen to share a boundary character cannot collide. The
// secret is part of the key intentionally — rotating it invalidates
// the cache without an explicit Forget() call.
func cacheKey(appID, appSecret string) string {
	return appID + "\x00" + appSecret
}

// tokenReq is the JSON body Lark expects for the internal-app endpoint.
type tokenReq struct {
	AppID     string `json:"app_id"`
	AppSecret string `json:"app_secret"`
}

// tokenResp mirrors the Lark response shape:
//
//	{
//	  "code": 0,
//	  "msg": "ok",
//	  "tenant_access_token": "t-...",
//	  "expire": 7200
//	}
//
// A non-zero code signals an app-level failure (wrong app_id, wrong
// secret, app not enabled, ...) and is converted to PermanentClient
// *APIError. Token() never caches a failed response.
type tokenResp struct {
	Code              int    `json:"code"`
	Msg               string `json:"msg"`
	TenantAccessToken string `json:"tenant_access_token"`
	Expire            int    `json:"expire"`
}

// Token returns a usable tenant_access_token, refreshing on cache
// miss / near-expiry. M-1: the mutex only protects the cache lookup;
// concurrent fetches for the same key are deduplicated by
// singleflight, and fetches for different keys run in parallel.
func (c *OAuthClient) Token(ctx context.Context, appID, appSecret string) (string, error) {
	if appID == "" {
		return "", fmt.Errorf("%w: app_id is empty", ErrBadAppCreds)
	}
	if appSecret == "" {
		return "", fmt.Errorf("%w: app_secret is empty", ErrBadAppCreds)
	}
	key := cacheKey(appID, appSecret)

	// Fast path: cached and fresh. Critical section is bounded to a
	// map lookup so slow Lark responses cannot starve other tenants.
	if tok, ok := c.lookup(key); ok {
		return tok, nil
	}

	// Slow path: deduplicated network fetch. singleflight.Do guarantees
	// only one in-flight call per key; the rest of the callers receive
	// the same result without spawning extra requests.
	v, err, _ := c.fetch.Do(key, func() (any, error) {
		// Re-check the cache under the fetch barrier: another goroutine
		// in the same singleflight cohort may already have populated it
		// while we were queued behind their HTTP call.
		if tok, ok := c.lookup(key); ok {
			return tok, nil
		}
		return c.fetchToken(ctx, appID, appSecret, key)
	})
	if err != nil {
		return "", err
	}
	return v.(string), nil
}

// lookup returns the cached token if present and not yet expired.
// Holds c.mu only for the map read so callers never block on a slow
// HTTP call.
func (c *OAuthClient) lookup(key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if entry, ok := c.cache[key]; ok && c.clock().Before(entry.expiresAt) {
		return entry.token, true
	}
	return "", false
}

// fetchToken performs the network round-trip and stores the result on
// success. Runs inside singleflight.Do so it executes at most once per
// key concurrently; if it returns an error nothing is cached.
func (c *OAuthClient) fetchToken(ctx context.Context, appID, appSecret, key string) (string, error) {
	body, err := json.Marshal(tokenReq{AppID: appID, AppSecret: appSecret})
	if err != nil {
		return "", fmt.Errorf("lark oauth: marshal req: %w", err)
	}

	base := c.apiBase
	if base == "" {
		base = canonicalAPIBase
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		base+"/open-apis/auth/v3/tenant_access_token/internal", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("lark oauth: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")

	resp, err := c.httpC.Do(req)
	if err != nil {
		return "", &APIError{
			Class:       Transient,
			Description: "oauth http do: " + err.Error(),
		}
	}
	defer resp.Body.Close()

	respBody, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return "", &APIError{
			Class:       Transient,
			Code:        resp.StatusCode,
			Description: "oauth read body: " + readErr.Error(),
		}
	}

	if resp.StatusCode >= 500 {
		return "", &APIError{
			Class:       Transient,
			Code:        resp.StatusCode,
			Description: "lark oauth 5xx: " + safeBodyExcerpt(respBody),
		}
	}
	if resp.StatusCode == 429 {
		return "", &APIError{
			Class:       Transient,
			Code:        resp.StatusCode,
			Description: "lark oauth rate limited",
		}
	}
	if resp.StatusCode >= 400 {
		return "", &APIError{
			Class:       PermanentClient,
			Code:        resp.StatusCode,
			Description: "lark oauth 4xx: " + safeBodyExcerpt(respBody),
		}
	}

	var env tokenResp
	if err := json.Unmarshal(respBody, &env); err != nil {
		return "", &APIError{
			Class:       Transient,
			Code:        resp.StatusCode,
			Description: "oauth parse body: " + err.Error(),
		}
	}
	if env.Code != 0 {
		return "", &APIError{
			Class:       PermanentClient,
			Code:        env.Code,
			Description: fmt.Sprintf("lark oauth code=%d msg=%s", env.Code, env.Msg),
		}
	}
	if env.TenantAccessToken == "" {
		return "", &APIError{
			Class:       Transient,
			Code:        resp.StatusCode,
			Description: "lark oauth: empty tenant_access_token in 2xx response",
		}
	}

	// Compute the cache TTL: Lark says "expire" seconds. We subtract
	// the safety margin so we refresh strictly before Lark would
	// reject us. Guard against pathological Lark responses by
	// clamping to a minimum 60s window — a 0/negative value would
	// trigger refresh on every call and spam the endpoint.
	ttl := time.Duration(env.Expire)*time.Second - tokenExpirySafetyMargin
	if ttl < time.Minute {
		ttl = time.Minute
	}
	c.mu.Lock()
	c.cache[key] = cachedToken{
		token:     env.TenantAccessToken,
		expiresAt: c.clock().Add(ttl),
	}
	c.mu.Unlock()
	return env.TenantAccessToken, nil
}

// Forget evicts a single (appID, appSecret) entry. Useful when the
// caller learns the token is invalid out-of-band (e.g. a 401 from the
// IM send endpoint) and wants the next Token() call to refresh
// instead of replaying the cached value. Returns whether an entry
// existed.
func (c *OAuthClient) Forget(appID, appSecret string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := cacheKey(appID, appSecret)
	if _, ok := c.cache[key]; ok {
		delete(c.cache, key)
		return true
	}
	return false
}

// ErrBadAppCreds is returned by Token when the caller passes an empty
// app_id or app_secret. Separate sentinel so callers can detect a
// misconfigured bot row before any network traffic.
var ErrBadAppCreds = errors.New("lark oauth: app credentials missing")

// compile-time interface check.
var _ TokenSource = (*OAuthClient)(nil)

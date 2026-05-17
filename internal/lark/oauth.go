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
// stale bearer. The lock is held across the HTTP call deliberately:
// the call is rare (≤ once per 2 hours per app) and serialising
// concurrent first-fetches prevents the thundering-herd that would
// otherwise hit Lark with N identical token requests on startup.
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
// miss / near-expiry. The lock is held for the entire (cache lookup +
// HTTP call + cache store) sequence to serialise the first-fetch
// across goroutines without a sync.Once-per-key dance — see the type
// comment for the rationale.
func (c *OAuthClient) Token(ctx context.Context, appID, appSecret string) (string, error) {
	if appID == "" {
		return "", fmt.Errorf("%w: app_id is empty", ErrBadAppCreds)
	}
	if appSecret == "" {
		return "", fmt.Errorf("%w: app_secret is empty", ErrBadAppCreds)
	}
	key := cacheKey(appID, appSecret)

	c.mu.Lock()
	defer c.mu.Unlock()

	now := c.clock()
	if entry, ok := c.cache[key]; ok && now.Before(entry.expiresAt) {
		return entry.token, nil
	}

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
	c.cache[key] = cachedToken{
		token:     env.TenantAccessToken,
		expiresAt: now.Add(ttl),
	}
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

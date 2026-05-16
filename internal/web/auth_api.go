package web

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/wendi/pulseguard/internal/domain"
	wmw "github.com/wendi/pulseguard/internal/web/middleware"

	"github.com/go-chi/chi/v5"
)

// authPayload models the JSON body for register/login. Fields not in the
// payload are silently ignored by the DisallowUnknownFields decoder.
type authPayload struct {
	Email      string `json:"email"`
	Password   string `json:"password"`
	InviteCode string `json:"invite_code,omitempty"`
}

// authResp is the success envelope returned by register/login. The
// session cookie is sent via Set-Cookie alongside; the payload exposes
// the same expiry for clients that prefer to read it from the body.
type authResp struct {
	Tenant           tenantView `json:"tenant"`
	SessionExpiresAt time.Time  `json:"session_expires_at"`
}

type tenantView struct {
	ID          int64       `json:"id"`
	Email       string      `json:"email"`
	Role        domain.Role `json:"role"`
	DisplayName string      `json:"display_name,omitempty"`
}

func toTenantView(t *domain.Tenant) tenantView {
	return tenantView{
		ID:          t.ID,
		Email:       t.Email,
		Role:        t.Role,
		DisplayName: t.DisplayName,
	}
}

// installAuthAPIRoutes is invoked from server.go via the routes.go hook.
func installAuthAPIRoutes(r chi.Router, deps Deps) {
	r.Post("/auth/register", apiRegister(deps))
	r.Post("/auth/login", apiLogin(deps))
}

func apiRegister(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var p authPayload
		if !decodeJSON(w, r, &p) {
			return
		}
		p.Email = strings.TrimSpace(strings.ToLower(p.Email))
		if p.Email == "" || p.Password == "" || p.InviteCode == "" {
			writeError(w, r, http.StatusBadRequest, "VALIDATION", "email/password/invite_code required")
			return
		}
		tenant, sess, err := deps.Auth.Register(r.Context(), p.Email, p.Password, p.InviteCode)
		if err != nil {
			writeAuthError(w, r, deps, err)
			return
		}
		setSessionCookie(w, sess, deps)
		// Bind the CSRF token to the freshly-minted session id directly;
		// the ctx has no session attached on this code path (RequireAuth
		// only runs on /api/v1/* authed routes, not on register).
		IssueCSRF(w, sess.ID, deps.csrfSecret(), deps.cookieSecure())
		writeJSON(w, http.StatusCreated, authResp{
			Tenant:           toTenantView(tenant),
			SessionExpiresAt: sess.ExpiresAt,
		})
	}
}

func apiLogin(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var p authPayload
		if !decodeJSON(w, r, &p) {
			return
		}
		p.Email = strings.TrimSpace(strings.ToLower(p.Email))
		sess, err := deps.Auth.Login(r.Context(), p.Email, p.Password)
		if err != nil {
			writeAuthError(w, r, deps, err)
			return
		}
		// Tenant lookup is cheap (single row) and lets us return the
		// caller's role/email without forcing an extra /me roundtrip.
		tenant, _, err := deps.Auth.SessionFromID(r.Context(), sess.ID)
		if err != nil {
			writeInternal(w, r, deps, "post-login lookup", err)
			return
		}
		setSessionCookie(w, sess, deps)
		IssueCSRF(w, sess.ID, deps.csrfSecret(), deps.cookieSecure())
		writeJSON(w, http.StatusOK, authResp{
			Tenant:           toTenantView(tenant),
			SessionExpiresAt: sess.ExpiresAt,
		})
	}
}

func apiLogout(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !VerifyCSRF(r, deps.csrfSecret()) {
			writeError(w, r, http.StatusForbidden, "FORBIDDEN", "csrf required")
			return
		}
		sess := wmw.Session(r.Context())
		if sess != nil {
			_ = deps.Auth.Logout(r.Context(), sess.ID)
		}
		clearSessionCookie(w, deps)
		ClearCSRF(w, deps.cookieSecure())
		w.WriteHeader(http.StatusNoContent)
	}
}

func apiMe(deps Deps) http.HandlerFunc {
	_ = deps
	return func(w http.ResponseWriter, r *http.Request) {
		t := wmw.Tenant(r.Context())
		if t == nil {
			writeError(w, r, http.StatusUnauthorized, "UNAUTHORIZED", "login required")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"tenant": toTenantView(t)})
	}
}

// setSessionCookie writes the psg_session cookie sized to the session's
// expiry. HttpOnly is mandatory; Secure follows config.
func setSessionCookie(w http.ResponseWriter, sess *domain.Session, deps Deps) {
	http.SetCookie(w, &http.Cookie{
		Name:     wmw.CookieSession,
		Value:    sess.ID,
		Path:     "/",
		HttpOnly: true,
		Secure:   deps.cookieSecure(),
		SameSite: http.SameSiteLaxMode,
		Expires:  sess.ExpiresAt,
	})
}

func clearSessionCookie(w http.ResponseWriter, deps Deps) {
	http.SetCookie(w, &http.Cookie{
		Name:     wmw.CookieSession,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   deps.cookieSecure(),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

func writeAuthError(w http.ResponseWriter, r *http.Request, deps Deps, err error) {
	switch {
	case errors.Is(err, domain.ErrInviteInvalid):
		writeError(w, r, http.StatusBadRequest, "VALIDATION", "invite code invalid")
	case errors.Is(err, domain.ErrValidation):
		writeError(w, r, http.StatusBadRequest, "VALIDATION", err.Error())
	case errors.Is(err, domain.ErrConflict):
		writeError(w, r, http.StatusConflict, "CONFLICT", "email already registered")
	case errors.Is(err, domain.ErrUnauthorized):
		writeError(w, r, http.StatusUnauthorized, "UNAUTHORIZED", "invalid credentials")
	default:
		writeInternal(w, r, deps, "auth error", err)
	}
}

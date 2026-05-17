package web

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/prowendi/PulseGuard/internal/domain"
	wmw "github.com/prowendi/PulseGuard/internal/web/middleware"

	"github.com/go-chi/chi/v5"
)

// inviteView is the JSON projection of domain.InviteCode. UsedAt /
// ExpiresAt are RFC3339 strings (empty when nil) so JSON clients don't
// have to decode null markers.
type inviteView struct {
	Code      string `json:"code"`
	CreatedBy int64  `json:"created_by"`
	UsedBy    int64  `json:"used_by,omitempty"`
	ExpiresAt string `json:"expires_at,omitempty"`
	UsedAt    string `json:"used_at,omitempty"`
	CreatedAt string `json:"created_at"`
	// Used is a convenience boolean so the UI does not have to compare
	// against a sentinel string.
	Used bool `json:"used"`
}

func toInviteView(inv *domain.InviteCode) inviteView {
	v := inviteView{
		Code:      inv.Code,
		CreatedBy: inv.CreatedBy,
		CreatedAt: inv.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
	if inv.UsedBy != nil {
		v.UsedBy = *inv.UsedBy
	}
	if inv.UsedAt != nil {
		v.UsedAt = inv.UsedAt.UTC().Format("2006-01-02T15:04:05Z")
		v.Used = true
	}
	if inv.ExpiresAt != nil {
		v.ExpiresAt = inv.ExpiresAt.UTC().Format("2006-01-02T15:04:05Z")
	}
	return v
}

type inviteCreatePayload struct {
	TTLSeconds int `json:"ttl_seconds"`
	Count      int `json:"count"`
}

// InvitesPerAdminDailyCap is the hard ceiling on how many invite codes
// a single admin can generate in a rolling UTC day. Sized for a
// reasonable onboarding burst (hundreds of new tenants per admin per
// day is plenty) while preventing a compromised admin from bulk-minting
// codes that could be reused to register thousands of accounts.
//
// Refs: round2-security-report S-L3.
const InvitesPerAdminDailyCap = 500

func installInvitesAPIRoutes(r chi.Router, deps Deps) {
	r.Get("/invites", apiInviteList(deps))
	r.Post("/invites", apiInviteCreate(deps))
	r.Delete("/invites/{code}", apiInviteDelete(deps))
}

func apiInviteList(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		admin := wmw.Tenant(r.Context())
		items, err := deps.Invites.ListByCreator(r.Context(), admin.ID)
		if err != nil {
			writeRepoError(w, r, deps, err)
			return
		}
		views := make([]inviteView, 0, len(items))
		for _, inv := range items {
			views = append(views, toInviteView(inv))
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": views})
	}
}

func apiInviteCreate(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var p inviteCreatePayload
		if !decodeJSON(w, r, &p) {
			return
		}
		count, ttl, ok := validateInviteCreate(w, r, p)
		if !ok {
			return
		}
		admin := wmw.Tenant(r.Context())
		// Enforce per-admin per-UTC-day cap. The window starts at the
		// most recent UTC midnight relative to the server clock; using
		// a real-time clock here (rather than deps.Clock) keeps the
		// behaviour aligned with the wall-clock day operators see in
		// their dashboards while remaining cheap (one SELECT COUNT).
		nowUTC := deps.Clock.Now().UTC()
		windowStart := time.Date(nowUTC.Year(), nowUTC.Month(), nowUTC.Day(), 0, 0, 0, 0, time.UTC)
		existing, err := deps.Invites.CountByCreatorSince(r.Context(), admin.ID, windowStart)
		if err != nil {
			writeRepoError(w, r, deps, err)
			return
		}
		if existing+count > InvitesPerAdminDailyCap {
			remaining := InvitesPerAdminDailyCap - existing
			if remaining < 0 {
				remaining = 0
			}
			// Retry-After is the seconds-until-UTC-midnight (the cap
			// resets at that boundary). Surface it so a polite client
			// can back off without hammering us.
			retryAfter := int(time.Date(nowUTC.Year(), nowUTC.Month(), nowUTC.Day()+1, 0, 0, 0, 0, time.UTC).Sub(nowUTC).Seconds())
			if retryAfter < 1 {
				retryAfter = 1
			}
			w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
			writeError(w, r, http.StatusTooManyRequests, "RATE_LIMITED",
				fmt.Sprintf("daily invite cap reached (%d/%d); %d remaining today",
					existing, InvitesPerAdminDailyCap, remaining))
			return
		}
		created := make([]inviteView, 0, count)
		for i := 0; i < count; i++ {
			inv, err := deps.Auth.GenerateInvite(r.Context(), admin.ID, ttl)
			if err != nil {
				writeRepoError(w, r, deps, err)
				return
			}
			created = append(created, toInviteView(inv))
		}
		writeJSON(w, http.StatusCreated, map[string]any{"items": created})
	}
}

func apiInviteDelete(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		code := strings.TrimSpace(chi.URLParam(r, "code"))
		if code == "" {
			writeError(w, r, http.StatusBadRequest, "VALIDATION", "missing code")
			return
		}
		admin := wmw.Tenant(r.Context())
		if err := deps.Invites.Delete(r.Context(), code, admin.ID); err != nil {
			switch {
			case errors.Is(err, domain.ErrNotFound):
				writeError(w, r, http.StatusNotFound, "NOT_FOUND", "invite not found")
			case errors.Is(err, domain.ErrInviteInvalid):
				writeError(w, r, http.StatusConflict, "CONFLICT", "invite already consumed")
			default:
				writeRepoError(w, r, deps, err)
			}
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// validateInviteCreate normalises the request. Defaults: count=1
// (capped at 100 per call to avoid runaway loops), ttl_seconds=0 means
// no expiry. Negative values are rejected.
func validateInviteCreate(w http.ResponseWriter, r *http.Request, p inviteCreatePayload) (int, time.Duration, bool) {
	if p.Count < 0 || p.TTLSeconds < 0 {
		writeError(w, r, http.StatusBadRequest, "VALIDATION", "count and ttl_seconds must be >= 0")
		return 0, 0, false
	}
	count := p.Count
	if count == 0 {
		count = 1
	}
	if count > 100 {
		writeError(w, r, http.StatusBadRequest, "VALIDATION", "count must be <= 100")
		return 0, 0, false
	}
	return count, time.Duration(p.TTLSeconds) * time.Second, true
}

package web

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	wmw "github.com/wendi/pulseguard/internal/web/middleware"

	"github.com/go-chi/chi/v5"
)

type inviteListPage struct {
	pageData
	Invites []inviteView
}

func installInvitesUIRoutes(r chi.Router, deps Deps) {
	r.Get("/invites", uiInviteList(deps))
	r.Post("/invites", uiInviteCreate(deps))
	r.Post("/invites/{code}/delete", uiInviteDelete(deps))
}

func uiInviteList(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		renderInvitePage(w, r, deps, nil)
	}
}

func uiInviteCreate(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !VerifyCSRF(r) {
			http.Error(w, "csrf", http.StatusForbidden)
			return
		}
		_ = r.ParseForm()
		ttlSec, _ := strconv.Atoi(r.PostForm.Get("ttl_seconds"))
		count, _ := strconv.Atoi(r.PostForm.Get("count"))
		if count <= 0 {
			count = 1
		}
		if count > 100 {
			renderInvitePage(w, r, deps, &flash{Level: "error", Message: "count 不能大于 100"})
			return
		}
		if ttlSec < 0 {
			renderInvitePage(w, r, deps, &flash{Level: "error", Message: "ttl_seconds 不能为负"})
			return
		}
		admin := wmw.Tenant(r.Context())
		ttl := time.Duration(ttlSec) * time.Second
		for i := 0; i < count; i++ {
			if _, err := deps.Auth.GenerateInvite(r.Context(), admin.ID, ttl); err != nil {
				renderInvitePage(w, r, deps, &flash{Level: "error", Message: err.Error()})
				return
			}
		}
		http.Redirect(w, r, "/ui/invites", http.StatusSeeOther)
	}
}

func uiInviteDelete(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !VerifyCSRF(r) {
			http.Error(w, "csrf", http.StatusForbidden)
			return
		}
		code := strings.TrimSpace(chi.URLParam(r, "code"))
		admin := wmw.Tenant(r.Context())
		_ = deps.Invites.Delete(r.Context(), code, admin.ID)
		http.Redirect(w, r, "/ui/invites", http.StatusSeeOther)
	}
}

func renderInvitePage(w http.ResponseWriter, r *http.Request, deps Deps, fl *flash) {
	admin := wmw.Tenant(r.Context())
	items, _ := deps.Invites.ListByCreator(r.Context(), admin.ID)
	views := make([]inviteView, 0, len(items))
	for _, inv := range items {
		views = append(views, toInviteView(inv))
	}
	_ = Render(w, http.StatusOK, "invites-page", inviteListPage{
		pageData: pageData{
			Title:  "邀请码",
			Tenant: admin,
			Active: "invites",
			CSRF:   readCSRFCookie(r),
			Flash:  fl,
		},
		Invites: views,
	})
}

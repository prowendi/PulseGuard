package web

import "github.com/go-chi/chi/v5"

// mountAuthAPI installs the public auth endpoints (register/login).
// Populated in W2.
func mountAuthAPI(r chi.Router, deps Deps) {}

// mountPushAPI installs the public push endpoint. Populated in W3.
func mountPushAPI(r chi.Router, deps Deps) {}

// mountAuthedAPI installs every session-gated /api/v1/* endpoint.
// Populated in W2/W4..W9.
func mountAuthedAPI(r chi.Router, deps Deps) {}

// mountAuthUI installs the public login/register HTMX views.
// Populated in W2.
func mountAuthUI(r chi.Router, deps Deps) {}

// mountAuthedUI installs every session-gated /ui/* view.
// Populated in W2/W4..W9.
func mountAuthedUI(r chi.Router, deps Deps) {}

package aviary

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

// controlHandler builds the HTTP handler served on the control-plane host
// (requests without a project subdomain). It exposes the project-management
// JSON API plus the landing page.
func (a *Aviary) controlHandler() http.Handler {
	mux := http.NewServeMux()

	// Public auth endpoints.
	mux.HandleFunc("POST /api/auth/login", a.apiLogin)
	mux.HandleFunc("POST /api/auth/logout", a.apiLogout)
	mux.HandleFunc("GET /api/auth/session", a.apiSession)

	// Superuser passkey ceremonies. Login is passwordless (public); register
	// and management require an authenticated superuser session.
	mux.HandleFunc("POST /api/auth/passkey/login/begin", a.suPasskeyLoginBegin)
	mux.HandleFunc("POST /api/auth/passkey/login/finish", a.suPasskeyLoginFinish)
	mux.HandleFunc("POST /api/auth/passkey/register/begin", a.requireSuperuser(a.suPasskeyRegisterBegin))
	mux.HandleFunc("POST /api/auth/passkey/register/finish", a.requireSuperuser(a.suPasskeyRegisterFinish))
	mux.HandleFunc("GET /api/auth/passkey", a.requireSuperuser(a.suPasskeyList))
	mux.HandleFunc("DELETE /api/auth/passkey/{id}", a.requireSuperuser(a.suPasskeyDelete))

	// Superuser authentication hardening (passkey-only sign-in toggle).
	mux.HandleFunc("PUT /api/auth/security", a.requireSuperuser(a.apiPutSecurity))

	// Project lifecycle. Listing/reading is available to any authenticated
	// identity (filtered by access); mutations are superuser-only.
	mux.HandleFunc("GET /api/projects", a.requireAuth(a.apiListProjects))
	mux.HandleFunc("POST /api/projects", a.requireSuperuser(a.apiCreateProject))
	mux.HandleFunc("GET /api/projects/{id}", a.requireAuth(a.apiGetProject))
	mux.HandleFunc("PATCH /api/projects/{id}", a.requireSuperuser(a.apiPatchProject))
	mux.HandleFunc("DELETE /api/projects/{id}", a.requireSuperuser(a.apiDeleteProject))

	// One-click dashboard SSO: mints a single-use ticket and redirects the
	// caller into the project's PocketBase admin dashboard, already logged in.
	// Auth is enforced inside the handler (not via requireAuth) so an
	// unauthenticated browser is sent to the login page rather than a JSON 401.
	mux.HandleFunc("GET /api/projects/{id}/dashboard", a.apiDashboardSSO)

	// Programmatic equivalent of dashboard SSO: mint a short-lived PocketBase
	// superuser token for the project (no native password login required), for
	// migrations, seed scripts and CI. Auth is enforced inside the handler so a
	// project-scoped API key is accepted alongside interactive sessions.
	mux.HandleFunc("POST /api/projects/{id}/admin-token", a.apiProjectAdminToken)

	// Static file editor for each project's pb_public directory. Available to
	// superusers and to collaborators granted the project.
	mux.HandleFunc("GET /api/projects/{id}/files", a.requireAuth(a.apiListFiles))
	mux.HandleFunc("GET /api/projects/{id}/files/content", a.requireAuth(a.apiReadFile))
	mux.HandleFunc("PUT /api/projects/{id}/files/content", a.requireAuth(a.apiWriteFile))
	mux.HandleFunc("DELETE /api/projects/{id}/files/content", a.requireAuth(a.apiDeleteFile))

	// JS hooks editor for each project's pb_hooks directory. Editing hooks
	// changes server-side behavior, so this is owner-only (superuser or granted
	// collaborator) and rejects project-scoped API keys, and a write/delete
	// reboots the project so the new hooks take effect.
	mux.HandleFunc("GET /api/projects/{id}/hooks", a.requireAuth(a.apiListHooks))
	mux.HandleFunc("GET /api/projects/{id}/hooks/content", a.requireAuth(a.apiReadHook))
	mux.HandleFunc("PUT /api/projects/{id}/hooks/content", a.requireAuth(a.apiWriteHook))
	mux.HandleFunc("DELETE /api/projects/{id}/hooks/content", a.requireAuth(a.apiDeleteHook))

	// Scheduled jobs (cron). The control plane owns the schedule and wakes the
	// project's cage on demand to invoke a POST /cron/... route, so scheduled
	// work survives idle eviction. Management is owner-only (superuser or
	// granted collaborator); project-scoped API keys are rejected because a job
	// runs server-side project code.
	mux.HandleFunc("GET /api/projects/{id}/crons", a.requireAuth(a.apiListCrons))
	mux.HandleFunc("POST /api/projects/{id}/crons", a.requireAuth(a.apiCreateCron))
	mux.HandleFunc("PATCH /api/projects/{id}/crons/{cronId}", a.requireAuth(a.apiUpdateCron))
	mux.HandleFunc("DELETE /api/projects/{id}/crons/{cronId}", a.requireAuth(a.apiDeleteCron))
	mux.HandleFunc("POST /api/projects/{id}/crons/{cronId}/run", a.requireAuth(a.apiRunCron))

	// Bulk atomic deploy: publish a built site (tar.gz/zip) into pb_public in a
	// single swap. Same auth as the file endpoints (cookie or API key).
	mux.HandleFunc("POST /api/projects/{id}/deploy", a.requireAuth(a.apiDeployProject))

	// Per-project storage usage + quota metrics. Same auth as files.
	mux.HandleFunc("GET /api/projects/{id}/metrics", a.requireAuth(a.apiProjectMetrics))

	// Project-scoped API keys: non-interactive credentials for agents/CI to
	// drive the file (and future deploy) endpoints. Management is owner-only
	// (superuser or granted collaborator); API keys themselves cannot manage
	// keys.
	mux.HandleFunc("GET /api/projects/{id}/keys", a.requireAuth(a.apiListAPIKeys))
	mux.HandleFunc("POST /api/projects/{id}/keys", a.requireAuth(a.apiCreateAPIKey))
	mux.HandleFunc("DELETE /api/projects/{id}/keys/{keyId}", a.requireAuth(a.apiDeleteAPIKey))

	mux.HandleFunc("GET /api/superuser", a.requireSuperuser(a.apiGetSuperuser))
	// PUT is allowed without auth only for first-run setup (no superuser yet);
	// afterwards it requires an authenticated superuser session.
	mux.HandleFunc("PUT /api/superuser", a.requireAuthOrBootstrap(a.apiPutSuperuser))

	// Collaborators & invitations (superuser-only), plus a public accept.
	mux.HandleFunc("GET /api/collaborators", a.requireSuperuser(a.apiListCollaborators))
	mux.HandleFunc("DELETE /api/collaborators/{email}", a.requireSuperuser(a.apiDeleteCollaborator))
	mux.HandleFunc("DELETE /api/collaborators/{email}/projects/{id}", a.requireSuperuser(a.apiRevokeCollaboratorProject))
	mux.HandleFunc("GET /api/invitations", a.requireSuperuser(a.apiListInvitations))
	mux.HandleFunc("POST /api/invitations", a.requireSuperuser(a.apiCreateInvitation))
	mux.HandleFunc("POST /api/invitations/accept", a.apiAcceptInvitation)

	// Catch-all GET serves the landing page.
	mux.HandleFunc("GET /", a.landing)

	// Machine-readable OpenAPI description of this control-plane API.
	mux.HandleFunc("GET /api/openapi.json", a.apiControlOpenAPI)

	// LLM/agent orientation index (https://llmstxt.org).
	mux.HandleFunc("GET /llms.txt", a.apiLlmsTxt)

	return mux
}

// requireAuthOrBootstrap permits the request when either the caller is an
// authenticated superuser, or no superuser exists yet (first-run setup).
func (a *Aviary) requireAuthOrBootstrap(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.currentSuperuser(r) != "" {
			next(w, r)
			return
		}
		configured, err := a.HasSuperuser(r.Context())
		if err != nil {
			a.apiError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if configured {
			a.apiError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		next(w, r) // bootstrap: first superuser may be created unauthenticated
	}
}

// --- API handlers ---

type createProjectRequest struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type patchProjectRequest struct {
	Status     *Status `json:"status,omitempty"`
	Name       *string `json:"name,omitempty"`
	SPA        *bool   `json:"spa,omitempty"`
	QuotaBytes *int64  `json:"quotaBytes,omitempty"`
}

// projectView augments a stored Project with live runtime state for API
// responses.
type projectView struct {
	*Project
	Running bool `json:"running"`
}

func (a *Aviary) apiListProjects(w http.ResponseWriter, r *http.Request) {
	projects, err := a.store.List(r.Context())
	if err != nil {
		a.apiError(w, http.StatusInternalServerError, err.Error())
		return
	}

	email, role, _ := a.identity(r)
	var allowed map[string]bool
	if role != roleSuperuser {
		ids, err := a.store.ListCollaboratorProjects(r.Context(), email)
		if err != nil {
			a.apiError(w, http.StatusInternalServerError, err.Error())
			return
		}
		allowed = make(map[string]bool, len(ids))
		for _, id := range ids {
			allowed[id] = true
		}
	}

	running := a.runningProjects()
	views := make([]projectView, 0, len(projects))
	for _, p := range projects {
		if allowed != nil && !allowed[p.ID] {
			continue
		}
		views = append(views, projectView{Project: p, Running: running[p.ID]})
	}
	writeJSON(w, http.StatusOK, views)
}

func (a *Aviary) apiCreateProject(w http.ResponseWriter, r *http.Request) {
	var req createProjectRequest
	if !decodeJSON(w, r, &req, a) {
		return
	}

	p, err := a.CreateProject(r.Context(), req.ID, req.Name)
	switch {
	case errors.Is(err, ErrInvalidID):
		a.apiError(w, http.StatusBadRequest, "invalid project id")
	case errors.Is(err, ErrReserved):
		a.apiError(w, http.StatusBadRequest, "project id is reserved")
	case errors.Is(err, ErrExists):
		a.apiError(w, http.StatusConflict, "project already exists")
	case err != nil:
		a.apiError(w, http.StatusInternalServerError, err.Error())
	default:
		writeJSON(w, http.StatusCreated, p)
	}
}

func (a *Aviary) apiGetProject(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	email, role, _ := a.identity(r)
	if role != roleSuperuser {
		ok, err := a.store.CollaboratorHasProject(r.Context(), email, id)
		if err != nil {
			a.apiError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if !ok {
			// Hide existence of projects the collaborator cannot access.
			a.apiError(w, http.StatusNotFound, "project not found")
			return
		}
	}

	p, err := a.GetProject(r.Context(), id)
	switch {
	case errors.Is(err, ErrNotFound):
		a.apiError(w, http.StatusNotFound, "project not found")
	case err != nil:
		a.apiError(w, http.StatusInternalServerError, err.Error())
	default:
		writeJSON(w, http.StatusOK, p)
	}
}

func (a *Aviary) apiPatchProject(w http.ResponseWriter, r *http.Request) {
	var req patchProjectRequest
	if !decodeJSON(w, r, &req, a) {
		return
	}
	if req.Status == nil && req.Name == nil && req.SPA == nil && req.QuotaBytes == nil {
		a.apiError(w, http.StatusBadRequest, "nothing to update: provide 'status', 'name', 'spa' and/or 'quotaBytes'")
		return
	}

	id := r.PathValue("id")

	if req.Name != nil {
		if err := a.SetProjectName(r.Context(), id, *req.Name); err != nil {
			a.patchError(w, err)
			return
		}
	}
	if req.SPA != nil {
		if err := a.SetProjectSPA(r.Context(), id, *req.SPA); err != nil {
			a.patchError(w, err)
			return
		}
	}
	if req.QuotaBytes != nil {
		if *req.QuotaBytes < 0 {
			a.apiError(w, http.StatusBadRequest, "quotaBytes must be zero (unlimited) or a positive number of bytes")
			return
		}
		if err := a.SetProjectQuota(r.Context(), id, *req.QuotaBytes); err != nil {
			a.patchError(w, err)
			return
		}
	}
	if req.Status != nil {
		if *req.Status != StatusActive && *req.Status != StatusDisabled {
			a.apiError(w, http.StatusBadRequest, "status must be 'active' or 'disabled'")
			return
		}
		if err := a.SetProjectStatus(r.Context(), id, *req.Status); err != nil {
			a.patchError(w, err)
			return
		}
	}

	p, err := a.GetProject(r.Context(), id)
	if err != nil {
		a.patchError(w, err)
		return
	}
	running := a.runningProjects()
	writeJSON(w, http.StatusOK, projectView{Project: p, Running: running[p.ID]})
}

// patchError maps a project mutation error to an HTTP response.
func (a *Aviary) patchError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		a.apiError(w, http.StatusNotFound, "project not found")
	default:
		a.apiError(w, http.StatusInternalServerError, err.Error())
	}
}

func (a *Aviary) apiDeleteProject(w http.ResponseWriter, r *http.Request) {
	err := a.DeleteProject(r.Context(), r.PathValue("id"))
	switch {
	case errors.Is(err, ErrNotFound):
		a.apiError(w, http.StatusNotFound, "project not found")
	case err != nil:
		a.apiError(w, http.StatusInternalServerError, err.Error())
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// --- superuser handlers ---

type putSuperuserRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

func (a *Aviary) apiGetSuperuser(w http.ResponseWriter, r *http.Request) {
	su, err := a.GetSuperuser(r.Context())
	switch {
	case errors.Is(err, ErrNoSuperuser):
		writeJSON(w, http.StatusOK, map[string]any{"configured": false})
	case err != nil:
		a.apiError(w, http.StatusInternalServerError, err.Error())
	default:
		writeJSON(w, http.StatusOK, map[string]any{
			"configured": true,
			"email":      su.Email,
			"updated":    su.UpdatedAt,
		})
	}
}

func (a *Aviary) apiPutSuperuser(w http.ResponseWriter, r *http.Request) {
	var req putSuperuserRequest
	if !decodeJSON(w, r, &req, a) {
		return
	}

	err := a.SetSuperuser(r.Context(), req.Email, req.Password)
	switch {
	case err != nil && (errors.Is(err, ErrNoSuperuser)):
		a.apiError(w, http.StatusInternalServerError, err.Error())
	case err != nil && isSuperuserValidationErr(err):
		a.apiError(w, http.StatusBadRequest, err.Error())
	case err != nil:
		a.apiError(w, http.StatusInternalServerError, err.Error())
	default:
		su, _ := a.GetSuperuser(r.Context())
		writeJSON(w, http.StatusOK, map[string]any{
			"configured": true,
			"email":      su.Email,
			"updated":    su.UpdatedAt,
		})
	}
}

// isSuperuserValidationErr reports whether err is a user-facing validation
// failure from SetSuperuser (bad email or too-short password).
func isSuperuserValidationErr(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "email is required") ||
		strings.Contains(msg, "at least 8 characters")
}

// --- helpers ---

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// decodeJSON decodes the request body into v, writing a 400 response and
// returning false on failure.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any, a *Aviary) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		a.apiError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

func (a *Aviary) apiError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": msg, "code": status})
}

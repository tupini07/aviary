package aviary

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/pocketbase/pocketbase/core"
)

// apiProjectAdminToken mints a short-lived PocketBase superuser auth token for a
// project and returns it as JSON. It lets automation (migrations, seed scripts,
// CI) drive a project's PocketBase API as a superuser WITHOUT enabling native
// password login — the same brute-force-proof handoff the browser dashboard SSO
// uses, exposed for programmatic callers.
//
// Access mirrors the other per-project endpoints: an interactive superuser, a
// collaborator granted the project, or a project-scoped API key. Because an API
// key has no associated person, key callers receive a token for the federated
// control-plane superuser (present in every project's _superusers collection).
//
// The returned token is a standard PocketBase auth token signed by the target
// project's instance, so it authenticates against https://<id>.<domain>/api/...
// as a Bearer token until it expires (PocketBase's default superuser token TTL).
func (a *Aviary) apiProjectAdminToken(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	caller, ok := a.authorizeProjectAccess(w, r, id)
	if !ok {
		return
	}

	p, err := a.store.Get(r.Context(), id)
	switch {
	case errors.Is(err, ErrNotFound):
		a.apiError(w, http.StatusNotFound, "unknown project")
		return
	case err != nil:
		a.apiError(w, http.StatusInternalServerError, err.Error())
		return
	case p.Status != StatusActive:
		a.apiError(w, http.StatusForbidden, "project disabled")
		return
	}

	// Resolve a real _superusers email to mint for. API-key principals are
	// synthetic, so fall back to the federated control-plane superuser.
	mintEmail := caller
	if _, role, _ := a.identity(r); role == roleAPIKey {
		su, err := a.GetSuperuser(r.Context())
		if err != nil {
			a.apiError(w, http.StatusInternalServerError, "no control-plane superuser configured")
			return
		}
		mintEmail = su.Email
	}

	cage, err := a.getCage(id, p.SPA)
	if err != nil {
		a.apiError(w, http.StatusInternalServerError, "failed to start project: "+err.Error())
		return
	}

	record, err := cage.app.FindAuthRecordByEmail(core.CollectionNameSuperusers, mintEmail)
	if errors.Is(err, sql.ErrNoRows) {
		a.apiError(w, http.StatusForbidden, "no dashboard access for this account")
		return
	}
	if err != nil {
		a.apiError(w, http.StatusInternalServerError, err.Error())
		return
	}

	record.IgnoreEmailVisibility(true)
	token, err := record.NewAuthToken()
	if err != nil {
		a.apiError(w, http.StatusInternalServerError, "failed to mint token")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"token":          token,
		"superuserEmail": mintEmail,
		"apiURL":         projectPublicOrigin(r, id),
		"issued":         time.Now().UTC().Format(time.RFC3339),
	})
}

// projectPublicOrigin returns the scheme://host origin that serves project id's
// PocketBase API, derived from the control-plane request the caller arrived on.
func projectPublicOrigin(r *http.Request, id string) string {
	scheme := "http"
	if isHTTPS(r) {
		scheme = "https"
	}
	return fmt.Sprintf("%s://%s", scheme, projectHostFromControl(r.Host, id))
}

package aviary

import (
	"errors"
	"net/http"
	"strings"

	"github.com/tupini07/aviary/internal/controlplane"
)

// --- invitation handlers ---

type createInvitationRequest struct {
	Email     string `json:"email"`
	ProjectID string `json:"projectId"`
}

// apiCreateInvitation issues a single-use invitation for a project and returns
// the raw token (shown once) plus a ready-to-share accept URL.
func (a *Aviary) apiCreateInvitation(w http.ResponseWriter, r *http.Request) {
	var req createInvitationRequest
	if !decodeJSON(w, r, &req, a) {
		return
	}
	if strings.TrimSpace(req.Email) == "" || strings.TrimSpace(req.ProjectID) == "" {
		a.apiError(w, http.StatusBadRequest, "email and projectId are required")
		return
	}

	token, err := a.CreateInvitation(r.Context(), req.Email, req.ProjectID)
	switch {
	case errors.Is(err, ErrNotFound):
		a.apiError(w, http.StatusNotFound, "project not found")
	case err != nil:
		a.apiError(w, http.StatusInternalServerError, err.Error())
	default:
		scheme := "http"
		if isHTTPS(r) {
			scheme = "https"
		}
		writeJSON(w, http.StatusCreated, map[string]any{
			"token":     token,
			"email":     strings.ToLower(strings.TrimSpace(req.Email)),
			"projectId": req.ProjectID,
			"acceptUrl": scheme + "://" + r.Host + "/invite?token=" + token,
		})
	}
}

// apiListInvitations returns all pending invitations (without their tokens).
func (a *Aviary) apiListInvitations(w http.ResponseWriter, r *http.Request) {
	invites, err := a.store.ListInvitations(r.Context())
	if err != nil {
		a.apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if invites == nil {
		invites = []controlplane.Invitation{}
	}
	writeJSON(w, http.StatusOK, invites)
}

type acceptInvitationRequest struct {
	Token    string `json:"token"`
	Password string `json:"password"`
}

// apiAcceptInvitation redeems an invitation token, creating the collaborator
// (for a new invitee) and granting project access. It is public.
func (a *Aviary) apiAcceptInvitation(w http.ResponseWriter, r *http.Request) {
	var req acceptInvitationRequest
	if !decodeJSON(w, r, &req, a) {
		return
	}
	if strings.TrimSpace(req.Token) == "" {
		a.apiError(w, http.StatusBadRequest, "token is required")
		return
	}

	email, projectID, err := a.AcceptInvitation(r.Context(), req.Token, req.Password)
	switch {
	case errors.Is(err, ErrInvitationInvalid):
		a.apiError(w, http.StatusBadRequest, "invitation is invalid or has expired")
	case err != nil && strings.Contains(err.Error(), "at least 8 characters"):
		a.apiError(w, http.StatusBadRequest, err.Error())
	case err != nil:
		a.apiError(w, http.StatusInternalServerError, err.Error())
	default:
		writeJSON(w, http.StatusOK, map[string]any{
			"accepted":  true,
			"email":     email,
			"projectId": projectID,
		})
	}
}

// --- collaborator handlers ---

// apiListCollaborators returns every collaborator with their granted projects.
func (a *Aviary) apiListCollaborators(w http.ResponseWriter, r *http.Request) {
	collabs, err := a.store.ListCollaborators(r.Context())
	if err != nil {
		a.apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if collabs == nil {
		collabs = []controlplane.Collaborator{}
	}
	writeJSON(w, http.StatusOK, collabs)
}

// apiDeleteCollaborator removes a collaborator and revokes all their access,
// deleting their _superusers records from any running granted projects.
func (a *Aviary) apiDeleteCollaborator(w http.ResponseWriter, r *http.Request) {
	email := strings.ToLower(strings.TrimSpace(r.PathValue("email")))
	c, err := a.store.GetCollaborator(r.Context(), email)
	if errors.Is(err, controlplane.ErrNotFound) {
		a.apiError(w, http.StatusNotFound, "collaborator not found")
		return
	}
	if err != nil {
		a.apiError(w, http.StatusInternalServerError, err.Error())
		return
	}

	for _, projectID := range c.Projects {
		a.removeSuperuserFromProject(projectID, email)
	}
	if err := a.store.DeleteCollaborator(r.Context(), email); err != nil {
		a.apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

// apiRevokeCollaboratorProject removes a single project grant from a
// collaborator.
func (a *Aviary) apiRevokeCollaboratorProject(w http.ResponseWriter, r *http.Request) {
	email := strings.ToLower(strings.TrimSpace(r.PathValue("email")))
	projectID := r.PathValue("id")

	err := a.RevokeCollaboratorProject(r.Context(), email, projectID)
	switch {
	case errors.Is(err, controlplane.ErrNotFound):
		a.apiError(w, http.StatusNotFound, "grant not found")
	case err != nil:
		a.apiError(w, http.StatusInternalServerError, err.Error())
	default:
		writeJSON(w, http.StatusOK, map[string]any{"revoked": true})
	}
}

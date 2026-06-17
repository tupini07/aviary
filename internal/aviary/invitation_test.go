package aviary

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/pocketbase/pocketbase/core"
)

func TestInvitationFlow(t *testing.T) {
	av := newTestAviary(t)
	sess := loginAs(t, av, "admin@example.com", "password123")

	// Create a project to invite a collaborator to.
	rec := doControl(t, av, http.MethodPost, "/api/projects", createProjectRequest{ID: "alpha", Name: "Alpha"}, sess)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create project: status %d body %s", rec.Code, rec.Body.String())
	}

	// Superuser issues an invitation.
	rec = doControl(t, av, http.MethodPost, "/api/invitations",
		createInvitationRequest{Email: "collab@example.com", ProjectID: "alpha"}, sess)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create invitation: status %d body %s", rec.Code, rec.Body.String())
	}
	var inv struct {
		Token     string `json:"token"`
		AcceptURL string `json:"acceptUrl"`
	}
	mustJSON(t, rec.Body.Bytes(), &inv)
	if inv.Token == "" {
		t.Fatal("expected an invitation token")
	}

	// It should show up in the pending list.
	rec = doControl(t, av, http.MethodGet, "/api/invitations", nil, sess)
	var pending []Invitation
	mustJSON(t, rec.Body.Bytes(), &pending)
	if len(pending) != 1 || pending[0].Email != "collab@example.com" {
		t.Fatalf("unexpected pending invitations: %+v", pending)
	}

	// Invitee accepts (public endpoint, no session).
	rec = doControl(t, av, http.MethodPost, "/api/invitations/accept",
		acceptInvitationRequest{Token: inv.Token, Password: "collabpass1"})
	if rec.Code != http.StatusOK {
		t.Fatalf("accept invitation: status %d body %s", rec.Code, rec.Body.String())
	}

	// Invitation is now consumed.
	rec = doControl(t, av, http.MethodGet, "/api/invitations", nil, sess)
	mustJSON(t, rec.Body.Bytes(), &pending)
	if len(pending) != 0 {
		t.Fatalf("expected invitation to be consumed, got %+v", pending)
	}

	// Collaborator can log in.
	collabSess := loginCollaborator(t, av, "collab@example.com", "collabpass1")

	// Collaborator sees only the granted project.
	rec = doControl(t, av, http.MethodGet, "/api/projects", nil, collabSess)
	var list []projectView
	mustJSON(t, rec.Body.Bytes(), &list)
	if len(list) != 1 || list[0].ID != "alpha" {
		t.Fatalf("collaborator project list: %+v", list)
	}

	// Collaborator cannot create projects (superuser-only).
	rec = doControl(t, av, http.MethodPost, "/api/projects",
		createProjectRequest{ID: "beta", Name: "Beta"}, collabSess)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("collaborator create project: status %d, want 401", rec.Code)
	}

	// Collaborator cannot see a project they were not granted.
	rec = doControl(t, av, http.MethodPost, "/api/projects",
		createProjectRequest{ID: "beta", Name: "Beta"}, sess)
	if rec.Code != http.StatusCreated {
		t.Fatalf("superuser create beta: status %d", rec.Code)
	}
	rec = doControl(t, av, http.MethodGet, "/api/projects/beta", nil, collabSess)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("collaborator get beta: status %d, want 404", rec.Code)
	}

	// The collaborator's credentials unlock the granted project's dashboard:
	// booting the project should seed them into _superusers.
	app := bootProject(t, av, "alpha")
	if _, err := app.FindAuthRecordByEmail(core.CollectionNameSuperusers, "collab@example.com"); err != nil {
		t.Fatalf("collaborator not seeded into project _superusers: %v", err)
	}

	// Superuser revokes the grant; collaborator loses visibility.
	rec = doControl(t, av, http.MethodDelete, "/api/collaborators/collab@example.com/projects/alpha", nil, sess)
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke grant: status %d body %s", rec.Code, rec.Body.String())
	}
	rec = doControl(t, av, http.MethodGet, "/api/projects", nil, collabSess)
	mustJSON(t, rec.Body.Bytes(), &list)
	if len(list) != 0 {
		t.Fatalf("expected no projects after revoke, got %+v", list)
	}

	// And the _superusers record should have been removed from the running app.
	if _, err := app.FindAuthRecordByEmail(core.CollectionNameSuperusers, "collab@example.com"); err == nil {
		t.Fatal("collaborator still present in project _superusers after revoke")
	}
}

func TestAcceptInvitationRejectsBadToken(t *testing.T) {
	av := newTestAviary(t)
	rec := doControl(t, av, http.MethodPost, "/api/invitations/accept",
		acceptInvitationRequest{Token: "nope", Password: "whatever12"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("accept bad token: status %d, want 400", rec.Code)
	}
}

func TestCreateInvitationUnknownProject(t *testing.T) {
	av := newTestAviary(t)
	sess := loginAs(t, av, "admin@example.com", "password123")
	rec := doControl(t, av, http.MethodPost, "/api/invitations",
		createInvitationRequest{Email: "x@example.com", ProjectID: "ghost"}, sess)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("invite to unknown project: status %d, want 404", rec.Code)
	}
}

func TestDeleteProjectCascadesCollaboratorGrants(t *testing.T) {
	av := newTestAviary(t)
	ctx := context.Background()
	sess := loginAs(t, av, "admin@example.com", "password123")

	if _, err := av.CreateProject(ctx, "alpha", ""); err != nil {
		t.Fatalf("create project: %v", err)
	}
	token, err := av.CreateInvitation(ctx, "c@example.com", "alpha")
	if err != nil {
		t.Fatalf("create invitation: %v", err)
	}
	if _, _, err := av.AcceptInvitation(ctx, token, "password123"); err != nil {
		t.Fatalf("accept invitation: %v", err)
	}

	// Deleting the project should drop the collaborator's grant.
	rec := doControl(t, av, http.MethodDelete, "/api/projects/alpha", nil, sess)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete project: status %d", rec.Code)
	}
	has, err := av.store.CollaboratorHasProject(ctx, "c@example.com", "alpha")
	if err != nil {
		t.Fatalf("check grant: %v", err)
	}
	if has {
		t.Fatal("collaborator grant survived project deletion")
	}
}

// loginCollaborator logs in an existing collaborator and returns the session
// cookie.
func loginCollaborator(t *testing.T, av *Aviary, email, password string) *http.Cookie {
	t.Helper()
	rec := doControl(t, av, http.MethodPost, "/api/auth/login",
		loginRequest{Email: email, Password: password})
	if rec.Code != http.StatusOK {
		t.Fatalf("collaborator login: status %d body %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Role string `json:"role"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Role != roleCollaborator {
		t.Fatalf("expected collaborator role, got %q", resp.Role)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			return c
		}
	}
	t.Fatal("collaborator login set no session cookie")
	return nil
}

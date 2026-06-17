package aviary

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// TestFilesCRUD exercises the happy path: write, list, read, delete a file in a
// project's pb_public directory through the control-plane API.
func TestFilesCRUD(t *testing.T) {
	av := newTestAviary(t)
	sess := loginAs(t, av, "admin@example.com", "password123")
	doControl(t, av, http.MethodPost, "/api/projects", createProjectRequest{ID: "alpha"}, sess)

	// Empty project => empty listing.
	rec := doControl(t, av, http.MethodGet, "/api/projects/alpha/files", nil, sess)
	if rec.Code != http.StatusOK {
		t.Fatalf("list empty: status %d body %s", rec.Code, rec.Body.String())
	}
	var list []fileEntry
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list) != 0 {
		t.Fatalf("expected empty listing, got %+v", list)
	}

	// Write a file (in a nested dir to prove parents are created).
	body := fileContent{Path: "css/app.css", Content: "body{color:red}"}
	rec = doControl(t, av, http.MethodPut, "/api/projects/alpha/files/content", body, sess)
	if rec.Code != http.StatusOK {
		t.Fatalf("write: status %d body %s", rec.Code, rec.Body.String())
	}

	// List should now include it.
	rec = doControl(t, av, http.MethodGet, "/api/projects/alpha/files", nil, sess)
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list) != 1 || list[0].Path != "css/app.css" {
		t.Fatalf("listing = %+v, want css/app.css", list)
	}

	// Read it back.
	rec = doControl(t, av, http.MethodGet, "/api/projects/alpha/files/content?path=css/app.css", nil, sess)
	if rec.Code != http.StatusOK {
		t.Fatalf("read: status %d body %s", rec.Code, rec.Body.String())
	}
	var got fileContent
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.Content != "body{color:red}" {
		t.Fatalf("read content = %q", got.Content)
	}

	// Delete it.
	rec = doControl(t, av, http.MethodDelete, "/api/projects/alpha/files/content?path=css/app.css", nil, sess)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete: status %d body %s", rec.Code, rec.Body.String())
	}

	// Reading the deleted file => 404.
	rec = doControl(t, av, http.MethodGet, "/api/projects/alpha/files/content?path=css/app.css", nil, sess)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("read after delete: status %d, want 404", rec.Code)
	}
}

// TestFilesPathTraversalRejected ensures paths escaping pb_public are refused.
func TestFilesPathTraversalRejected(t *testing.T) {
	av := newTestAviary(t)
	sess := loginAs(t, av, "admin@example.com", "password123")
	doControl(t, av, http.MethodPost, "/api/projects", createProjectRequest{ID: "alpha"}, sess)

	escape := "../../secret.txt"

	rec := doControl(t, av, http.MethodPut, "/api/projects/alpha/files/content",
		fileContent{Path: escape, Content: "x"}, sess)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("write traversal: status %d, want 400", rec.Code)
	}

	q := url.QueryEscape(escape)
	rec = doControl(t, av, http.MethodGet, "/api/projects/alpha/files/content?path="+q, nil, sess)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("read traversal: status %d, want 400", rec.Code)
	}
	rec = doControl(t, av, http.MethodDelete, "/api/projects/alpha/files/content?path="+q, nil, sess)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("delete traversal: status %d, want 400", rec.Code)
	}
}

// TestFilesAccessControl verifies unauthenticated and unauthorized callers are
// rejected, while a granted collaborator is allowed.
func TestFilesAccessControl(t *testing.T) {
	av := newTestAviary(t)
	ctx := context.Background()
	loginAs(t, av, "admin@example.com", "password123")

	if _, err := av.CreateProject(ctx, "alpha", ""); err != nil {
		t.Fatalf("create alpha: %v", err)
	}
	if _, err := av.CreateProject(ctx, "beta", ""); err != nil {
		t.Fatalf("create beta: %v", err)
	}

	// Unauthenticated => 401.
	rec := doControl(t, av, http.MethodGet, "/api/projects/alpha/files", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauth list: status %d, want 401", rec.Code)
	}

	// A collaborator granted only "alpha".
	token, err := av.CreateInvitation(ctx, "collab@example.com", "alpha")
	if err != nil {
		t.Fatalf("invite: %v", err)
	}
	if _, _, err := av.AcceptInvitation(ctx, token, "collabpass1"); err != nil {
		t.Fatalf("accept: %v", err)
	}
	collab := loginCollaborator(t, av, "collab@example.com", "collabpass1")

	// Granted project => allowed.
	rec = doControl(t, av, http.MethodGet, "/api/projects/alpha/files", nil, collab)
	if rec.Code != http.StatusOK {
		t.Fatalf("collab granted list: status %d body %s", rec.Code, rec.Body.String())
	}

	// Non-granted project => 403.
	rec = doControl(t, av, http.MethodGet, "/api/projects/beta/files", nil, collab)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("collab ungranted list: status %d, want 403", rec.Code)
	}
}

// TestFilesMissingProject ensures file endpoints 404 for unknown projects.
func TestFilesMissingProject(t *testing.T) {
	av := newTestAviary(t)
	sess := loginAs(t, av, "admin@example.com", "password123")
	rec := doControl(t, av, http.MethodGet, "/api/projects/ghost/files", nil, sess)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("ghost list: status %d, want 404", rec.Code)
	}
}

// TestFilesServedAtSubdomain is an integration test: a file written through the
// control-plane API is served live at the project's public URL.
func TestFilesServedAtSubdomain(t *testing.T) {
	av := newTestAviary(t)
	sess := loginAs(t, av, "admin@example.com", "password123")
	doControl(t, av, http.MethodPost, "/api/projects", createProjectRequest{ID: "alpha"}, sess)

	const html = "<h1>hello from aviary</h1>"
	rec := doControl(t, av, http.MethodPut, "/api/projects/alpha/files/content",
		fileContent{Path: "index.html", Content: html}, sess)
	if rec.Code != http.StatusOK {
		t.Fatalf("write index.html: status %d body %s", rec.Code, rec.Body.String())
	}

	// Fetch it on the project subdomain. The static file server canonicalizes
	// "/index.html" to "/", so request the directory root directly.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "alpha.localhost"
	pub := httptest.NewRecorder()
	av.ServeHTTP(pub, req)
	if pub.Code != http.StatusOK {
		t.Fatalf("serve index.html: status %d body %s", pub.Code, pub.Body.String())
	}
	if pub.Body.String() != html {
		t.Fatalf("served body = %q, want %q", pub.Body.String(), html)
	}

	// The REST API must still win over the static fallback.
	api := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	api.Host = "alpha.localhost"
	apiRec := httptest.NewRecorder()
	av.ServeHTTP(apiRec, api)
	if apiRec.Code != http.StatusOK {
		t.Fatalf("api/health after static route: status %d", apiRec.Code)
	}
}

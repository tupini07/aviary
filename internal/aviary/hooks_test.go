package aviary

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// TestHooksCRUD exercises the happy path for the pb_hooks editor: write, list,
// read and delete a hook file through the control-plane API.
func TestHooksCRUD(t *testing.T) {
	av := newTestAviary(t)
	sess := loginAs(t, av, "admin@example.com", "password123")
	doControl(t, av, http.MethodPost, "/api/projects", createProjectRequest{ID: "alpha"}, sess)

	// Empty project => empty listing.
	rec := doControl(t, av, http.MethodGet, "/api/projects/alpha/hooks", nil, sess)
	if rec.Code != http.StatusOK {
		t.Fatalf("list empty: status %d body %s", rec.Code, rec.Body.String())
	}
	var list []fileEntry
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list) != 0 {
		t.Fatalf("expected empty listing, got %+v", list)
	}

	// Write a hook file.
	body := fileContent{Path: "main.pb.js", Content: `routerAdd("GET", "/x", (e) => e.json(200, {}))`}
	rec = doControl(t, av, http.MethodPut, "/api/projects/alpha/hooks/content", body, sess)
	if rec.Code != http.StatusOK {
		t.Fatalf("write: status %d body %s", rec.Code, rec.Body.String())
	}

	// List should now include it.
	rec = doControl(t, av, http.MethodGet, "/api/projects/alpha/hooks", nil, sess)
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list) != 1 || list[0].Path != "main.pb.js" {
		t.Fatalf("listing = %+v, want main.pb.js", list)
	}

	// Read it back.
	rec = doControl(t, av, http.MethodGet, "/api/projects/alpha/hooks/content?path=main.pb.js", nil, sess)
	if rec.Code != http.StatusOK {
		t.Fatalf("read: status %d body %s", rec.Code, rec.Body.String())
	}
	var got fileContent
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.Content != body.Content {
		t.Fatalf("read content = %q", got.Content)
	}

	// Delete it.
	rec = doControl(t, av, http.MethodDelete, "/api/projects/alpha/hooks/content?path=main.pb.js", nil, sess)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete: status %d body %s", rec.Code, rec.Body.String())
	}
	rec = doControl(t, av, http.MethodGet, "/api/projects/alpha/hooks/content?path=main.pb.js", nil, sess)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("read after delete: status %d, want 404", rec.Code)
	}
}

// TestHooksPathTraversalRejected ensures the hooks editor refuses to escape the
// pb_hooks directory, exactly like the pb_public editor.
func TestHooksPathTraversalRejected(t *testing.T) {
	av := newTestAviary(t)
	sess := loginAs(t, av, "admin@example.com", "password123")
	doControl(t, av, http.MethodPost, "/api/projects", createProjectRequest{ID: "alpha"}, sess)

	escape := "../../secret.txt"
	rec := doControl(t, av, http.MethodPut, "/api/projects/alpha/hooks/content",
		fileContent{Path: escape, Content: "x"}, sess)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("write traversal: status %d, want 400", rec.Code)
	}
	q := url.QueryEscape(escape)
	rec = doControl(t, av, http.MethodGet, "/api/projects/alpha/hooks/content?path="+q, nil, sess)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("read traversal: status %d, want 400", rec.Code)
	}
	rec = doControl(t, av, http.MethodDelete, "/api/projects/alpha/hooks/content?path="+q, nil, sess)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("delete traversal: status %d, want 400", rec.Code)
	}
}

// TestHooksRejectAPIKey verifies the hooks editor is owner-only: a valid
// project-scoped API key (which may write pb_public files) must not be able to
// read or write hooks, because hook files run server-side.
func TestHooksRejectAPIKey(t *testing.T) {
	av := newTestAviary(t)
	sess := loginAs(t, av, "admin@example.com", "password123")
	doControl(t, av, http.MethodPost, "/api/projects", createProjectRequest{ID: "alpha"}, sess)
	key := mintKey(t, av, "alpha", sess, createAPIKeyRequest{Label: "ci"})

	// Sanity: the key works for pb_public files.
	rec := doControlBearer(t, av, http.MethodGet, "/api/projects/alpha/files", nil, key.Token)
	if rec.Code != http.StatusOK {
		t.Fatalf("bearer files list: status %d body %s", rec.Code, rec.Body.String())
	}

	// But every hooks operation must be forbidden for an API key.
	rec = doControlBearer(t, av, http.MethodGet, "/api/projects/alpha/hooks", nil, key.Token)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("bearer hooks list: status %d, want 403", rec.Code)
	}
	rec = doControlBearer(t, av, http.MethodPut, "/api/projects/alpha/hooks/content",
		fileContent{Path: "main.pb.js", Content: "x"}, key.Token)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("bearer hooks write: status %d, want 403", rec.Code)
	}
	rec = doControlBearer(t, av, http.MethodDelete, "/api/projects/alpha/hooks/content?path=main.pb.js", nil, key.Token)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("bearer hooks delete: status %d, want 403", rec.Code)
	}
}

// TestHookRouteExecuted is the end-to-end proof that JS hooks are actually
// loaded and run: it writes a hook registering a custom route, then hits that
// route on the project subdomain and asserts the hook produced the response.
// This also exercises the eviction-based reload (the write reboots the cage).
func TestHookRouteExecuted(t *testing.T) {
	av := newTestAviary(t)
	sess := loginAs(t, av, "admin@example.com", "password123")
	doControl(t, av, http.MethodPost, "/api/projects", createProjectRequest{ID: "alpha"}, sess)

	hook := `routerAdd("GET", "/hello", (e) => e.json(200, { msg: "from-hook" }))`
	rec := doControl(t, av, http.MethodPut, "/api/projects/alpha/hooks/content",
		fileContent{Path: "main.pb.js", Content: hook}, sess)
	if rec.Code != http.StatusOK {
		t.Fatalf("write hook: status %d body %s", rec.Code, rec.Body.String())
	}

	// Boot the project by requesting the custom route on its subdomain.
	req := httptest.NewRequest(http.MethodGet, "/hello", nil)
	req.Host = "alpha.localhost"
	w := httptest.NewRecorder()
	av.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /hello: status %d body %s", w.Code, w.Body.String())
	}
	var got struct {
		Msg string `json:"msg"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.Msg != "from-hook" {
		t.Fatalf("hook response = %s, want msg=from-hook", w.Body.String())
	}
}

// TestProjectBootsWithoutHooks guards the jsvm self-disable path: a project with
// no hook files must still boot and serve normally.
func TestProjectBootsWithoutHooks(t *testing.T) {
	av := newTestAviary(t)
	sess := loginAs(t, av, "admin@example.com", "password123")
	doControl(t, av, http.MethodPost, "/api/projects", createProjectRequest{ID: "alpha"}, sess)

	req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	req.Host = "alpha.localhost"
	w := httptest.NewRecorder()
	av.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("api/health on hookless project: status %d body %s", w.Code, w.Body.String())
	}
}

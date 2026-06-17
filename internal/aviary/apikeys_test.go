package aviary

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// doControlBearer issues a control-plane request authenticated with an API key
// (Authorization: Bearer) instead of a session cookie.
func doControlBearer(t *testing.T, av *Aviary, method, path string, body any, token string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Host = "localhost"
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	av.ServeHTTP(rec, req)
	return rec
}

// createAPIKey is the decoded one-time key-creation response.
type createdKeyResp struct {
	ID        string `json:"id"`
	ProjectID string `json:"projectId"`
	Label     string `json:"label"`
	Token     string `json:"token"`
}

func mintKey(t *testing.T, av *Aviary, project string, sess *http.Cookie, body any) createdKeyResp {
	t.Helper()
	rec := doControl(t, av, http.MethodPost, "/api/projects/"+project+"/keys", body, sess)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create key: status %d body %s", rec.Code, rec.Body.String())
	}
	var out createdKeyResp
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode key: %v", err)
	}
	if out.Token == "" || out.ID == "" {
		t.Fatalf("expected token and id, got %+v", out)
	}
	return out
}

// TestAPIKeyDeployFlow covers the headline use case: a superuser mints a
// project-scoped key, and an agent uses it (Bearer) to publish and read files.
func TestAPIKeyDeployFlow(t *testing.T) {
	av := newTestAviary(t)
	sess := loginAs(t, av, "admin@example.com", "password123")
	doControl(t, av, http.MethodPost, "/api/projects", createProjectRequest{ID: "alpha"}, sess)

	key := mintKey(t, av, "alpha", sess, createAPIKeyRequest{Label: "ci"})

	// List shows metadata but never the token.
	rec := doControl(t, av, http.MethodGet, "/api/projects/alpha/keys", nil, sess)
	if rec.Code != http.StatusOK {
		t.Fatalf("list keys: status %d body %s", rec.Code, rec.Body.String())
	}
	if bytes.Contains(rec.Body.Bytes(), []byte(key.Token)) {
		t.Fatal("listing leaked the raw token")
	}
	var keys []APIKey
	_ = json.Unmarshal(rec.Body.Bytes(), &keys)
	if len(keys) != 1 || keys[0].Label != "ci" {
		t.Fatalf("keys = %+v, want one labeled ci", keys)
	}

	// Use the key (Bearer) to write a file.
	rec = doControlBearer(t, av, http.MethodPut, "/api/projects/alpha/files/content",
		fileContent{Path: "index.html", Content: "<h1>hi</h1>"}, key.Token)
	if rec.Code != http.StatusOK {
		t.Fatalf("bearer write: status %d body %s", rec.Code, rec.Body.String())
	}
	// And to read it back.
	rec = doControlBearer(t, av, http.MethodGet, "/api/projects/alpha/files/content?path=index.html", nil, key.Token)
	if rec.Code != http.StatusOK {
		t.Fatalf("bearer read: status %d body %s", rec.Code, rec.Body.String())
	}
	var got fileContent
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.Content != "<h1>hi</h1>" {
		t.Fatalf("bearer read content = %q", got.Content)
	}

	// Revoke it; the same token must stop working.
	rec = doControl(t, av, http.MethodDelete, "/api/projects/alpha/keys/"+key.ID, nil, sess)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("revoke: status %d body %s", rec.Code, rec.Body.String())
	}
	rec = doControlBearer(t, av, http.MethodGet, "/api/projects/alpha/files", nil, key.Token)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("revoked key still works: status %d body %s", rec.Code, rec.Body.String())
	}
}

// TestAPIKeyScopeEnforced verifies a key bound to one project cannot touch
// another project.
func TestAPIKeyScopeEnforced(t *testing.T) {
	av := newTestAviary(t)
	sess := loginAs(t, av, "admin@example.com", "password123")
	doControl(t, av, http.MethodPost, "/api/projects", createProjectRequest{ID: "alpha"}, sess)
	doControl(t, av, http.MethodPost, "/api/projects", createProjectRequest{ID: "beta"}, sess)

	key := mintKey(t, av, "alpha", sess, createAPIKeyRequest{Label: "alpha-only"})

	// Bearer against beta's files must be forbidden.
	rec := doControlBearer(t, av, http.MethodGet, "/api/projects/beta/files", nil, key.Token)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-project access: status %d body %s", rec.Code, rec.Body.String())
	}
	// But its own project is fine.
	rec = doControlBearer(t, av, http.MethodGet, "/api/projects/alpha/files", nil, key.Token)
	if rec.Code != http.StatusOK {
		t.Fatalf("own-project access: status %d body %s", rec.Code, rec.Body.String())
	}
}

// TestAPIKeyCannotManageKeys verifies a key cannot mint or revoke keys (no
// privilege escalation from a leaked deploy key).
func TestAPIKeyCannotManageKeys(t *testing.T) {
	av := newTestAviary(t)
	sess := loginAs(t, av, "admin@example.com", "password123")
	doControl(t, av, http.MethodPost, "/api/projects", createProjectRequest{ID: "alpha"}, sess)
	key := mintKey(t, av, "alpha", sess, createAPIKeyRequest{Label: "ci"})

	rec := doControlBearer(t, av, http.MethodGet, "/api/projects/alpha/keys", nil, key.Token)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("key listed keys: status %d body %s", rec.Code, rec.Body.String())
	}
	rec = doControlBearer(t, av, http.MethodPost, "/api/projects/alpha/keys", createAPIKeyRequest{Label: "evil"}, key.Token)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("key minted key: status %d body %s", rec.Code, rec.Body.String())
	}
	rec = doControlBearer(t, av, http.MethodDelete, "/api/projects/alpha/keys/"+key.ID, nil, key.Token)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("key revoked key: status %d body %s", rec.Code, rec.Body.String())
	}
}

// TestAPIKeyExpired verifies an expired key is rejected.
func TestAPIKeyExpired(t *testing.T) {
	av := newTestAviary(t)
	sess := loginAs(t, av, "admin@example.com", "password123")
	doControl(t, av, http.MethodPost, "/api/projects", createProjectRequest{ID: "alpha"}, sess)

	// Insert an already-expired key directly into the store.
	token, id, err := generateAPIKeyToken()
	if err != nil {
		t.Fatalf("gen token: %v", err)
	}
	past := time.Now().Add(-time.Hour)
	if err := av.store.CreateAPIKey(context.Background(), id, "alpha", "stale", hashToken(token), &past); err != nil {
		t.Fatalf("create expired key: %v", err)
	}
	rec := doControlBearer(t, av, http.MethodGet, "/api/projects/alpha/files", nil, token)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expired key accepted: status %d body %s", rec.Code, rec.Body.String())
	}
}

// TestAPIKeyCollaboratorCanManage verifies a collaborator granted the project
// can mint keys (key management is owner-level, not superuser-only).
func TestAPIKeyCollaboratorCanManage(t *testing.T) {
	av := newTestAviary(t)
	sess := loginAs(t, av, "admin@example.com", "password123")
	doControl(t, av, http.MethodPost, "/api/projects", createProjectRequest{ID: "alpha"}, sess)

	token, err := av.CreateInvitation(context.Background(), "dev@example.com", "alpha")
	if err != nil {
		t.Fatalf("invite: %v", err)
	}
	if _, _, err := av.AcceptInvitation(context.Background(), token, "devpassword"); err != nil {
		t.Fatalf("accept: %v", err)
	}
	collab := loginCollaborator(t, av, "dev@example.com", "devpassword")

	rec := doControl(t, av, http.MethodPost, "/api/projects/alpha/keys", createAPIKeyRequest{Label: "from-collab"}, collab)
	if rec.Code != http.StatusCreated {
		t.Fatalf("collaborator create key: status %d body %s", rec.Code, rec.Body.String())
	}
}

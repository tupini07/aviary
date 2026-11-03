package aviary

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

type adminTokenResp struct {
	Token          string `json:"token"`
	SuperuserEmail string `json:"superuserEmail"`
	APIURL         string `json:"apiURL"`
	Issued         string `json:"issued"`
}

// TestProjectAdminTokenMint covers minting a PocketBase superuser token for a
// project via the control plane — the brute-force-proof alternative to native
// password login, used by migrations/CI. Both a session and a project-scoped
// API key may mint, and the token must authenticate against the project's
// PocketBase API as a superuser.
func TestProjectAdminTokenMint(t *testing.T) {
	av := newTestAviary(t)
	sess := loginAs(t, av, "admin@example.com", "password123")
	doControl(t, av, http.MethodPost, "/api/projects", createProjectRequest{ID: "alpha"}, sess)

	mint := func(rec *httptest.ResponseRecorder) adminTokenResp {
		t.Helper()
		if rec.Code != http.StatusOK {
			t.Fatalf("mint admin-token: status %d body %s", rec.Code, rec.Body.String())
		}
		var out adminTokenResp
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode token: %v", err)
		}
		if out.Token == "" {
			t.Fatalf("empty token: %+v", out)
		}
		return out
	}

	// 1) Session superuser can mint, and the token authenticates against the
	// project's PocketBase API as a superuser.
	sessTok := mint(doControl(t, av, http.MethodPost, "/api/projects/alpha/admin-token", nil, sess))
	if sessTok.SuperuserEmail != "admin@example.com" {
		t.Fatalf("superuserEmail = %q", sessTok.SuperuserEmail)
	}
	assertTokenIsSuperuser(t, av, "alpha", sessTok.Token)

	// 2) A project-scoped API key can also mint (the automation path).
	key := mintKey(t, av, "alpha", sess, createAPIKeyRequest{Label: "migration"})
	keyTok := mint(doControlBearer(t, av, http.MethodPost, "/api/projects/alpha/admin-token", nil, key.Token))
	assertTokenIsSuperuser(t, av, "alpha", keyTok.Token)
}

func TestProjectAdminTokenCollectionWrites(t *testing.T) {
	av := newTestAviary(t)
	sess := loginAs(t, av, "admin@example.com", "password123")
	doControl(t, av, http.MethodPost, "/api/projects", createProjectRequest{ID: "alpha"}, sess)
	key := mintKey(t, av, "alpha", sess, createAPIKeyRequest{Label: "migration"})
	rec := doControlBearer(t, av, http.MethodPost, "/api/projects/alpha/admin-token", nil, key.Token)
	var token adminTokenResp
	if rec.Code != http.StatusOK {
		t.Fatalf("mint admin-token: status %d body %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &token); err != nil {
		t.Fatal(err)
	}

	type collectionResponse struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Type string `json:"type"`
	}
	write := func(t *testing.T, method, path string, body map[string]any) collectionResponse {
		t.Helper()
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(method, path, bytes.NewReader(data))
		req.Host = "alpha.localhost"
		req.Header.Set("Authorization", "Bearer "+token.Token)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		av.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s %s: status %d body %s", method, path, rec.Code, rec.Body.String())
		}
		var result collectionResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}

	for _, kind := range []string{"base", "auth"} {
		t.Run(kind, func(t *testing.T) {
			created := write(t, http.MethodPost, "/api/collections", map[string]any{
				"name": "write_test_" + kind, "type": kind,
				"fields": []map[string]any{{"name": "label", "type": "text"}},
			})
			if created.ID == "" || created.Name != "write_test_"+kind || created.Type != kind {
				t.Fatalf("unexpected created collection: %+v", created)
			}
			updated := write(t, http.MethodPatch, "/api/collections/"+created.ID, map[string]any{
				"name": "updated_" + kind,
			})
			if updated.ID != created.ID || updated.Name != "updated_"+kind || updated.Type != kind {
				t.Fatalf("unexpected updated collection: %+v", updated)
			}
		})
	}
	assertTokenIsSuperuser(t, av, "alpha", token.Token)
}

// TestProjectAdminTokenScope verifies a key bound to one project cannot mint a
// token for another, and unknown projects / unauthenticated callers are refused.
func TestProjectAdminTokenScope(t *testing.T) {
	av := newTestAviary(t)
	sess := loginAs(t, av, "admin@example.com", "password123")
	doControl(t, av, http.MethodPost, "/api/projects", createProjectRequest{ID: "alpha"}, sess)
	doControl(t, av, http.MethodPost, "/api/projects", createProjectRequest{ID: "beta"}, sess)

	betaKey := mintKey(t, av, "beta", sess, createAPIKeyRequest{Label: "x"})
	rec := doControlBearer(t, av, http.MethodPost, "/api/projects/alpha/admin-token", nil, betaKey.Token)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-project mint: status %d, want 403", rec.Code)
	}

	rec = doControl(t, av, http.MethodPost, "/api/projects/ghost/admin-token", nil, sess)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown project: status %d, want 404", rec.Code)
	}

	rec = doControl(t, av, http.MethodPost, "/api/projects/alpha/admin-token", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauth mint: status %d, want 401", rec.Code)
	}
}

// assertTokenIsSuperuser confirms the given token authenticates against the
// project's PocketBase API by listing collections (a superuser-only endpoint).
func assertTokenIsSuperuser(t *testing.T, av *Aviary, project, token string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/collections", nil)
	req.Host = project + ".localhost"
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	av.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("minted token rejected by PB API: status %d body %s", rec.Code, rec.Body.String())
	}
}

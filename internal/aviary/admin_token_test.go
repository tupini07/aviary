package aviary

import (
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

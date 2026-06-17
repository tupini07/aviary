package aviary

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	virtualwebauthn "github.com/descope/virtualwebauthn"
	"github.com/pocketbase/pocketbase/core"
)

// bootProject sends a request that boots the named project and returns its
// running PocketBase app.
func bootProject(t *testing.T, av *Aviary, id string) core.App {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	req.Host = id + ".localhost"
	av.ServeHTTP(httptest.NewRecorder(), req)

	av.mu.Lock()
	c := av.cages[id]
	av.mu.Unlock()
	if c == nil {
		t.Fatalf("project %q did not boot", id)
	}
	<-c.ready
	if c.startErr != nil {
		t.Fatalf("project %q boot error: %v", id, c.startErr)
	}
	return c.app
}

// doProject issues a request to a project (by subdomain) through the Aviary
// front, optionally with a bearer token.
func doProject(t *testing.T, av *Aviary, id, method, path, bearer string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	var r *bytes.Reader
	if body != nil {
		r = bytes.NewReader(body)
	} else {
		r = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, r)
	req.Host = id + ".localhost"
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	av.ServeHTTP(rec, req)
	return rec
}

// createUser provisions a verified user record in the project's "users"
// collection and returns the record plus a valid auth token.
func createUser(t *testing.T, app core.App, email, password string) (*core.Record, string) {
	t.Helper()
	col, err := app.FindCollectionByNameOrId("users")
	if err != nil {
		t.Fatalf("find users collection: %v", err)
	}
	rec := core.NewRecord(col)
	rec.SetEmail(email)
	rec.SetPassword(password)
	rec.SetVerified(true)
	rec.Set("name", "Test User")
	if err := app.Save(rec); err != nil {
		t.Fatalf("save user: %v", err)
	}
	token, err := rec.NewAuthToken()
	if err != nil {
		t.Fatalf("new auth token: %v", err)
	}
	return rec, token
}

func TestPasskeyRegisterAndLogin(t *testing.T) {
	av := newTestAviary(t)
	ctx := context.Background()
	if _, err := av.CreateProject(ctx, "alpha", "Alpha"); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	app := bootProject(t, av, "alpha")
	user, token := createUser(t, app, "user@example.com", "password123")

	// Virtual relying party must match what the server derives from the Host
	// header ("alpha.localhost", http scheme, no port in httptest).
	rp := virtualwebauthn.RelyingParty{
		ID:     "alpha.localhost",
		Name:   "Aviary",
		Origin: "http://alpha.localhost",
	}
	authenticator := virtualwebauthn.NewAuthenticator()
	credential := virtualwebauthn.NewCredential(virtualwebauthn.KeyTypeEC2)

	// --- registration ---
	rec := doProject(t, av, "alpha", http.MethodPost, "/api/aviary/passkey/register/begin", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("register/begin: status %d body %s", rec.Code, rec.Body.String())
	}
	regBegin := struct {
		Token     string          `json:"token"`
		PublicKey json.RawMessage `json:"publicKey"`
	}{}
	mustJSON(t, rec.Body.Bytes(), &regBegin)

	attOpts, err := virtualwebauthn.ParseAttestationOptions(wrapPublicKey(regBegin.PublicKey))
	if err != nil {
		t.Fatalf("parse attestation options: %v", err)
	}
	attResp := virtualwebauthn.CreateAttestationResponse(rp, authenticator, credential, *attOpts)

	finishBody, _ := json.Marshal(map[string]any{
		"token":    regBegin.Token,
		"label":    "My Laptop",
		"response": json.RawMessage(attResp),
	})
	rec = doProject(t, av, "alpha", http.MethodPost, "/api/aviary/passkey/register/finish", token, finishBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("register/finish: status %d body %s", rec.Code, rec.Body.String())
	}
	authenticator.AddCredential(credential)
	authenticator.Options.UserHandle = []byte(user.Id)

	// --- discoverable login (no bearer token) ---
	rec = doProject(t, av, "alpha", http.MethodPost, "/api/aviary/passkey/login/begin", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("login/begin: status %d body %s", rec.Code, rec.Body.String())
	}
	loginBegin := struct {
		Token     string          `json:"token"`
		PublicKey json.RawMessage `json:"publicKey"`
	}{}
	mustJSON(t, rec.Body.Bytes(), &loginBegin)

	assOpts, err := virtualwebauthn.ParseAssertionOptions(wrapPublicKey(loginBegin.PublicKey))
	if err != nil {
		t.Fatalf("parse assertion options: %v", err)
	}
	assResp := virtualwebauthn.CreateAssertionResponse(rp, authenticator, credential, *assOpts)

	loginFinishBody, _ := json.Marshal(map[string]any{
		"token":    loginBegin.Token,
		"response": json.RawMessage(assResp),
	})
	rec = doProject(t, av, "alpha", http.MethodPost, "/api/aviary/passkey/login/finish", "", loginFinishBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("login/finish: status %d body %s", rec.Code, rec.Body.String())
	}
	var authResp struct {
		Token  string `json:"token"`
		Record struct {
			ID    string `json:"id"`
			Email string `json:"email"`
		} `json:"record"`
	}
	mustJSON(t, rec.Body.Bytes(), &authResp)
	if authResp.Token == "" {
		t.Fatal("expected a PocketBase auth token from passkey login")
	}
	if authResp.Record.ID != user.Id || authResp.Record.Email != "user@example.com" {
		t.Fatalf("unexpected auth record: %+v", authResp.Record)
	}
}

func TestPasskeyRegisterRequiresAuth(t *testing.T) {
	av := newTestAviary(t)
	if _, err := av.CreateProject(context.Background(), "alpha", ""); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	bootProject(t, av, "alpha")

	// No bearer token => must be rejected.
	rec := doProject(t, av, "alpha", http.MethodPost, "/api/aviary/passkey/register/begin", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("register/begin without auth: status %d, want 401", rec.Code)
	}
}

func TestPasskeyLoginFinishRejectsBadToken(t *testing.T) {
	av := newTestAviary(t)
	if _, err := av.CreateProject(context.Background(), "alpha", ""); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	bootProject(t, av, "alpha")

	body, _ := json.Marshal(map[string]any{"token": "bogus", "response": json.RawMessage(`{}`)})
	rec := doProject(t, av, "alpha", http.MethodPost, "/api/aviary/passkey/login/finish", "", body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("login/finish with bad token: status %d, want 400", rec.Code)
	}
}

// wrapPublicKey wraps the inner options object as {"publicKey": <obj>} so the
// virtualwebauthn parsers (which accept either form) get a canonical shape.
func wrapPublicKey(inner json.RawMessage) string {
	b, _ := json.Marshal(map[string]json.RawMessage{"publicKey": inner})
	return string(b)
}

// registerPasskey runs the full enrollment ceremony for an authenticated user
// and returns the registered credential id. It mirrors the registration steps
// in TestPasskeyRegisterAndLogin and is used by the management tests.
func registerPasskey(t *testing.T, av *Aviary, id, token string, label string) string {
	t.Helper()
	rp := virtualwebauthn.RelyingParty{ID: id + ".localhost", Name: "Aviary", Origin: "http://" + id + ".localhost"}
	authenticator := virtualwebauthn.NewAuthenticator()
	credential := virtualwebauthn.NewCredential(virtualwebauthn.KeyTypeEC2)

	rec := doProject(t, av, id, http.MethodPost, "/api/aviary/passkey/register/begin", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("register/begin: status %d body %s", rec.Code, rec.Body.String())
	}
	var begin struct {
		Token     string          `json:"token"`
		PublicKey json.RawMessage `json:"publicKey"`
	}
	mustJSON(t, rec.Body.Bytes(), &begin)

	attOpts, err := virtualwebauthn.ParseAttestationOptions(wrapPublicKey(begin.PublicKey))
	if err != nil {
		t.Fatalf("parse attestation options: %v", err)
	}
	attResp := virtualwebauthn.CreateAttestationResponse(rp, authenticator, credential, *attOpts)
	finishBody, _ := json.Marshal(map[string]any{"token": begin.Token, "label": label, "response": json.RawMessage(attResp)})
	rec = doProject(t, av, id, http.MethodPost, "/api/aviary/passkey/register/finish", token, finishBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("register/finish: status %d body %s", rec.Code, rec.Body.String())
	}
	var finish struct {
		CredentialID string `json:"credentialId"`
	}
	mustJSON(t, rec.Body.Bytes(), &finish)
	if finish.CredentialID == "" {
		t.Fatal("register/finish returned empty credential id")
	}
	return finish.CredentialID
}

// TestPasskeyListAndDelete verifies a user can list and remove their own
// passkeys, and cannot delete another user's passkey.
func TestPasskeyListAndDelete(t *testing.T) {
	av := newTestAviary(t)
	ctx := context.Background()
	if _, err := av.CreateProject(ctx, "alpha", "Alpha"); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	app := bootProject(t, av, "alpha")
	_, token := createUser(t, app, "user@example.com", "password123")
	_, otherToken := createUser(t, app, "other@example.com", "password123")

	credID := registerPasskey(t, av, "alpha", token, "My Laptop")

	// List shows exactly the one enrolled passkey, with no credential material.
	rec := doProject(t, av, "alpha", http.MethodGet, "/api/aviary/passkey", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: status %d body %s", rec.Code, rec.Body.String())
	}
	var list []struct {
		CredentialID string `json:"credentialId"`
		Label        string `json:"label"`
		Created      string `json:"created"`
	}
	mustJSON(t, rec.Body.Bytes(), &list)
	if len(list) != 1 || list[0].CredentialID != credID || list[0].Label != "My Laptop" {
		t.Fatalf("unexpected list: %+v", list)
	}
	if list[0].Created == "" {
		t.Error("expected a created timestamp")
	}

	// Listing requires authentication.
	if rec := doProject(t, av, "alpha", http.MethodGet, "/api/aviary/passkey", "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("list without auth: status %d, want 401", rec.Code)
	}

	// A different user must not be able to delete this user's passkey.
	if rec := doProject(t, av, "alpha", http.MethodDelete, "/api/aviary/passkey/"+credID, otherToken, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("cross-user delete: status %d, want 404", rec.Code)
	}
	// It must still be present for the owner.
	rec = doProject(t, av, "alpha", http.MethodGet, "/api/aviary/passkey", token, nil)
	mustJSON(t, rec.Body.Bytes(), &list)
	if len(list) != 1 {
		t.Fatalf("passkey should survive a foreign delete attempt, got %d", len(list))
	}

	// The owner deletes their passkey.
	if rec := doProject(t, av, "alpha", http.MethodDelete, "/api/aviary/passkey/"+credID, token, nil); rec.Code != http.StatusOK {
		t.Fatalf("owner delete: status %d body %s", rec.Code, rec.Body.String())
	}
	// Deleting again is a 404.
	if rec := doProject(t, av, "alpha", http.MethodDelete, "/api/aviary/passkey/"+credID, token, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("delete missing: status %d, want 404", rec.Code)
	}
	// List is now empty.
	rec = doProject(t, av, "alpha", http.MethodGet, "/api/aviary/passkey", token, nil)
	mustJSON(t, rec.Body.Bytes(), &list)
	if len(list) != 0 {
		t.Fatalf("expected empty list after delete, got %+v", list)
	}
}

func mustJSON(t *testing.T, data []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatalf("unmarshal: %v (body: %s)", err, string(data))
	}
}

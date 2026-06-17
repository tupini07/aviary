package aviary

import (
	"encoding/json"
	"net/http"
	"testing"

	virtualwebauthn "github.com/descope/virtualwebauthn"
)

func TestSuperuserPasskeyRegisterAndLogin(t *testing.T) {
	av := newTestAviary(t)
	sess := loginAs(t, av, "admin@example.com", "password123")

	// doControl uses Host "localhost", so the relying party is scoped there.
	rp := virtualwebauthn.RelyingParty{
		ID:     "localhost",
		Name:   "Aviary Control Plane",
		Origin: "http://localhost",
	}
	authenticator := virtualwebauthn.NewAuthenticator()
	credential := virtualwebauthn.NewCredential(virtualwebauthn.KeyTypeEC2)

	// --- registration (authenticated superuser) ---
	rec := doControl(t, av, http.MethodPost, "/api/auth/passkey/register/begin", nil, sess)
	if rec.Code != http.StatusOK {
		t.Fatalf("register/begin: status %d body %s", rec.Code, rec.Body.String())
	}
	var regBegin struct {
		Token     string          `json:"token"`
		PublicKey json.RawMessage `json:"publicKey"`
	}
	mustJSON(t, rec.Body.Bytes(), &regBegin)

	attOpts, err := virtualwebauthn.ParseAttestationOptions(wrapPublicKey(regBegin.PublicKey))
	if err != nil {
		t.Fatalf("parse attestation options: %v", err)
	}
	attResp := virtualwebauthn.CreateAttestationResponse(rp, authenticator, credential, *attOpts)

	rec = doControl(t, av, http.MethodPost, "/api/auth/passkey/register/finish", map[string]any{
		"token":    regBegin.Token,
		"label":    "YubiKey",
		"response": json.RawMessage(attResp),
	}, sess)
	if rec.Code != http.StatusOK {
		t.Fatalf("register/finish: status %d body %s", rec.Code, rec.Body.String())
	}
	authenticator.AddCredential(credential)
	authenticator.Options.UserHandle = []byte(superuserHandle)

	// The credential should now be listed.
	rec = doControl(t, av, http.MethodGet, "/api/auth/passkey", nil, sess)
	if rec.Code != http.StatusOK {
		t.Fatalf("list passkeys: status %d body %s", rec.Code, rec.Body.String())
	}
	var list []suPasskeyView
	mustJSON(t, rec.Body.Bytes(), &list)
	if len(list) != 1 || list[0].Label != "YubiKey" {
		t.Fatalf("unexpected passkey list: %+v", list)
	}

	// --- passwordless login (no session cookie) ---
	rec = doControl(t, av, http.MethodPost, "/api/auth/passkey/login/begin", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("login/begin: status %d body %s", rec.Code, rec.Body.String())
	}
	var loginBegin struct {
		Token     string          `json:"token"`
		PublicKey json.RawMessage `json:"publicKey"`
	}
	mustJSON(t, rec.Body.Bytes(), &loginBegin)

	assOpts, err := virtualwebauthn.ParseAssertionOptions(wrapPublicKey(loginBegin.PublicKey))
	if err != nil {
		t.Fatalf("parse assertion options: %v", err)
	}
	assResp := virtualwebauthn.CreateAssertionResponse(rp, authenticator, credential, *assOpts)

	rec = doControl(t, av, http.MethodPost, "/api/auth/passkey/login/finish", map[string]any{
		"token":    loginBegin.Token,
		"response": json.RawMessage(assResp),
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("login/finish: status %d body %s", rec.Code, rec.Body.String())
	}

	// A valid session cookie must be issued.
	var loginCookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			loginCookie = c
		}
	}
	if loginCookie == nil {
		t.Fatal("passkey login did not set a session cookie")
	}
	if email, role, ok := av.verifySession(loginCookie.Value); !ok || email != "admin@example.com" || role != roleSuperuser {
		t.Fatalf("session cookie invalid: email=%q role=%q ok=%v", email, role, ok)
	}

	// The issued cookie should authenticate a protected endpoint.
	rec = doControl(t, av, http.MethodGet, "/api/projects", nil, loginCookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("authed request with passkey session: status %d", rec.Code)
	}

	// --- delete the credential ---
	rec = doControl(t, av, http.MethodDelete, "/api/auth/passkey/"+list[0].CredentialID, nil, sess)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete passkey: status %d body %s", rec.Code, rec.Body.String())
	}
	rec = doControl(t, av, http.MethodGet, "/api/auth/passkey", nil, sess)
	mustJSON(t, rec.Body.Bytes(), &list)
	if len(list) != 0 {
		t.Fatalf("expected no passkeys after delete, got %d", len(list))
	}
}

func TestSuperuserPasskeyRegisterRequiresAuth(t *testing.T) {
	av := newTestAviary(t)
	// No session cookie => 401.
	rec := doControl(t, av, http.MethodPost, "/api/auth/passkey/register/begin", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("register/begin without auth: status %d, want 401", rec.Code)
	}
}

func TestSuperuserPasskeyLoginFinishRejectsBadToken(t *testing.T) {
	av := newTestAviary(t)
	loginAs(t, av, "admin@example.com", "password123")

	rec := doControl(t, av, http.MethodPost, "/api/auth/passkey/login/finish", map[string]any{
		"token":    "bogus",
		"response": json.RawMessage(`{}`),
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("login/finish with bad token: status %d, want 400", rec.Code)
	}
}

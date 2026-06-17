package aviary

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/tupini07/aviary/internal/controlplane"
)

// superuserHandle is the fixed WebAuthn user handle for the single control-plane
// superuser. Because there is exactly one superuser identity, the handle is
// constant and a discoverable login always resolves back to it.
const superuserHandle = "aviary-superuser"

// suPasskeyRoutePrefix is the base path for control-plane superuser passkey
// ceremonies.
const suPasskeyRoutePrefix = "/api/auth/passkey"

// suSessionTTL bounds how long a superuser passkey ceremony may stay in-flight.
const suSessionTTL = 5 * time.Minute

// suSessionStore holds in-flight WebAuthn ceremony state for the control-plane
// superuser, keyed by an opaque token handed to the browser.
type suSessionStore struct {
	mu  sync.Mutex
	m   map[string]suSessionEntry
	now func() time.Time
}

type suSessionEntry struct {
	data    *webauthn.SessionData
	expires time.Time
}

func newSUSessionStore() *suSessionStore {
	return &suSessionStore{m: make(map[string]suSessionEntry), now: time.Now}
}

func (s *suSessionStore) put(data *webauthn.SessionData) (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(buf)

	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for k, e := range s.m {
		if now.After(e.expires) {
			delete(s.m, k)
		}
	}
	s.m[token] = suSessionEntry{data: data, expires: now.Add(suSessionTTL)}
	return token, nil
}

func (s *suSessionStore) take(token string) (*webauthn.SessionData, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.m[token]
	if !ok {
		return nil, false
	}
	delete(s.m, token)
	if s.now().After(e.expires) {
		return nil, false
	}
	return e.data, true
}

// superuserWebAuthnUser adapts the control-plane superuser to webauthn.User.
type superuserWebAuthnUser struct {
	email string
	creds []webauthn.Credential
}

func (u *superuserWebAuthnUser) WebAuthnID() []byte          { return []byte(superuserHandle) }
func (u *superuserWebAuthnUser) WebAuthnName() string        { return u.email }
func (u *superuserWebAuthnUser) WebAuthnDisplayName() string { return u.email }
func (u *superuserWebAuthnUser) WebAuthnIcon() string        { return "" }
func (u *superuserWebAuthnUser) WebAuthnCredentials() []webauthn.Credential {
	return u.creds
}

// loadSuperuserWebAuthnUser builds the webauthn.User for the configured
// superuser, loading any stored credentials from the control store.
func (a *Aviary) loadSuperuserWebAuthnUser(ctx context.Context) (*superuserWebAuthnUser, error) {
	su, err := a.store.GetSuperuser(ctx)
	if err != nil {
		return nil, err
	}
	creds, err := a.loadSuperuserCredentials(ctx)
	if err != nil {
		return nil, err
	}
	return &superuserWebAuthnUser{email: su.Email, creds: creds}, nil
}

func (a *Aviary) loadSuperuserCredentials(ctx context.Context) ([]webauthn.Credential, error) {
	stored, err := a.store.ListSuperuserPasskeys(ctx)
	if err != nil {
		return nil, err
	}
	creds := make([]webauthn.Credential, 0, len(stored))
	for _, pk := range stored {
		var c webauthn.Credential
		if err := json.Unmarshal(pk.Data, &c); err != nil {
			return nil, err
		}
		creds = append(creds, c)
	}
	return creds, nil
}

// newControlWebAuthn builds a WebAuthn relying party for the control-plane host
// derived from the request, so passkeys are scoped to the dashboard domain.
func newControlWebAuthn(r *http.Request) (*webauthn.WebAuthn, error) {
	host := r.Host
	if h, _, ok := strings.Cut(host, ":"); ok {
		host = h
	}
	scheme := "http"
	if isHTTPS(r) {
		scheme = "https"
	}
	return webauthn.New(&webauthn.Config{
		RPID:          host,
		RPDisplayName: "Aviary Control Plane",
		RPOrigins:     []string{scheme + "://" + r.Host},
	})
}

// --- handlers ---

// suPasskeyRegisterBegin starts enrolling a passkey for the authenticated
// superuser.
func (a *Aviary) suPasskeyRegisterBegin(w http.ResponseWriter, r *http.Request) {
	wa, err := newControlWebAuthn(r)
	if err != nil {
		a.apiError(w, http.StatusInternalServerError, "passkey unavailable: "+err.Error())
		return
	}
	user, err := a.loadSuperuserWebAuthnUser(r.Context())
	if err != nil {
		a.apiError(w, http.StatusInternalServerError, err.Error())
		return
	}

	exclusions := make([]protocol.CredentialDescriptor, 0, len(user.creds))
	for _, c := range user.creds {
		exclusions = append(exclusions, c.Descriptor())
	}

	options, session, err := wa.BeginRegistration(user,
		webauthn.WithExclusions(exclusions),
		webauthn.WithResidentKeyRequirement(protocol.ResidentKeyRequirementPreferred),
	)
	if err != nil {
		a.apiError(w, http.StatusBadRequest, "could not begin registration: "+err.Error())
		return
	}
	token, err := a.suPasskeySessions.put(session)
	if err != nil {
		a.apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"token": token, "publicKey": options.Response})
}

type suPasskeyFinishRequest struct {
	Token    string          `json:"token"`
	Label    string          `json:"label"`
	Response json.RawMessage `json:"response"`
}

// suPasskeyRegisterFinish completes passkey enrollment for the superuser.
func (a *Aviary) suPasskeyRegisterFinish(w http.ResponseWriter, r *http.Request) {
	var req suPasskeyFinishRequest
	if !decodeJSON(w, r, &req, a) {
		return
	}
	session, ok := a.suPasskeySessions.take(req.Token)
	if !ok {
		a.apiError(w, http.StatusBadRequest, "unknown or expired ceremony token")
		return
	}

	wa, err := newControlWebAuthn(r)
	if err != nil {
		a.apiError(w, http.StatusInternalServerError, "passkey unavailable: "+err.Error())
		return
	}
	user, err := a.loadSuperuserWebAuthnUser(r.Context())
	if err != nil {
		a.apiError(w, http.StatusInternalServerError, err.Error())
		return
	}

	parsed, err := protocol.ParseCredentialCreationResponseBody(bytes.NewReader(req.Response))
	if err != nil {
		a.apiError(w, http.StatusBadRequest, "could not parse attestation: "+err.Error())
		return
	}
	cred, err := wa.CreateCredential(user, *session, parsed)
	if err != nil {
		a.apiError(w, http.StatusBadRequest, "could not verify attestation: "+err.Error())
		return
	}

	label := strings.TrimSpace(req.Label)
	if label == "" {
		label = "Passkey"
	}
	data, err := json.Marshal(cred)
	if err != nil {
		a.apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	credID := base64.RawURLEncoding.EncodeToString(cred.ID)
	if err := a.store.AddSuperuserPasskey(r.Context(), credID, label, data); err != nil {
		a.apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"registered": true, "credentialId": credID})
}

// suPasskeyLoginBegin starts a passwordless superuser login.
func (a *Aviary) suPasskeyLoginBegin(w http.ResponseWriter, r *http.Request) {
	wa, err := newControlWebAuthn(r)
	if err != nil {
		a.apiError(w, http.StatusInternalServerError, "passkey unavailable: "+err.Error())
		return
	}
	options, session, err := wa.BeginDiscoverableLogin()
	if err != nil {
		a.apiError(w, http.StatusBadRequest, "could not begin login: "+err.Error())
		return
	}
	token, err := a.suPasskeySessions.put(session)
	if err != nil {
		a.apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"token": token, "publicKey": options.Response})
}

// suPasskeyLoginFinish verifies a superuser passkey assertion and, on success,
// issues a control-plane session cookie.
func (a *Aviary) suPasskeyLoginFinish(w http.ResponseWriter, r *http.Request) {
	if ok, retry := a.loginLimit.allow(clientIP(r)); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
		a.apiError(w, http.StatusTooManyRequests, "too many login attempts; try again later")
		return
	}

	var req suPasskeyFinishRequest
	if !decodeJSON(w, r, &req, a) {
		return
	}
	session, ok := a.suPasskeySessions.take(req.Token)
	if !ok {
		a.apiError(w, http.StatusBadRequest, "unknown or expired ceremony token")
		return
	}

	wa, err := newControlWebAuthn(r)
	if err != nil {
		a.apiError(w, http.StatusInternalServerError, "passkey unavailable: "+err.Error())
		return
	}

	su, err := a.store.GetSuperuser(r.Context())
	if errors.Is(err, ErrNoSuperuser) {
		a.apiError(w, http.StatusUnauthorized, "passkey verification failed")
		return
	}
	if err != nil {
		a.apiError(w, http.StatusInternalServerError, err.Error())
		return
	}

	parsed, err := protocol.ParseCredentialRequestResponseBody(bytes.NewReader(req.Response))
	if err != nil {
		a.apiError(w, http.StatusBadRequest, "could not parse assertion: "+err.Error())
		return
	}

	user, err := a.loadSuperuserWebAuthnUser(r.Context())
	if err != nil {
		a.apiError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// The single superuser identity backs every credential, so the discoverable
	// handler resolves any returned user handle to it.
	handler := func(rawID, userHandle []byte) (webauthn.User, error) {
		if string(userHandle) != superuserHandle {
			return nil, errors.New("unknown user handle")
		}
		return user, nil
	}

	cred, err := wa.ValidateDiscoverableLogin(handler, *session, parsed)
	if err != nil {
		a.apiError(w, http.StatusUnauthorized, "passkey verification failed")
		return
	}

	// Persist the updated signature counter / clone-warning flags.
	if data, mErr := json.Marshal(cred); mErr == nil {
		credID := base64.RawURLEncoding.EncodeToString(cred.ID)
		if uErr := a.store.UpdateSuperuserPasskey(r.Context(), credID, data); uErr != nil {
			a.log.Warn("failed to update superuser passkey counter", "error", uErr)
		}
	}

	a.setSessionCookie(w, r, su.Email, roleSuperuser)
	writeJSON(w, http.StatusOK, map[string]any{"authenticated": true, "email": su.Email})
}

// suPasskeyView is the API representation of a stored superuser credential.
type suPasskeyView struct {
	CredentialID string `json:"credentialId"`
	Label        string `json:"label"`
	Created      string `json:"created"`
}

// suPasskeyList returns the superuser's registered passkeys.
func (a *Aviary) suPasskeyList(w http.ResponseWriter, r *http.Request) {
	stored, err := a.store.ListSuperuserPasskeys(r.Context())
	if err != nil {
		a.apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]suPasskeyView, 0, len(stored))
	for _, pk := range stored {
		out = append(out, suPasskeyView{
			CredentialID: pk.CredentialID,
			Label:        pk.Label,
			Created:      pk.CreatedAt.Format(time.RFC3339),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// suPasskeyDelete removes one of the superuser's registered passkeys.
func (a *Aviary) suPasskeyDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		a.apiError(w, http.StatusBadRequest, "credential id is required")
		return
	}
	if err := a.store.DeleteSuperuserPasskey(r.Context(), id); err != nil {
		if errors.Is(err, controlplane.ErrNotFound) {
			a.apiError(w, http.StatusNotFound, "passkey not found")
			return
		}
		a.apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

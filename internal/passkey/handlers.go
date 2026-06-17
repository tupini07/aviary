package passkey

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
)

// bindRoutes registers the passkey ceremony endpoints on the serve event.
func (m *Manager) bindRoutes(se *core.ServeEvent) {
	// Registration requires an already-authenticated user (they add a passkey
	// to their existing account).
	se.Router.POST(routePrefix+"/register/begin", m.registerBegin).
		Bind(apis.RequireAuth(UserCollection))
	se.Router.POST(routePrefix+"/register/finish", m.registerFinish).
		Bind(apis.RequireAuth(UserCollection))

	// Login is passwordless/discoverable: no prior auth needed.
	se.Router.POST(routePrefix+"/login/begin", m.loginBegin)
	se.Router.POST(routePrefix+"/login/finish", m.loginFinish)
}

// finishRequest is the shared body for the finish endpoints: the ceremony token
// returned by begin, plus the raw authenticator response.
type finishRequest struct {
	Token    string          `json:"token"`
	Label    string          `json:"label"`
	Response json.RawMessage `json:"response"`
}

func (m *Manager) registerBegin(e *core.RequestEvent) error {
	wa, err := newWebAuthn(e.Request)
	if err != nil {
		return e.InternalServerError("passkey unavailable", err)
	}
	user, err := newUser(m.app, e.Auth)
	if err != nil {
		return e.InternalServerError("could not load user", err)
	}

	// Exclude already-registered credentials so the same authenticator isn't
	// enrolled twice.
	exclusions := make([]protocol.CredentialDescriptor, 0, len(user.creds))
	for _, c := range user.creds {
		exclusions = append(exclusions, c.Descriptor())
	}

	options, session, err := wa.BeginRegistration(user,
		webauthn.WithExclusions(exclusions),
		webauthn.WithResidentKeyRequirement(protocol.ResidentKeyRequirementPreferred),
	)
	if err != nil {
		return e.BadRequestError("could not begin registration", err)
	}

	token, err := m.sessions.put(session)
	if err != nil {
		return e.InternalServerError("could not store ceremony", err)
	}
	return e.JSON(http.StatusOK, map[string]any{"token": token, "publicKey": options.Response})
}

func (m *Manager) registerFinish(e *core.RequestEvent) error {
	var req finishRequest
	if err := e.BindBody(&req); err != nil {
		return e.BadRequestError("invalid body", err)
	}
	session, ok := m.sessions.take(req.Token)
	if !ok {
		return e.BadRequestError("unknown or expired ceremony token", nil)
	}

	wa, err := newWebAuthn(e.Request)
	if err != nil {
		return e.InternalServerError("passkey unavailable", err)
	}
	user, err := newUser(m.app, e.Auth)
	if err != nil {
		return e.InternalServerError("could not load user", err)
	}

	parsed, err := protocol.ParseCredentialCreationResponseBody(bytes.NewReader(req.Response))
	if err != nil {
		return e.BadRequestError("could not parse attestation", err)
	}
	cred, err := wa.CreateCredential(user, *session, parsed)
	if err != nil {
		return e.BadRequestError("could not verify attestation", err)
	}

	label := req.Label
	if label == "" {
		label = "Passkey"
	}
	if err := saveCredential(m.app, e.Auth.Id, label, cred); err != nil {
		return e.InternalServerError("could not store credential", err)
	}
	return e.JSON(http.StatusOK, map[string]any{"registered": true, "credentialId": encodeID(cred.ID)})
}

func (m *Manager) loginBegin(e *core.RequestEvent) error {
	wa, err := newWebAuthn(e.Request)
	if err != nil {
		return e.InternalServerError("passkey unavailable", err)
	}

	options, session, err := wa.BeginDiscoverableLogin()
	if err != nil {
		return e.BadRequestError("could not begin login", err)
	}
	token, err := m.sessions.put(session)
	if err != nil {
		return e.InternalServerError("could not store ceremony", err)
	}
	return e.JSON(http.StatusOK, map[string]any{"token": token, "publicKey": options.Response})
}

func (m *Manager) loginFinish(e *core.RequestEvent) error {
	var req finishRequest
	if err := e.BindBody(&req); err != nil {
		return e.BadRequestError("invalid body", err)
	}
	session, ok := m.sessions.take(req.Token)
	if !ok {
		return e.BadRequestError("unknown or expired ceremony token", nil)
	}

	wa, err := newWebAuthn(e.Request)
	if err != nil {
		return e.InternalServerError("passkey unavailable", err)
	}

	parsed, err := protocol.ParseCredentialRequestResponseBody(bytes.NewReader(req.Response))
	if err != nil {
		return e.BadRequestError("could not parse assertion", err)
	}

	// The discoverable handler maps the authenticator-supplied user handle back
	// to its PocketBase record.
	var authedRecord *core.Record
	handler := func(rawID, userHandle []byte) (webauthn.User, error) {
		rec, err := m.app.FindRecordById(UserCollection, string(userHandle))
		if err != nil {
			return nil, errors.New("unknown user handle")
		}
		authedRecord = rec
		return newUser(m.app, rec)
	}

	cred, err := wa.ValidateDiscoverableLogin(handler, *session, parsed)
	if err != nil {
		return e.UnauthorizedError("passkey verification failed", err)
	}
	if authedRecord == nil {
		return e.UnauthorizedError("passkey verification failed", nil)
	}

	// Persist the updated signature counter / clone-warning flags.
	if err := updateCredential(m.app, authedRecord.Id, cred); err != nil {
		m.app.Logger().Warn("passkey: failed to update credential counter", "error", err)
	}

	return apis.RecordAuthResponse(e, authedRecord, "passkey", nil)
}

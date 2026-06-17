// Package passkey adds WebAuthn / passkey authentication to a PocketBase
// project. It exposes register and login ceremonies under
// /api/aviary/passkey/* and stores credentials in a dedicated collection.
//
// It is designed to be mounted onto any core.App (one per Aviary project) and
// to be upstreamable to PocketBase (see pocketbase/pocketbase#6800).
package passkey

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/pocketbase/pocketbase/core"
)

// CollectionName is the PocketBase collection that stores passkey credentials.
const CollectionName = "_passkeys"

// UserCollection is the auth collection whose records may own passkeys.
const UserCollection = "users"

// routePrefix is the base path for all passkey ceremony endpoints.
const routePrefix = "/api/aviary/passkey"

// Manager wires passkey endpoints and credential storage onto a single
// PocketBase app.
type Manager struct {
	app      core.App
	sessions *sessionStore
}

// New returns a Manager bound to app.
func New(app core.App) *Manager {
	return &Manager{app: app, sessions: newSessionStore()}
}

// Setup ensures the credentials collection exists and registers the passkey
// ceremony routes on the app's serve event. It should be called after the app
// is bootstrapped and before its HTTP handler is built, so the OnServe hook
// fires during handler construction.
func Setup(app core.App) error {
	m := New(app)
	app.OnServe().BindFunc(func(se *core.ServeEvent) error {
		if err := EnsureCollection(app); err != nil {
			return err
		}
		m.bindRoutes(se)
		return se.Next()
	})
	return nil
}

// newWebAuthn builds a per-request WebAuthn relying party derived from the
// request host, so that each project subdomain is its own RP. The RP ID is the
// host without a port; the origin includes the scheme and port.
func newWebAuthn(r *http.Request) (*webauthn.WebAuthn, error) {
	host := r.Host
	if h, _, ok := strings.Cut(host, ":"); ok {
		host = h
	}
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	origin := scheme + "://" + r.Host

	w, err := webauthn.New(&webauthn.Config{
		RPID:          host,
		RPDisplayName: "Aviary — " + host,
		RPOrigins:     []string{origin},
	})
	if err != nil {
		return nil, fmt.Errorf("passkey: webauthn config: %w", err)
	}
	return w, nil
}

package aviary

import (
	"crypto/hmac"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pocketbase/pocketbase/core"
)

// ssoTicketTTL bounds how long a freshly minted dashboard SSO ticket is valid.
// It only needs to survive a single browser redirect, so it is deliberately
// short to minimise the window in which a leaked ticket could be replayed.
const ssoTicketTTL = 60 * time.Second

// ssoPath is the project-subdomain path Aviary intercepts to complete a
// dashboard single-sign-on handoff. It is namespaced under /__aviary/ so it
// cannot collide with any PocketBase route.
const ssoPath = "/__aviary/sso"

// ticketStore records consumed SSO ticket nonces so a ticket can be redeemed at
// most once, even within its (short) validity window.
type ticketStore struct {
	mu   sync.Mutex
	used map[string]time.Time
	now  func() time.Time
}

func newTicketStore() *ticketStore {
	return &ticketStore{used: make(map[string]time.Time), now: time.Now}
}

// consume marks a nonce as used and reports whether it was still unused (and
// thus may be redeemed). Expired entries are pruned opportunistically.
func (s *ticketStore) consume(nonce string, expiry time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for k, exp := range s.used {
		if now.After(exp) {
			delete(s.used, k)
		}
	}
	if _, seen := s.used[nonce]; seen {
		return false
	}
	s.used[nonce] = expiry
	return true
}

// signSSOTicket builds an HMAC-signed, single-use ticket authorising the given
// identity to open the dashboard of a specific project. The ticket is bound to
// the email + project so it cannot be replayed against another project, and to
// a random nonce so it can be invalidated after a single redemption.
func (a *Aviary) signSSOTicket(email, projectID string, expiry time.Time) (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	nonce := base64.RawURLEncoding.EncodeToString(buf)
	payload := strings.Join([]string{email, projectID, strconv.FormatInt(expiry.Unix(), 10), nonce}, "|")
	mac := a.sessionMAC(payload)
	return base64.RawURLEncoding.EncodeToString([]byte(payload + "|" + mac)), nil
}

// verifySSOTicket validates an SSO ticket's signature, expiry and single-use
// nonce, returning the authorised email and project id on success.
func (a *Aviary) verifySSOTicket(ticket string) (email, projectID string, ok bool) {
	raw, err := base64.RawURLEncoding.DecodeString(ticket)
	if err != nil {
		return "", "", false
	}
	parts := strings.Split(string(raw), "|")
	if len(parts) != 5 {
		return "", "", false
	}
	email, projectID, expiryStr, nonce, gotMAC := parts[0], parts[1], parts[2], parts[3], parts[4]

	wantMAC := a.sessionMAC(strings.Join([]string{email, projectID, expiryStr, nonce}, "|"))
	if !hmac.Equal([]byte(gotMAC), []byte(wantMAC)) {
		return "", "", false
	}

	expiryUnix, err := strconv.ParseInt(expiryStr, 10, 64)
	if err != nil {
		return "", "", false
	}
	expiry := time.Unix(expiryUnix, 0)
	if time.Now().After(expiry) {
		return "", "", false
	}
	if !a.ssoTickets.consume(nonce, expiry) {
		return "", "", false // already redeemed
	}
	return email, projectID, true
}

// apiDashboardSSO mints a one-time ticket for the authenticated caller and
// redirects them to the target project's dashboard, where Aviary completes the
// handoff by minting a PocketBase auth token. Superusers may open any project;
// collaborators only the projects granted to them.
func (a *Aviary) apiDashboardSSO(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	email, role, ok := a.identity(r)
	if !ok {
		a.apiError(w, http.StatusUnauthorized, "authentication required")
		return
	}

	if role != roleSuperuser {
		granted, err := a.store.CollaboratorHasProject(r.Context(), email, id)
		if err != nil {
			a.apiError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if !granted {
			a.apiError(w, http.StatusForbidden, "no access to this project")
			return
		}
	}

	p, err := a.store.Get(r.Context(), id)
	switch {
	case errors.Is(err, ErrNotFound):
		a.apiError(w, http.StatusNotFound, "unknown project")
		return
	case err != nil:
		a.apiError(w, http.StatusInternalServerError, err.Error())
		return
	case p.Status != StatusActive:
		a.apiError(w, http.StatusForbidden, "project disabled")
		return
	}

	ticket, err := a.signSSOTicket(email, id, time.Now().Add(ssoTicketTTL))
	if err != nil {
		a.apiError(w, http.StatusInternalServerError, err.Error())
		return
	}

	target := projectSSOURL(r, id, ticket)
	http.Redirect(w, r, target, http.StatusFound)
}

// handleProjectSSO completes a dashboard handoff on the project subdomain: it
// verifies the ticket, mints a PocketBase auth token for the authorised
// superuser record and returns a tiny page that seeds the admin dashboard's
// auth store before redirecting to /_/. The cage is already booted by the time
// this runs (ServeHTTP boots it before dispatching).
func (a *Aviary) handleProjectSSO(w http.ResponseWriter, r *http.Request, id string, c *cage) {
	ticket := r.URL.Query().Get("ticket")
	email, projectID, ok := a.verifySSOTicket(ticket)
	if !ok || projectID != id {
		http.Error(w, "invalid or expired dashboard link", http.StatusForbidden)
		return
	}

	record, err := c.app.FindAuthRecordByEmail(core.CollectionNameSuperusers, email)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "no dashboard access for this account", http.StatusForbidden)
		return
	}
	if err != nil {
		a.log.Error("dashboard sso: superuser lookup failed", "project", id, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	record.IgnoreEmailVisibility(true)
	token, err := record.NewAuthToken()
	if err != nil {
		a.log.Error("dashboard sso: token mint failed", "project", id, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	recordJSON, err := json.Marshal(record)
	if err != nil {
		a.log.Error("dashboard sso: record marshal failed", "project", id, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// encoding/json escapes <, > and & to \u003c etc, so both values are safe
	// to embed directly inside the inline <script> below.
	auth, err := json.Marshal(map[string]any{"token": token, "record": json.RawMessage(recordJSON)})
	if err != nil {
		a.log.Error("dashboard sso: auth marshal failed", "project", id, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	fmt.Fprintf(w, ssoBootstrapHTML, auth)
}

// ssoBootstrapHTML seeds the PocketBase admin dashboard's auth store (localStorage
// key "pocketbase_auth") with the minted credential, then redirects to the
// dashboard already logged in.
const ssoBootstrapHTML = `<!doctype html><html><head><meta charset="utf-8"><title>Opening dashboard…</title></head>` +
	`<body><script>try{localStorage.setItem("pocketbase_auth",JSON.stringify(%s));}catch(e){}` +
	`location.replace("/_/");</script>Opening dashboard…</body></html>`

// projectSSOURL builds the absolute URL of a project's SSO handoff endpoint,
// derived from the control-plane request host. A bare IP control host cannot
// carry a project subdomain label (e.g. "p2uy3f.127.0.0.1" is unresolvable), so
// it falls back to *.localhost, which browsers resolve to loopback.
func projectSSOURL(r *http.Request, id, ticket string) string {
	scheme := "http"
	if isHTTPS(r) {
		scheme = "https"
	}
	host := projectHostFromControl(r.Host, id)
	return fmt.Sprintf("%s://%s%s?ticket=%s", scheme, host, ssoPath, ticket)
}

// projectHostFromControl derives the host (including port) that serves project
// id, given the host the control plane was reached on.
func projectHostFromControl(controlHost, id string) string {
	hostname := controlHost
	port := ""
	if h, p, err := net.SplitHostPort(controlHost); err == nil {
		hostname, port = h, ":"+p
	}

	var base string
	switch {
	case net.ParseIP(hostname) != nil:
		base = id + ".localhost"
	default:
		label, rest, found := strings.Cut(hostname, ".")
		switch {
		case !found:
			// Bare host such as "localhost".
			base = id + "." + hostname
		case reserved[label]:
			// Reserved control label (e.g. aviary-console.apps.example.com) →
			// swap it for the project id.
			base = id + "." + rest
		default:
			base = id + "." + hostname
		}
	}
	return base + port
}

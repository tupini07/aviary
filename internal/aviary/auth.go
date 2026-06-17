package aviary

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// sessionCookie is the name of the control-plane session cookie.
const sessionCookie = "aviary_session"

// sessionTTL is how long a control-plane login session stays valid.
const sessionTTL = 7 * 24 * time.Hour

// ctxKey is an unexported context-key type for request-scoped values.
type ctxKey string

const ctxSuperuserEmail ctxKey = "superuser_email"

// Session roles. A superuser administers the whole instance; a collaborator has
// scoped access to the specific projects granted to them.
const (
	roleSuperuser    = "superuser"
	roleCollaborator = "collaborator"
)

// signSession builds a signed, tamper-evident session token of the form
// "<email>|<role>|<expiryUnix>|<hmac>" encoded so the identity, role and expiry
// cannot be forged without the server's session key.
func (a *Aviary) signSession(email, role string, expiry time.Time) string {
	payload := email + "|" + role + "|" + strconv.FormatInt(expiry.Unix(), 10)
	mac := a.sessionMAC(payload)
	return base64.RawURLEncoding.EncodeToString([]byte(payload + "|" + mac))
}

// verifySession validates a session token and returns the identity email and
// role if the signature is valid and the token has not expired.
func (a *Aviary) verifySession(token string) (email, role string, ok bool) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return "", "", false
	}
	parts := strings.Split(string(raw), "|")
	if len(parts) != 4 {
		return "", "", false
	}
	email, role, expiryStr, gotMAC := parts[0], parts[1], parts[2], parts[3]

	wantMAC := a.sessionMAC(email + "|" + role + "|" + expiryStr)
	if !hmac.Equal([]byte(gotMAC), []byte(wantMAC)) {
		return "", "", false
	}

	expiryUnix, err := strconv.ParseInt(expiryStr, 10, 64)
	if err != nil || time.Now().After(time.Unix(expiryUnix, 0)) {
		return "", "", false
	}
	return email, role, true
}

func (a *Aviary) sessionMAC(payload string) string {
	mac := hmac.New(sha256.New, a.sessionKey)
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// identity returns the authenticated email and role from the request's session
// cookie, or ("", "", false) if the request is not authenticated.
func (a *Aviary) identity(r *http.Request) (email, role string, ok bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return "", "", false
	}
	return a.verifySession(c.Value)
}

// currentSuperuser returns the authenticated superuser email from the request's
// session cookie, or "" if the request is not an authenticated superuser.
func (a *Aviary) currentSuperuser(r *http.Request) string {
	email, role, ok := a.identity(r)
	if !ok || role != roleSuperuser {
		return ""
	}
	return email
}

// requireAuth wraps a handler, allowing it through only when the request
// carries a valid session (superuser or collaborator). Unauthenticated requests
// get 401.
func (a *Aviary) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, _, ok := a.identity(r); !ok {
			a.apiError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		next(w, r)
	}
}

// requireSuperuser wraps a handler, allowing it through only for an
// authenticated superuser. Collaborators and anonymous callers get 401.
func (a *Aviary) requireSuperuser(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.currentSuperuser(r) == "" {
			a.apiError(w, http.StatusUnauthorized, "superuser authentication required")
			return
		}
		next(w, r)
	}
}

// --- auth handlers ---

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// apiLogin authenticates control-plane superuser credentials and, on success,
// sets a signed session cookie.
func (a *Aviary) apiLogin(w http.ResponseWriter, r *http.Request) {
	if ok, retry := a.loginLimit.allow(clientIP(r)); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
		a.apiError(w, http.StatusTooManyRequests, "too many login attempts; try again later")
		return
	}

	var req loginRequest
	if !decodeJSON(w, r, &req, a) {
		return
	}

	ok, err := a.AuthenticateSuperuser(r.Context(), req.Email, req.Password)
	if err != nil {
		a.apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if ok {
		email := strings.TrimSpace(strings.ToLower(req.Email))
		a.setSessionCookie(w, r, email, roleSuperuser)
		writeJSON(w, http.StatusOK, map[string]any{"authenticated": true, "email": email, "role": roleSuperuser})
		return
	}

	// Fall back to collaborator credentials.
	collabOK, err := a.AuthenticateCollaborator(r.Context(), req.Email, req.Password)
	if err != nil {
		a.apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !collabOK {
		a.apiError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	email := strings.TrimSpace(strings.ToLower(req.Email))
	a.setSessionCookie(w, r, email, roleCollaborator)
	writeJSON(w, http.StatusOK, map[string]any{"authenticated": true, "email": email, "role": roleCollaborator})
}

// apiLogout clears the session cookie.
func (a *Aviary) apiLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   isHTTPS(r),
	})
	writeJSON(w, http.StatusOK, map[string]any{"authenticated": false})
}

// apiSession reports the current authentication state plus whether a superuser
// has been configured at all (so the UI can show first-run setup).
func (a *Aviary) apiSession(w http.ResponseWriter, r *http.Request) {
	configured, err := a.HasSuperuser(r.Context())
	if err != nil {
		a.apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	email, role, _ := a.identity(r)
	writeJSON(w, http.StatusOK, map[string]any{
		"authenticated": email != "",
		"email":         email,
		"role":          role,
		"configured":    configured,
	})
}

func (a *Aviary) setSessionCookie(w http.ResponseWriter, r *http.Request, email, role string) {
	expiry := time.Now().Add(sessionTTL)
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    a.signSession(email, role, expiry),
		Path:     "/",
		Expires:  expiry,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   isHTTPS(r),
	})
}

func isHTTPS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

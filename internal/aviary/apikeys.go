package aviary

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/tupini07/aviary/internal/controlplane"
)

// apiKeyPrefix marks Aviary API-key tokens so they are recognizable in logs,
// secret scanners and the Authorization header.
const apiKeyPrefix = "av_"

// APIKey is a project-scoped, non-interactive credential.
type APIKey = controlplane.APIKey

// generateAPIKeyToken mints a fresh opaque token and its stored public id. The
// token is "av_<base64url(32 random bytes)>"; only its SHA-256 hash is persisted.
func generateAPIKeyToken() (token, id string, err error) {
	secret := make([]byte, 32)
	if _, err = rand.Read(secret); err != nil {
		return "", "", err
	}
	idBytes := make([]byte, 8)
	if _, err = rand.Read(idBytes); err != nil {
		return "", "", err
	}
	return apiKeyPrefix + base64.RawURLEncoding.EncodeToString(secret), hex.EncodeToString(idBytes), nil
}

// bearerAPIKeyToken extracts an Aviary API-key token from the Authorization
// header, returning false when the header is absent or not an Aviary key.
func bearerAPIKeyToken(r *http.Request) (string, bool) {
	const scheme = "bearer "
	h := r.Header.Get("Authorization")
	if len(h) <= len(scheme) || !strings.EqualFold(h[:len(scheme)], scheme) {
		return "", false
	}
	tok := strings.TrimSpace(h[len(scheme):])
	if !strings.HasPrefix(tok, apiKeyPrefix) {
		return "", false
	}
	return tok, true
}

// authenticateAPIKey resolves a request's bearer token to its stored key,
// rejecting unknown or expired keys.
func (a *Aviary) authenticateAPIKey(r *http.Request) (*APIKey, bool) {
	tok, ok := bearerAPIKeyToken(r)
	if !ok {
		return nil, false
	}
	key, err := a.store.APIKeyByHash(r.Context(), hashToken(tok))
	if err != nil {
		return nil, false
	}
	if key.ExpiresAt != nil && time.Now().After(*key.ExpiresAt) {
		return nil, false
	}
	return key, true
}

// apiKeyPrincipal is the synthetic identity email reported for a request
// authenticated by an API key bound to projectID.
func apiKeyPrincipal(projectID string) string {
	return "apikey:" + projectID
}

// --- HTTP handlers (control plane) ---

type createAPIKeyRequest struct {
	Label         string `json:"label"`
	ExpiresInDays *int   `json:"expiresInDays,omitempty"`
}

// createdAPIKey is the one-time response to key creation: the stored metadata
// plus the raw token, which is never retrievable again.
type createdAPIKey struct {
	APIKey
	Token string `json:"token"`
}

// apiListAPIKeys returns a project's API keys (metadata only; never the token).
func (a *Aviary) apiListAPIKeys(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := a.authorizeProjectAdmin(w, r, id); !ok {
		return
	}
	if !a.projectExists(w, r, id) {
		return
	}
	keys, err := a.store.ListAPIKeys(r.Context(), id)
	if err != nil {
		a.apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, keys)
}

// apiCreateAPIKey mints a new project-scoped API key and returns the raw token
// exactly once.
func (a *Aviary) apiCreateAPIKey(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := a.authorizeProjectAdmin(w, r, id); !ok {
		return
	}
	if !a.projectExists(w, r, id) {
		return
	}

	var req createAPIKeyRequest
	if !decodeJSON(w, r, &req, a) {
		return
	}
	var expiresAt *time.Time
	if req.ExpiresInDays != nil {
		if *req.ExpiresInDays <= 0 {
			a.apiError(w, http.StatusBadRequest, "expiresInDays must be a positive number")
			return
		}
		t := time.Now().Add(time.Duration(*req.ExpiresInDays) * 24 * time.Hour).UTC()
		expiresAt = &t
	}

	token, keyID, err := generateAPIKeyToken()
	if err != nil {
		a.apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	label := strings.TrimSpace(req.Label)
	if err := a.store.CreateAPIKey(r.Context(), keyID, id, label, hashToken(token), expiresAt); err != nil {
		a.apiError(w, http.StatusInternalServerError, err.Error())
		return
	}

	meta := APIKey{
		ID:        keyID,
		ProjectID: id,
		Label:     label,
		CreatedAt: time.Now().UTC(),
		ExpiresAt: expiresAt,
	}
	writeJSON(w, http.StatusCreated, createdAPIKey{APIKey: meta, Token: token})
}

// apiDeleteAPIKey revokes a single API key.
func (a *Aviary) apiDeleteAPIKey(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := a.authorizeProjectAdmin(w, r, id); !ok {
		return
	}
	err := a.store.DeleteAPIKey(r.Context(), id, r.PathValue("keyId"))
	switch {
	case errors.Is(err, controlplane.ErrNotFound):
		a.apiError(w, http.StatusNotFound, "api key not found")
	case err != nil:
		a.apiError(w, http.StatusInternalServerError, err.Error())
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

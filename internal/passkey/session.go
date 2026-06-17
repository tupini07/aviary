package passkey

import (
	"crypto/rand"
	"encoding/base64"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"
)

// sessionTTL bounds how long a ceremony may remain in-flight between its begin
// and finish steps.
const sessionTTL = 5 * time.Minute

// sessionStore holds in-flight WebAuthn ceremony state keyed by an opaque token
// handed to the client. It is in-memory (per process), which is sufficient for
// Aviary's single-process front.
type sessionStore struct {
	mu  sync.Mutex
	m   map[string]sessionEntry
	now func() time.Time
}

type sessionEntry struct {
	data    *webauthn.SessionData
	expires time.Time
}

func newSessionStore() *sessionStore {
	return &sessionStore{m: make(map[string]sessionEntry), now: time.Now}
}

// put stores session data and returns a freshly-generated lookup token.
func (s *sessionStore) put(data *webauthn.SessionData) (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(buf)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.gc()
	s.m[token] = sessionEntry{data: data, expires: s.now().Add(sessionTTL)}
	return token, nil
}

// take retrieves and removes the session data for token. The second return is
// false if the token is unknown or expired.
func (s *sessionStore) take(token string) (*webauthn.SessionData, bool) {
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

// gc removes expired entries. Caller must hold the lock.
func (s *sessionStore) gc() {
	now := s.now()
	for k, e := range s.m {
		if now.After(e.expires) {
			delete(s.m, k)
		}
	}
}

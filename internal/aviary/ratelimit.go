package aviary

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// loginRateLimiter throttles control-plane login attempts per client IP using a
// simple fixed-window counter. It is intentionally lightweight (in-memory, no
// external dependency) since the control plane is a single-process front.
type loginRateLimiter struct {
	mu       sync.Mutex
	attempts map[string]*rlWindow
	limit    int
	window   time.Duration
	now      func() time.Time
}

type rlWindow struct {
	count int
	reset time.Time
}

// newLoginRateLimiter allows up to limit attempts per window per IP.
func newLoginRateLimiter(limit int, window time.Duration) *loginRateLimiter {
	return &loginRateLimiter{
		attempts: make(map[string]*rlWindow),
		limit:    limit,
		window:   window,
		now:      time.Now,
	}
}

// allow reports whether a request from key may proceed, recording the attempt.
// When it returns false it also returns the duration until the window resets.
func (l *loginRateLimiter) allow(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	w, ok := l.attempts[key]
	if !ok || now.After(w.reset) {
		l.attempts[key] = &rlWindow{count: 1, reset: now.Add(l.window)}
		l.gc(now)
		return true, 0
	}
	if w.count >= l.limit {
		return false, w.reset.Sub(now)
	}
	w.count++
	return true, 0
}

// gc drops expired windows. Caller must hold the lock.
func (l *loginRateLimiter) gc(now time.Time) {
	for k, w := range l.attempts {
		if now.After(w.reset) {
			delete(l.attempts, k)
		}
	}
}

// clientIP extracts the best-effort client IP for rate-limiting purposes. It
// honours X-Forwarded-For (first hop) when present, since the front commonly
// sits behind a reverse proxy.
func clientIP(r *http.Request) string {
	return clientIPFrom(r.RemoteAddr, r.Header.Get("X-Forwarded-For"))
}

// clientIPFrom is the pure core of clientIP, separated for testability.
func clientIPFrom(remoteAddr, xff string) string {
	if xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return remoteAddr
	}
	return host
}

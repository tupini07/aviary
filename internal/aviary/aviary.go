// Package aviary implements a multi-tenant control plane that hosts many
// isolated PocketBase projects ("cages") within a single process and routes
// incoming requests to them by subdomain.
//
// Each project gets its own data directory (and therefore its own SQLite
// database, files and settings), giving Firebase-style project isolation
// without running a separate OS process per project. Projects must be
// explicitly provisioned (see CreateProject); once provisioned they boot
// lazily on first request and are evicted after a configurable idle period.
package aviary

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/tupini07/aviary/internal/controlplane"
)

// Config controls how the Aviary hosts projects.
type Config struct {
	// DataDir is the parent directory for all Aviary state. It holds the
	// control-plane database (control.db) and a projects/ sub-directory with
	// one data dir per project.
	DataDir string

	// IdleTTL is how long a project may sit unused before it is evicted to
	// reclaim memory. A new request transparently boots it again.
	IdleTTL time.Duration

	// Logger is used for control-plane logging. Defaults to slog.Default().
	Logger *slog.Logger

	// AllowDashboardPassword keeps PocketBase's native superuser password login
	// enabled on each project. By default it is disabled: the only way into a
	// project dashboard is an Aviary-minted token (see dashboard SSO), which
	// removes the password brute-force surface entirely.
	AllowDashboardPassword bool
}

// Aviary is the multi-tenant registry and HTTP front. It implements
// http.Handler and may be mounted directly on an http.Server.
type Aviary struct {
	cfg         Config
	log         *slog.Logger
	store       *controlplane.Store
	projectsDir string
	control     http.Handler

	mu    sync.Mutex
	cages map[string]*cage

	sessionKey []byte
	loginLimit *loginRateLimiter

	suPasskeySessions *suSessionStore
	ssoTickets        *ticketStore

	quit chan struct{}
	wg   sync.WaitGroup
}

// reserved holds host labels that never map to a project and are instead
// served by the control plane. "_console" is the canonical control-plane
// subdomain (usable behind a reverse proxy on a real domain, e.g.
// _console.apps.example.com); "www" and "_" are kept as conventional aliases.
var reserved = map[string]bool{"_console": true, "www": true, "_": true}

// New creates an Aviary, opens its control-plane store and starts the
// background idle-eviction reaper.
func New(cfg Config) (*Aviary, error) {
	if cfg.DataDir == "" {
		cfg.DataDir = "./data"
	}
	if cfg.IdleTTL <= 0 {
		cfg.IdleTTL = 5 * time.Minute
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}

	projectsDir := filepath.Join(cfg.DataDir, "projects")
	if err := os.MkdirAll(projectsDir, 0o755); err != nil {
		return nil, fmt.Errorf("aviary: create projects dir: %w", err)
	}

	store, err := controlplane.Open(filepath.Join(cfg.DataDir, "control.db"))
	if err != nil {
		return nil, err
	}

	// Session-signing key. Persisted in the control store so login sessions
	// survive process restarts.
	sessionKey, err := store.SessionKey(context.Background())
	if err != nil {
		return nil, err
	}

	a := &Aviary{
		cfg:         cfg,
		log:         cfg.Logger,
		store:       store,
		projectsDir: projectsDir,
		cages:       make(map[string]*cage),
		sessionKey:  sessionKey,
		loginLimit:  newLoginRateLimiter(10, time.Minute),
		quit:        make(chan struct{}),
	}
	a.suPasskeySessions = newSUSessionStore()
	a.ssoTickets = newTicketStore()
	a.control = a.controlHandler()

	a.wg.Add(1)
	go a.reaper()

	return a, nil
}

// ServeHTTP routes a request to the project identified by the first label of
// the Host header. Requests without a project subdomain hit the control-plane
// landing page. Only provisioned, active projects are served.
func (a *Aviary) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id := projectID(r.Host)
	if id == "" || reserved[id] {
		a.control.ServeHTTP(w, r)
		return
	}
	if !controlplane.ValidID(id) {
		http.Error(w, "invalid project id", http.StatusBadRequest)
		return
	}

	p, err := a.store.Get(r.Context(), id)
	switch {
	case errors.Is(err, controlplane.ErrNotFound):
		http.Error(w, "unknown project", http.StatusNotFound)
		return
	case err != nil:
		a.log.Error("control-plane lookup failed", "project", id, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	case p.Status != controlplane.StatusActive:
		http.Error(w, "project disabled", http.StatusForbidden)
		return
	}

	c, err := a.getCage(id)
	if err != nil {
		a.log.Error("failed to start project", "project", id, "error", err)
		http.Error(w, "failed to start project: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Complete a dashboard single-sign-on handoff before handing the request to
	// PocketBase. The cage is booted above, so its app is ready to mint a token.
	if r.URL.Path == ssoPath {
		a.handleProjectSSO(w, r, id, c)
		return
	}

	// Serve a generated OpenAPI description of this project's PocketBase API.
	if r.URL.Path == openapiPath {
		a.handleProjectOpenAPI(w, r, id, c)
		return
	}

	c.handler.ServeHTTP(w, r)
}

// getCage returns the running cage for id, booting it lazily on first use.
// Concurrent callers for the same id share a single boot (single-flight).
func (a *Aviary) getCage(id string) (*cage, error) {
	a.mu.Lock()
	c, ok := a.cages[id]
	if !ok {
		c = &cage{id: id, ready: make(chan struct{}), allowDashboardPassword: a.cfg.AllowDashboardPassword}
		a.cages[id] = c
		go func() {
			c.startErr = c.start(a.projectsDir, a.log)
			if c.startErr == nil {
				// Seed the control-plane superuser into the freshly-booted
				// project so the same admin credentials unlock its dashboard.
				if err := a.applySuperuserFromStore(context.Background(), c.app); err != nil {
					a.log.Warn("failed to apply superuser on boot", "project", id, "error", err)
				}
				// Seed any project-scoped collaborators granted access.
				if err := a.applyCollaboratorsOnBoot(context.Background(), id, c.app); err != nil {
					a.log.Warn("failed to apply collaborators on boot", "project", id, "error", err)
				}
			}
			close(c.ready)
			if c.startErr != nil {
				a.mu.Lock()
				delete(a.cages, id)
				a.mu.Unlock()
			}
		}()
	}
	a.mu.Unlock()

	<-c.ready
	if c.startErr != nil {
		return nil, c.startErr
	}
	c.touch()
	return c, nil
}

// runningProjects returns the set of project ids whose app is currently booted
// and ready to serve. Cages that are still booting or that failed to start are
// excluded.
func (a *Aviary) runningProjects() map[string]bool {
	a.mu.Lock()
	cages := make([]*cage, 0, len(a.cages))
	for _, c := range a.cages {
		cages = append(cages, c)
	}
	a.mu.Unlock()

	running := make(map[string]bool, len(cages))
	for _, c := range cages {
		select {
		case <-c.ready:
			if c.startErr == nil {
				running[c.id] = true
			}
		default:
			// Still booting; report as not-yet-running rather than block.
		}
	}
	return running
}

// landing renders a minimal control-plane page listing the provisioned projects.
// reaper periodically evicts idle projects until Shutdown is called.
func (a *Aviary) reaper() {
	defer a.wg.Done()

	interval := a.cfg.IdleTTL / 2
	if interval < time.Second {
		interval = time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()

	for {
		select {
		case <-a.quit:
			return
		case <-t.C:
			a.evictIdle()
		}
	}
}

// evictIdle stops and removes every ready project unused for longer than IdleTTL.
func (a *Aviary) evictIdle() {
	cutoff := time.Now().Add(-a.cfg.IdleTTL)

	a.mu.Lock()
	var dead []*cage
	for id, c := range a.cages {
		if c.isReady() && c.lastUsed().Before(cutoff) {
			dead = append(dead, c)
			delete(a.cages, id)
		}
	}
	a.mu.Unlock()

	for _, c := range dead {
		a.log.Info("evicting idle project", "project", c.id)
		c.stop(a.log)
	}
}

// Shutdown stops the reaper, gracefully stops every running project and closes
// the control-plane store.
func (a *Aviary) Shutdown() {
	select {
	case <-a.quit:
		// already shut down
	default:
		close(a.quit)
	}
	a.wg.Wait()

	a.mu.Lock()
	cages := a.cages
	a.cages = make(map[string]*cage)
	a.mu.Unlock()

	for _, c := range cages {
		if c.isReady() {
			c.stop(a.log)
		}
	}

	if err := a.store.Close(); err != nil {
		a.log.Warn("error closing control-plane store", "error", err)
	}
}

// projectID extracts the project identifier from a request Host header: the
// first DNS label, lower-cased. Bare hosts ("localhost") and raw IPs return "".
func projectID(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" || net.ParseIP(host) != nil {
		return ""
	}

	label, _, found := strings.Cut(host, ".")
	if !found {
		return "" // e.g. "localhost"
	}
	return label
}

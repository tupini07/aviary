package aviary

import (
	"log/slog"
	"net/http"
	"path/filepath"
	"sync"
	"time"

	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"

	"github.com/tupini07/aviary/internal/passkey"
)

// cage is a single isolated PocketBase project running inside the Aviary.
type cage struct {
	id      string
	app     *pocketbase.PocketBase
	handler http.Handler

	// allowDashboardPassword keeps PocketBase's native superuser password login
	// enabled. When false (the default), password auth is rejected so the only
	// way into the dashboard is an Aviary-minted token.
	allowDashboardPassword bool

	// spa enables single-page-app fallback for the project's pb_public static
	// server: unmatched paths serve index.html instead of returning 404.
	spa bool

	ready    chan struct{} // closed once start() has finished (success or failure)
	startErr error

	mu   sync.Mutex
	last time.Time
}

// start boots the project's PocketBase app against its own data directory and
// builds its HTTP handler. It performs the same setup as apis.Serve but without
// binding a TCP listener, so the handler can be mounted by the Aviary front.
func (c *cage) start(projectsDir string, log *slog.Logger) error {
	dir := filepath.Join(projectsDir, c.id)

	app := pocketbase.NewWithConfig(pocketbase.Config{
		DefaultDataDir:  dir,
		HideStartBanner: true,
	})

	if err := app.Bootstrap(); err != nil {
		return err
	}

	// Register passkey/WebAuthn endpoints before the handler is built, so the
	// OnServe hook fires during buildHandler.
	if err := passkey.Setup(app); err != nil {
		_ = app.ResetBootstrapState()
		return err
	}

	// Unless explicitly allowed, reject PocketBase's native superuser password
	// login so the dashboard is reachable only via an Aviary-minted token. This
	// eliminates the password brute-force surface on every project.
	if !c.allowDashboardPassword {
		app.OnRecordAuthWithPasswordRequest(core.CollectionNameSuperusers).BindFunc(
			func(e *core.RecordAuthWithPasswordRequestEvent) error {
				return apis.NewForbiddenError("password login is disabled; open the dashboard from the Aviary control plane", nil)
			})
	}

	handler, err := buildHandler(app, filepath.Join(dir, "pb_public"), c.spa)
	if err != nil {
		_ = app.ResetBootstrapState()
		return err
	}

	c.app = app
	c.handler = handler
	c.last = time.Now()
	log.Info("project booted", "project", c.id, "dir", dir)
	return nil
}

// stop releases the project's resources (DB connections, etc.).
func (c *cage) stop(log *slog.Logger) {
	if c.app == nil {
		return
	}
	if err := c.app.ResetBootstrapState(); err != nil {
		log.Warn("error stopping project", "project", c.id, "error", err)
	}
}

func (c *cage) touch() {
	c.mu.Lock()
	c.last = time.Now()
	c.mu.Unlock()
}

func (c *cage) lastUsed() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.last
}

func (c *cage) isReady() bool {
	select {
	case <-c.ready:
		return c.startErr == nil
	default:
		return false
	}
}

// compile-time assurance that *pocketbase.PocketBase satisfies core.App, which
// buildHandler relies on.
var _ core.App = (*pocketbase.PocketBase)(nil)

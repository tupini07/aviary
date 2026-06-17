package aviary

import (
	"context"
	"database/sql"
	"errors"
	"sync"

	"github.com/pocketbase/pocketbase/core"

	"github.com/tupini07/aviary/internal/controlplane"
)

// applySuperuser upserts the control-plane superuser identity into a single
// project's _superusers collection, copying the canonical bcrypt hash so the
// same email + password authenticates against this project's dashboard.
//
// It relies on PocketBase's PasswordField supporting a direct bcrypt hash via
// SetRaw (see core/field_password.go). The per-project tokenKey differs and
// only signs that project's tokens, so it need not be shared.
func applySuperuser(app core.App, email, passwordHash string) error {
	col, err := app.FindCollectionByNameOrId(core.CollectionNameSuperusers)
	if err != nil {
		return err
	}

	record, err := app.FindAuthRecordByEmail(core.CollectionNameSuperusers, email)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		record = core.NewRecord(col)
		record.SetEmail(email)
		record.RefreshTokenKey()
	case err != nil:
		return err
	}

	// PocketBase's PasswordField stores a *PasswordFieldValue; setting the
	// bcrypt hash directly lets us share one canonical credential across
	// projects without knowing the plaintext here.
	record.SetRaw("password", &core.PasswordFieldValue{Hash: passwordHash})
	return app.Save(record)
}

// applySuperuserFromStore applies the configured control-plane superuser (if
// any) to the given project app. It is a no-op when no superuser is set.
func (a *Aviary) applySuperuserFromStore(ctx context.Context, app core.App) error {
	su, err := a.store.GetSuperuser(ctx)
	if errors.Is(err, controlplane.ErrNoSuperuser) {
		return nil
	}
	if err != nil {
		return err
	}
	return applySuperuser(app, su.Email, su.PasswordHash)
}

// propagateSuperuserToAll re-applies the control-plane superuser to every
// currently-running project. Stopped projects pick it up when they next boot.
// Projects are updated concurrently so a bulk change is not serialized behind
// each cage's boot.
func (a *Aviary) propagateSuperuserToAll(ctx context.Context) {
	su, err := a.store.GetSuperuser(ctx)
	if err != nil {
		return
	}

	a.mu.Lock()
	running := make([]*cage, 0, len(a.cages))
	for _, c := range a.cages {
		running = append(running, c)
	}
	a.mu.Unlock()

	var wg sync.WaitGroup
	for _, c := range running {
		wg.Add(1)
		go func(c *cage) {
			defer wg.Done()
			<-c.ready
			if c.startErr != nil || c.app == nil {
				return
			}
			if err := applySuperuser(c.app, su.Email, su.PasswordHash); err != nil {
				a.log.Warn("failed to propagate superuser", "project", c.id, "error", err)
			}
		}(c)
	}
	wg.Wait()
}

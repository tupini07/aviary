package aviary

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/tupini07/aviary/internal/controlplane"
)

// Re-export the control-plane types/errors that callers of the provisioning API
// need, so consumers don't have to import controlplane directly.
type (
	// Project is a provisioned project record.
	Project = controlplane.Project
	// Status is a project's administrative state.
	Status = controlplane.Status
)

const (
	StatusActive   = controlplane.StatusActive
	StatusDisabled = controlplane.StatusDisabled
)

var (
	ErrNotFound  = controlplane.ErrNotFound
	ErrExists    = controlplane.ErrExists
	ErrInvalidID = controlplane.ErrInvalidID
	// ErrReserved is returned by CreateProject when the requested id is a
	// reserved control-plane label (e.g. "aviary-console" or "www") and so
	// cannot be used for a tenant project.
	ErrReserved = errors.New("aviary: project id is reserved")
)

// projectPath returns the data directory for the given project id.
func (a *Aviary) projectPath(id string) string {
	return filepath.Join(a.projectsDir, id)
}

// CreateProject provisions a new project: it registers the project in the
// control-plane store and creates its (empty) data directory. The project's
// PocketBase app is booted lazily on first request, not here.
//
// Returns ErrInvalidID for a malformed id, ErrReserved if the id is a reserved
// control-plane label, or ErrExists if it already exists.
func (a *Aviary) CreateProject(ctx context.Context, id, name string) (*Project, error) {
	if reserved[id] {
		return nil, ErrReserved
	}

	p, err := a.store.Create(ctx, id, name)
	if err != nil {
		return nil, err
	}

	if err := os.MkdirAll(a.projectPath(id), 0o755); err != nil {
		// Roll back the registry entry so the store stays consistent with disk.
		_ = a.store.Delete(ctx, id)
		return nil, fmt.Errorf("aviary: create project dir %q: %w", id, err)
	}

	a.log.Info("project provisioned", "project", id)
	return p, nil
}

// GetProject returns the provisioned project, or ErrNotFound.
func (a *Aviary) GetProject(ctx context.Context, id string) (*Project, error) {
	return a.store.Get(ctx, id)
}

// ListProjects returns all provisioned projects ordered by id.
func (a *Aviary) ListProjects(ctx context.Context) ([]*Project, error) {
	return a.store.List(ctx)
}

// SetProjectStatus updates a project's administrative status. Disabling a
// project also stops its running app so it stops serving immediately.
func (a *Aviary) SetProjectStatus(ctx context.Context, id string, status Status) error {
	if err := a.store.SetStatus(ctx, id, status); err != nil {
		return err
	}
	if status != StatusActive {
		a.evict(id)
	}
	return nil
}

// SetProjectName updates a project's display name. Returns ErrNotFound if the
// project does not exist.
func (a *Aviary) SetProjectName(ctx context.Context, id, name string) error {
	return a.store.SetName(ctx, id, name)
}

// SetProjectSPA toggles a project's single-page-app static fallback. Because the
// fallback mode is baked into the static route when the project boots, the
// project is evicted so it reboots with the new setting on its next request.
func (a *Aviary) SetProjectSPA(ctx context.Context, id string, spa bool) error {
	if err := a.store.SetSPA(ctx, id, spa); err != nil {
		return err
	}
	a.evict(id)
	return nil
}

// SetProjectQuota updates a project's pb_public storage quota in bytes (0 =
// unlimited). The quota is enforced at write/deploy time, so no eviction or
// reboot is needed. Returns ErrNotFound if the project does not exist.
func (a *Aviary) SetProjectQuota(ctx context.Context, id string, bytes int64) error {
	return a.store.SetQuota(ctx, id, bytes)
}

// DeleteProject stops the project's app (if running), removes its registry
// entry and deletes its data directory. Returns ErrNotFound if it does not
// exist.
func (a *Aviary) DeleteProject(ctx context.Context, id string) error {
	if _, err := a.store.Get(ctx, id); err != nil {
		return err // ErrNotFound or a real error
	}

	a.evict(id)

	if err := a.store.Delete(ctx, id); err != nil {
		return err
	}

	if err := os.RemoveAll(a.projectPath(id)); err != nil {
		return fmt.Errorf("aviary: remove project dir %q: %w", id, err)
	}

	a.log.Info("project deleted", "project", id)
	return nil
}

// evict stops a single running project and removes it from the live registry.
// It is a no-op if the project is not currently booted.
func (a *Aviary) evict(id string) {
	a.mu.Lock()
	c, ok := a.cages[id]
	if ok {
		delete(a.cages, id)
	}
	a.mu.Unlock()

	if !ok {
		return
	}
	<-c.ready // ensure any in-flight boot has settled before stopping
	if c.startErr == nil {
		c.stop(a.log)
	}
}

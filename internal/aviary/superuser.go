package aviary

import (
	"context"
	"errors"
	"strings"

	"golang.org/x/crypto/bcrypt"

	"github.com/tupini07/aviary/internal/controlplane"
)

// ErrNoSuperuser is returned when no control-plane superuser is configured yet.
var ErrNoSuperuser = controlplane.ErrNoSuperuser

// Superuser is the control-plane administrator identity.
type Superuser = controlplane.Superuser

// SetSuperuser creates or updates the single control-plane superuser. The
// plaintext password is bcrypt-hashed; only the hash is persisted. The hash is
// the canonical credential later propagated to every project's _superusers
// collection, so the same email + password unlocks every project dashboard.
func (a *Aviary) SetSuperuser(ctx context.Context, email, password string) error {
	email = strings.TrimSpace(strings.ToLower(email))
	if email == "" {
		return errors.New("aviary: superuser email is required")
	}
	if len(password) < 8 {
		return errors.New("aviary: superuser password must be at least 8 characters")
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}

	if err := a.store.SetSuperuser(ctx, email, string(hash)); err != nil {
		return err
	}

	// Propagate the new credential to every project so dashboards stay in sync.
	a.propagateSuperuserToAll(ctx)

	a.log.Info("control-plane superuser set", "email", email)
	return nil
}

// GetSuperuser returns the control-plane superuser, or ErrNoSuperuser.
func (a *Aviary) GetSuperuser(ctx context.Context) (*Superuser, error) {
	return a.store.GetSuperuser(ctx)
}

// HasSuperuser reports whether a control-plane superuser has been configured.
func (a *Aviary) HasSuperuser(ctx context.Context) (bool, error) {
	return a.store.HasSuperuser(ctx)
}

// AuthenticateSuperuser verifies the given control-plane credentials. It
// returns true only when a superuser exists and the password matches.
func (a *Aviary) AuthenticateSuperuser(ctx context.Context, email, password string) (bool, error) {
	su, err := a.store.GetSuperuser(ctx)
	if errors.Is(err, ErrNoSuperuser) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !strings.EqualFold(strings.TrimSpace(email), su.Email) {
		return false, nil
	}
	if err := bcrypt.CompareHashAndPassword([]byte(su.PasswordHash), []byte(password)); err != nil {
		return false, nil
	}
	return true, nil
}

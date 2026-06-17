package aviary

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/pocketbase/pocketbase/core"
	"golang.org/x/crypto/bcrypt"

	"github.com/tupini07/aviary/internal/controlplane"
)

// invitationTTL is how long an invitation remains redeemable.
const invitationTTL = 7 * 24 * time.Hour

// ErrInvitationInvalid is returned when an invitation token is unknown or has
// expired.
var ErrInvitationInvalid = errors.New("aviary: invitation is invalid or has expired")

// Collaborator is a project-scoped admin identity.
type Collaborator = controlplane.Collaborator

// Invitation is a pending project-access grant.
type Invitation = controlplane.Invitation

// AuthenticateCollaborator verifies collaborator credentials against the
// control store. It returns true only when the collaborator exists and the
// password matches.
func (a *Aviary) AuthenticateCollaborator(ctx context.Context, email, password string) (bool, error) {
	email = strings.TrimSpace(strings.ToLower(email))
	c, err := a.store.GetCollaborator(ctx, email)
	if errors.Is(err, controlplane.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if bcrypt.CompareHashAndPassword([]byte(c.PasswordHash), []byte(password)) != nil {
		return false, nil
	}
	return true, nil
}

// hashToken returns the hex-encoded SHA-256 of a raw invitation token; only the
// hash is persisted so a leaked database cannot be used to redeem invitations.
func hashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// CreateInvitation issues a single-use invitation granting email access to
// projectID, returning the raw token (shown to the inviter only once).
func (a *Aviary) CreateInvitation(ctx context.Context, email, projectID string) (string, error) {
	email = strings.TrimSpace(strings.ToLower(email))
	if email == "" {
		return "", errors.New("aviary: invitee email is required")
	}
	if _, err := a.store.Get(ctx, projectID); err != nil {
		return "", err // ErrNotFound for an unknown project
	}

	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	raw := base64.RawURLEncoding.EncodeToString(buf)

	expires := time.Now().Add(invitationTTL)
	if err := a.store.CreateInvitation(ctx, hashToken(raw), email, projectID, expires); err != nil {
		return "", err
	}
	return raw, nil
}

// AcceptInvitation redeems an invitation token. For a brand-new collaborator the
// supplied password establishes their credentials; for an existing collaborator
// the password is ignored and only the new project grant is added. It returns
// the collaborator email and the granted project id.
func (a *Aviary) AcceptInvitation(ctx context.Context, rawToken, password string) (string, string, error) {
	inv, err := a.store.GetInvitation(ctx, hashToken(rawToken))
	if errors.Is(err, controlplane.ErrNotFound) {
		return "", "", ErrInvitationInvalid
	}
	if err != nil {
		return "", "", err
	}
	if time.Now().After(inv.ExpiresAt) {
		_ = a.store.DeleteInvitation(ctx, hashToken(rawToken))
		return "", "", ErrInvitationInvalid
	}

	_, err = a.store.GetCollaborator(ctx, inv.Email)
	switch {
	case errors.Is(err, controlplane.ErrNotFound):
		// New collaborator: a password is required to set their credentials.
		if len(password) < 8 {
			return "", "", errors.New("aviary: password must be at least 8 characters")
		}
		hash, hErr := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
		if hErr != nil {
			return "", "", hErr
		}
		if uErr := a.store.UpsertCollaborator(ctx, inv.Email, string(hash)); uErr != nil {
			return "", "", uErr
		}
	case err != nil:
		return "", "", err
	}

	if err := a.store.GrantProject(ctx, inv.Email, inv.ProjectID); err != nil {
		return "", "", err
	}
	if err := a.store.DeleteInvitation(ctx, hashToken(rawToken)); err != nil {
		return "", "", err
	}

	// Make the project dashboard usable immediately if it is already running.
	a.propagateCollaboratorToProject(ctx, inv.Email, inv.ProjectID)

	a.log.Info("invitation accepted", "email", inv.Email, "project", inv.ProjectID)
	return inv.Email, inv.ProjectID, nil
}

// propagateCollaboratorToProject upserts a collaborator's credentials into a
// running project's _superusers collection. Stopped projects pick the grant up
// on their next boot via applyCollaboratorsOnBoot.
func (a *Aviary) propagateCollaboratorToProject(ctx context.Context, email, projectID string) {
	c, err := a.store.GetCollaborator(ctx, email)
	if err != nil {
		return
	}

	a.mu.Lock()
	cage := a.cages[projectID]
	a.mu.Unlock()
	if cage == nil {
		return
	}
	<-cage.ready
	if cage.startErr != nil || cage.app == nil {
		return
	}
	if err := applySuperuser(cage.app, c.Email, c.PasswordHash); err != nil {
		a.log.Warn("failed to propagate collaborator", "project", projectID, "email", email, "error", err)
	}
}

// applyCollaboratorsOnBoot seeds every collaborator granted to projectID into
// the freshly-booted project's _superusers collection.
func (a *Aviary) applyCollaboratorsOnBoot(ctx context.Context, projectID string, app core.App) error {
	collabs, err := a.store.ProjectCollaborators(ctx, projectID)
	if err != nil {
		return err
	}
	for _, c := range collabs {
		if err := applySuperuser(app, c.Email, c.PasswordHash); err != nil {
			return err
		}
	}
	return nil
}

// RevokeCollaboratorProject removes a collaborator's access to a project,
// deleting their _superusers record from the project if it is running.
func (a *Aviary) RevokeCollaboratorProject(ctx context.Context, email, projectID string) error {
	if err := a.store.RevokeProject(ctx, email, projectID); err != nil {
		return err
	}
	a.removeSuperuserFromProject(projectID, email)
	return nil
}

// removeSuperuserFromProject deletes an email's _superusers record from a
// running project. It is a best-effort no-op when the project is stopped.
func (a *Aviary) removeSuperuserFromProject(projectID, email string) {
	a.mu.Lock()
	cage := a.cages[projectID]
	a.mu.Unlock()
	if cage == nil {
		return
	}
	<-cage.ready
	if cage.startErr != nil || cage.app == nil {
		return
	}
	rec, err := cage.app.FindAuthRecordByEmail(core.CollectionNameSuperusers, email)
	if errors.Is(err, sql.ErrNoRows) {
		return
	}
	if err != nil {
		a.log.Warn("failed to look up superuser for removal", "project", projectID, "email", email, "error", err)
		return
	}
	if err := cage.app.Delete(rec); err != nil {
		a.log.Warn("failed to remove collaborator from project", "project", projectID, "email", email, "error", err)
	}
}

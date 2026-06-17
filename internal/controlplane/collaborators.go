package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Collaborator is a non-superuser identity granted scoped admin access to one
// or more individual projects. Its bcrypt password hash is propagated only to
// the projects it has been granted, so it administers those projects'
// dashboards without unlocking the whole instance.
type Collaborator struct {
	Email        string    `json:"email"`
	PasswordHash string    `json:"-"`
	Projects     []string  `json:"projects"`
	CreatedAt    time.Time `json:"created"`
}

// Invitation is a pending, single-use grant of project access to an email,
// redeemable with the raw token whose SHA-256 hash is stored here.
type Invitation struct {
	Email     string    `json:"email"`
	ProjectID string    `json:"projectId"`
	ExpiresAt time.Time `json:"expires"`
	CreatedAt time.Time `json:"created"`
}

// CreateInvitation stores a pending invitation keyed by the token hash.
func (s *Store) CreateInvitation(ctx context.Context, tokenHash, email, projectID string, expires time.Time) error {
	now := formatTime(s.now().UTC())
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO invitations (token_hash, email, project_id, expires_at, created_at) VALUES (?, ?, ?, ?, ?)`,
		tokenHash, email, projectID, formatTime(expires.UTC()), now)
	if err != nil {
		if isUniqueViolation(err) {
			return ErrExists
		}
		return fmt.Errorf("controlplane: create invitation: %w", err)
	}
	return nil
}

// GetInvitation returns the invitation for the given token hash, or ErrNotFound.
// Expiry is not enforced here; callers should compare ExpiresAt.
func (s *Store) GetInvitation(ctx context.Context, tokenHash string) (*Invitation, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT email, project_id, expires_at, created_at FROM invitations WHERE token_hash = ?`, tokenHash)
	var (
		inv                    Invitation
		expiresRaw, createdRaw string
	)
	err := row.Scan(&inv.Email, &inv.ProjectID, &expiresRaw, &createdRaw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("controlplane: get invitation: %w", err)
	}
	if inv.ExpiresAt, err = parseTime(expiresRaw); err != nil {
		return nil, fmt.Errorf("parse expires_at: %w", err)
	}
	if inv.CreatedAt, err = parseTime(createdRaw); err != nil {
		return nil, fmt.Errorf("parse created_at: %w", err)
	}
	return &inv, nil
}

// DeleteInvitation removes a (typically just-redeemed) invitation.
func (s *Store) DeleteInvitation(ctx context.Context, tokenHash string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM invitations WHERE token_hash = ?`, tokenHash)
	if err != nil {
		return fmt.Errorf("controlplane: delete invitation: %w", err)
	}
	return requireAffected(res)
}

// ListInvitations returns all pending invitations, newest first.
func (s *Store) ListInvitations(ctx context.Context) ([]Invitation, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT email, project_id, expires_at, created_at FROM invitations ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("controlplane: list invitations: %w", err)
	}
	defer rows.Close()

	var out []Invitation
	for rows.Next() {
		var (
			inv                    Invitation
			expiresRaw, createdRaw string
		)
		if err := rows.Scan(&inv.Email, &inv.ProjectID, &expiresRaw, &createdRaw); err != nil {
			return nil, err
		}
		if inv.ExpiresAt, err = parseTime(expiresRaw); err != nil {
			return nil, fmt.Errorf("parse expires_at: %w", err)
		}
		if inv.CreatedAt, err = parseTime(createdRaw); err != nil {
			return nil, fmt.Errorf("parse created_at: %w", err)
		}
		out = append(out, inv)
	}
	return out, rows.Err()
}

// UpsertCollaborator creates or updates a collaborator's password hash,
// preserving the original created_at on update.
func (s *Store) UpsertCollaborator(ctx context.Context, email, passwordHash string) error {
	now := formatTime(s.now().UTC())
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO collaborators (email, password_hash, created_at) VALUES (?, ?, ?)
		 ON CONFLICT(email) DO UPDATE SET password_hash = excluded.password_hash`,
		email, passwordHash, now)
	if err != nil {
		return fmt.Errorf("controlplane: upsert collaborator: %w", err)
	}
	return nil
}

// GetCollaborator returns the collaborator and their granted project ids, or
// ErrNotFound.
func (s *Store) GetCollaborator(ctx context.Context, email string) (*Collaborator, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT email, password_hash, created_at FROM collaborators WHERE email = ?`, email)
	var (
		c          Collaborator
		createdRaw string
	)
	err := row.Scan(&c.Email, &c.PasswordHash, &createdRaw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("controlplane: get collaborator: %w", err)
	}
	if c.CreatedAt, err = parseTime(createdRaw); err != nil {
		return nil, fmt.Errorf("parse created_at: %w", err)
	}
	if c.Projects, err = s.ListCollaboratorProjects(ctx, email); err != nil {
		return nil, err
	}
	return &c, nil
}

// GrantProject grants a collaborator access to a project (idempotent).
func (s *Store) GrantProject(ctx context.Context, email, projectID string) error {
	now := formatTime(s.now().UTC())
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO collaborator_projects (email, project_id, created_at) VALUES (?, ?, ?)
		 ON CONFLICT(email, project_id) DO NOTHING`,
		email, projectID, now)
	if err != nil {
		return fmt.Errorf("controlplane: grant project: %w", err)
	}
	return nil
}

// RevokeProject removes a collaborator's access to a project.
func (s *Store) RevokeProject(ctx context.Context, email, projectID string) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM collaborator_projects WHERE email = ? AND project_id = ?`, email, projectID)
	if err != nil {
		return fmt.Errorf("controlplane: revoke project: %w", err)
	}
	return requireAffected(res)
}

// ListCollaboratorProjects returns the project ids a collaborator may access.
func (s *Store) ListCollaboratorProjects(ctx context.Context, email string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT project_id FROM collaborator_projects WHERE email = ? ORDER BY project_id`, email)
	if err != nil {
		return nil, fmt.Errorf("controlplane: list collaborator projects: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// CollaboratorHasProject reports whether the collaborator may access projectID.
func (s *Store) CollaboratorHasProject(ctx context.Context, email, projectID string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM collaborator_projects WHERE email = ? AND project_id = ?`,
		email, projectID).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("controlplane: check collaborator project: %w", err)
	}
	return n > 0, nil
}

// ListCollaborators returns all collaborators with their granted projects.
func (s *Store) ListCollaborators(ctx context.Context) ([]Collaborator, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT email, created_at FROM collaborators ORDER BY email`)
	if err != nil {
		return nil, fmt.Errorf("controlplane: list collaborators: %w", err)
	}
	defer rows.Close()

	var out []Collaborator
	for rows.Next() {
		var (
			c          Collaborator
			createdRaw string
		)
		if err := rows.Scan(&c.Email, &createdRaw); err != nil {
			return nil, err
		}
		if c.CreatedAt, err = parseTime(createdRaw); err != nil {
			return nil, fmt.Errorf("parse created_at: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		projects, err := s.ListCollaboratorProjects(ctx, out[i].Email)
		if err != nil {
			return nil, err
		}
		out[i].Projects = projects
	}
	return out, nil
}

// ProjectCollaborators returns the emails of collaborators granted access to a
// project, used when seeding a project's _superusers on boot.
func (s *Store) ProjectCollaborators(ctx context.Context, projectID string) ([]Collaborator, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT c.email, c.password_hash, c.created_at
		   FROM collaborators c
		   JOIN collaborator_projects cp ON cp.email = c.email
		  WHERE cp.project_id = ?
		  ORDER BY c.email`, projectID)
	if err != nil {
		return nil, fmt.Errorf("controlplane: project collaborators: %w", err)
	}
	defer rows.Close()

	var out []Collaborator
	for rows.Next() {
		var (
			c          Collaborator
			createdRaw string
		)
		if err := rows.Scan(&c.Email, &c.PasswordHash, &createdRaw); err != nil {
			return nil, err
		}
		if c.CreatedAt, err = parseTime(createdRaw); err != nil {
			return nil, fmt.Errorf("parse created_at: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// DeleteCollaborator removes a collaborator and all their project grants.
func (s *Store) DeleteCollaborator(ctx context.Context, email string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM collaborators WHERE email = ?`, email)
	if err != nil {
		return fmt.Errorf("controlplane: delete collaborator: %w", err)
	}
	if err := requireAffected(res); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM collaborator_projects WHERE email = ?`, email); err != nil {
		return fmt.Errorf("controlplane: delete collaborator grants: %w", err)
	}
	return nil
}

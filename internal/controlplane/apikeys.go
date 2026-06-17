package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// APIKey is a project-scoped, non-interactive credential. Only the SHA-256 hash
// of the raw token is stored, so a leaked database cannot be used to reconstruct
// usable keys. A key authorizes access to exactly one project — the file and
// deploy endpoints of that project — never instance-wide operations.
type APIKey struct {
	ID         string     `json:"id"`
	ProjectID  string     `json:"projectId"`
	Label      string     `json:"label"`
	KeyHash    string     `json:"-"`
	CreatedAt  time.Time  `json:"created"`
	LastUsedAt *time.Time `json:"lastUsed,omitempty"`
	ExpiresAt  *time.Time `json:"expires,omitempty"`
}

// CreateAPIKey stores a new key for projectID. keyHash is the hex SHA-256 of the
// raw token; expiresAt may be nil for a non-expiring key.
func (s *Store) CreateAPIKey(ctx context.Context, id, projectID, label, keyHash string, expiresAt *time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO api_keys (id, project_id, label, key_hash, created_at, last_used_at, expires_at)
		 VALUES (?, ?, ?, ?, ?, NULL, ?)`,
		id, projectID, label, keyHash, formatTime(s.now().UTC()), formatNullableTime(expiresAt))
	if err != nil {
		if isUniqueViolation(err) {
			return ErrExists
		}
		return fmt.Errorf("controlplane: create api key: %w", err)
	}
	return nil
}

// ListAPIKeys returns the keys for a project, newest first. The hash is never
// included.
func (s *Store) ListAPIKeys(ctx context.Context, projectID string) ([]APIKey, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, project_id, label, created_at, last_used_at, expires_at
		   FROM api_keys WHERE project_id = ? ORDER BY created_at DESC`, projectID)
	if err != nil {
		return nil, fmt.Errorf("controlplane: list api keys: %w", err)
	}
	defer rows.Close()

	out := make([]APIKey, 0)
	for rows.Next() {
		k, err := scanAPIKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *k)
	}
	return out, rows.Err()
}

// APIKeyByHash returns the key whose token hashes to keyHash, or ErrNotFound.
// Expiry is not enforced here; callers should consult ExpiresAt.
func (s *Store) APIKeyByHash(ctx context.Context, keyHash string) (*APIKey, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, project_id, label, created_at, last_used_at, expires_at
		   FROM api_keys WHERE key_hash = ?`, keyHash)
	k, err := scanAPIKey(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("controlplane: get api key by hash: %w", err)
	}
	return k, nil
}

// TouchAPIKey records that a key was just used (best-effort; callers may ignore
// the error).
func (s *Store) TouchAPIKey(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE api_keys SET last_used_at = ? WHERE id = ?`,
		formatTime(s.now().UTC()), id)
	if err != nil {
		return fmt.Errorf("controlplane: touch api key: %w", err)
	}
	return nil
}

// DeleteAPIKey removes a key, scoped to its project so a key for one project can
// never be revoked via another. Returns ErrNotFound if no such key exists.
func (s *Store) DeleteAPIKey(ctx context.Context, projectID, id string) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM api_keys WHERE id = ? AND project_id = ?`, id, projectID)
	if err != nil {
		return fmt.Errorf("controlplane: delete api key: %w", err)
	}
	return requireAffected(res)
}

// scanAPIKey reads a row into an APIKey. The key hash is never selected on read
// paths, so it is left empty.
func scanAPIKey(sc scanner) (*APIKey, error) {
	var (
		k                   APIKey
		createdRaw          string
		lastUsedRaw, expRaw sql.NullString
	)
	if err := sc.Scan(&k.ID, &k.ProjectID, &k.Label, &createdRaw, &lastUsedRaw, &expRaw); err != nil {
		return nil, err
	}

	var err error
	if k.CreatedAt, err = parseTime(createdRaw); err != nil {
		return nil, fmt.Errorf("parse created_at: %w", err)
	}
	if k.LastUsedAt, err = parseNullableTime(lastUsedRaw); err != nil {
		return nil, fmt.Errorf("parse last_used_at: %w", err)
	}
	if k.ExpiresAt, err = parseNullableTime(expRaw); err != nil {
		return nil, fmt.Errorf("parse expires_at: %w", err)
	}
	return &k, nil
}

func formatNullableTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return formatTime(*t)
}

func parseNullableTime(ns sql.NullString) (*time.Time, error) {
	if !ns.Valid || ns.String == "" {
		return nil, nil
	}
	t, err := parseTime(ns.String)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

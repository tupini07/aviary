// Package controlplane implements the persistent registry of projects that
// backs the Aviary control plane. It is the source of truth for which projects
// exist and their administrative state, independent of whether a given
// project's PocketBase app is currently booted.
package controlplane

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver, registered as "sqlite"
)

// Errors returned by the Store.
var (
	// ErrNotFound is returned when a project does not exist.
	ErrNotFound = errors.New("controlplane: project not found")
	// ErrExists is returned when creating a project whose id is already taken.
	ErrExists = errors.New("controlplane: project already exists")
	// ErrInvalidID is returned when a project id fails validation.
	ErrInvalidID = errors.New("controlplane: invalid project id")
	// ErrNoSuperuser is returned when no control-plane superuser is configured.
	ErrNoSuperuser = errors.New("controlplane: no superuser configured")
)

// Status is the administrative state of a project.
type Status string

const (
	// StatusActive means the project may be booted and serve traffic.
	StatusActive Status = "active"
	// StatusDisabled means the project exists but must not serve traffic.
	StatusDisabled Status = "disabled"
)

// idPattern is the canonical project-id rule: a DNS-label-safe slug usable as a
// subdomain. Lower-case alphanumerics and hyphens, 1-40 chars, leading alnum.
var idPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

// ValidID reports whether id is a well-formed project identifier.
func ValidID(id string) bool {
	return idPattern.MatchString(id)
}

// Project is a control-plane project record.
type Project struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Status    Status    `json:"status"`
	CreatedAt time.Time `json:"created"`
	UpdatedAt time.Time `json:"updated"`
}

// Store persists project records in a dedicated SQLite database.
//
// It is safe for concurrent use. Writes are serialized by the underlying
// single-writer SQLite connection.
type Store struct {
	db  *sql.DB
	now func() time.Time
}

// Open opens (creating if needed) the control-plane database at path and
// ensures its schema exists.
func Open(path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("controlplane: open db: %w", err)
	}
	// SQLite allows a single writer; cap connections to avoid "database is
	// locked" under concurrent writes from the control-plane API.
	db.SetMaxOpenConns(1)

	s := &Store{db: db, now: time.Now}
	if err := s.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the underlying database.
func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) migrate(ctx context.Context) error {
	const schema = `
CREATE TABLE IF NOT EXISTS projects (
	id         TEXT PRIMARY KEY,
	name       TEXT NOT NULL DEFAULT '',
	status     TEXT NOT NULL DEFAULT 'active',
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS superuser (
	id            INTEGER PRIMARY KEY CHECK (id = 1),
	email         TEXT NOT NULL,
	password_hash TEXT NOT NULL,
	updated_at    TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS kv (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS superuser_passkeys (
	credential_id TEXT PRIMARY KEY,
	label         TEXT NOT NULL DEFAULT '',
	data          TEXT NOT NULL,
	created_at    TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS collaborators (
	email         TEXT PRIMARY KEY,
	password_hash TEXT NOT NULL,
	created_at    TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS collaborator_projects (
	email      TEXT NOT NULL,
	project_id TEXT NOT NULL,
	created_at TEXT NOT NULL,
	PRIMARY KEY (email, project_id)
);

CREATE TABLE IF NOT EXISTS invitations (
	token_hash TEXT PRIMARY KEY,
	email      TEXT NOT NULL,
	project_id TEXT NOT NULL,
	expires_at TEXT NOT NULL,
	created_at TEXT NOT NULL
);`
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("controlplane: migrate: %w", err)
	}
	return nil
}

// Create inserts a new active project. It returns ErrInvalidID for a malformed
// id and ErrExists if the id is already taken.
func (s *Store) Create(ctx context.Context, id, name string) (*Project, error) {
	if !ValidID(id) {
		return nil, ErrInvalidID
	}

	now := s.now().UTC()
	p := &Project{
		ID:        id,
		Name:      name,
		Status:    StatusActive,
		CreatedAt: now,
		UpdatedAt: now,
	}

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO projects (id, name, status, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		p.ID, p.Name, string(p.Status), formatTime(p.CreatedAt), formatTime(p.UpdatedAt),
	)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrExists
		}
		return nil, fmt.Errorf("controlplane: create %q: %w", id, err)
	}
	return p, nil
}

// Get returns the project with the given id, or ErrNotFound.
func (s *Store) Get(ctx context.Context, id string) (*Project, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, name, status, created_at, updated_at FROM projects WHERE id = ?`, id)
	p, err := scanProject(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("controlplane: get %q: %w", id, err)
	}
	return p, nil
}

// List returns all projects ordered by id.
func (s *Store) List(ctx context.Context) ([]*Project, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, status, created_at, updated_at FROM projects ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("controlplane: list: %w", err)
	}
	defer rows.Close()

	var out []*Project
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, fmt.Errorf("controlplane: list scan: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("controlplane: list rows: %w", err)
	}
	return out, nil
}

// SetStatus updates a project's administrative status. Returns ErrNotFound if
// the project does not exist.
func (s *Store) SetStatus(ctx context.Context, id string, status Status) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE projects SET status = ?, updated_at = ? WHERE id = ?`,
		string(status), formatTime(s.now().UTC()), id)
	if err != nil {
		return fmt.Errorf("controlplane: set status %q: %w", id, err)
	}
	return requireAffected(res)
}

// SetName updates a project's display name. Returns ErrNotFound if the project
// does not exist.
func (s *Store) SetName(ctx context.Context, id, name string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE projects SET name = ?, updated_at = ? WHERE id = ?`,
		name, formatTime(s.now().UTC()), id)
	if err != nil {
		return fmt.Errorf("controlplane: set name %q: %w", id, err)
	}
	return requireAffected(res)
}

// Delete removes a project record. Returns ErrNotFound if it did not exist.
//
// Note: this only removes the registry entry; tearing down the project's data
// directory is the caller's (provisioning) responsibility.
func (s *Store) Delete(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM projects WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("controlplane: delete %q: %w", id, err)
	}
	if err := requireAffected(res); err != nil {
		return err
	}
	// Cascade: drop collaborator grants and pending invitations for the project.
	if _, err := s.db.ExecContext(ctx, `DELETE FROM collaborator_projects WHERE project_id = ?`, id); err != nil {
		return fmt.Errorf("controlplane: delete project grants %q: %w", id, err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM invitations WHERE project_id = ?`, id); err != nil {
		return fmt.Errorf("controlplane: delete project invitations %q: %w", id, err)
	}
	return nil
}

// --- superuser ---

// Superuser is the single control-plane administrator identity. Its bcrypt
// password hash is the canonical credential propagated to every project's
// _superusers collection.
type Superuser struct {
	Email        string    `json:"email"`
	PasswordHash string    `json:"-"`
	UpdatedAt    time.Time `json:"updated"`
}

// SetSuperuser upserts the single control-plane superuser identity. The hash
// must be a bcrypt hash (e.g. "$2a$...").
func (s *Store) SetSuperuser(ctx context.Context, email, passwordHash string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO superuser (id, email, password_hash, updated_at) VALUES (1, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET email = excluded.email, password_hash = excluded.password_hash, updated_at = excluded.updated_at`,
		email, passwordHash, formatTime(s.now().UTC()),
	)
	if err != nil {
		return fmt.Errorf("controlplane: set superuser: %w", err)
	}
	return nil
}

// GetSuperuser returns the control-plane superuser, or ErrNoSuperuser if none
// has been configured yet.
func (s *Store) GetSuperuser(ctx context.Context) (*Superuser, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT email, password_hash, updated_at FROM superuser WHERE id = 1`)

	var (
		su         Superuser
		updatedRaw string
	)
	err := row.Scan(&su.Email, &su.PasswordHash, &updatedRaw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoSuperuser
	}
	if err != nil {
		return nil, fmt.Errorf("controlplane: get superuser: %w", err)
	}
	if su.UpdatedAt, err = parseTime(updatedRaw); err != nil {
		return nil, fmt.Errorf("controlplane: parse superuser updated_at: %w", err)
	}
	return &su, nil
}

// HasSuperuser reports whether a control-plane superuser has been configured.
func (s *Store) HasSuperuser(ctx context.Context) (bool, error) {
	_, err := s.GetSuperuser(ctx)
	if errors.Is(err, ErrNoSuperuser) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// --- helpers ---

// SessionKey returns the persisted session-signing key, generating and storing
// a new random 32-byte key on first use. Persisting it means login sessions
// survive process restarts.
func (s *Store) SessionKey(ctx context.Context) ([]byte, error) {
	const key = "session_key"

	var hexVal string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM kv WHERE key = ?`, key).Scan(&hexVal)
	switch {
	case err == nil:
		b, decErr := hex.DecodeString(hexVal)
		if decErr != nil {
			return nil, fmt.Errorf("controlplane: decode session key: %w", decErr)
		}
		return b, nil
	case !errors.Is(err, sql.ErrNoRows):
		return nil, fmt.Errorf("controlplane: read session key: %w", err)
	}

	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return nil, fmt.Errorf("controlplane: generate session key: %w", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO kv (key, value) VALUES (?, ?)`, key, hex.EncodeToString(buf)); err != nil {
		return nil, fmt.Errorf("controlplane: persist session key: %w", err)
	}
	return buf, nil
}

const timeLayout = time.RFC3339Nano

func formatTime(t time.Time) string { return t.UTC().Format(timeLayout) }

func parseTime(s string) (time.Time, error) { return time.Parse(timeLayout, s) }

// scanner is satisfied by both *sql.Row and *sql.Rows.
type scanner interface {
	Scan(dest ...any) error
}

func scanProject(sc scanner) (*Project, error) {
	var (
		p                      Project
		status                 string
		createdRaw, updatedRaw string
	)
	if err := sc.Scan(&p.ID, &p.Name, &status, &createdRaw, &updatedRaw); err != nil {
		return nil, err
	}
	p.Status = Status(status)

	var err error
	if p.CreatedAt, err = parseTime(createdRaw); err != nil {
		return nil, fmt.Errorf("parse created_at: %w", err)
	}
	if p.UpdatedAt, err = parseTime(updatedRaw); err != nil {
		return nil, fmt.Errorf("parse updated_at: %w", err)
	}
	return &p, nil
}

func requireAffected(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// isUniqueViolation reports whether err is a SQLite UNIQUE/PRIMARY KEY conflict.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	// modernc.org/sqlite surfaces constraint failures in the error text;
	// matching the standard SQLite phrasing keeps this driver-agnostic.
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") ||
		strings.Contains(msg, "constraint failed: UNIQUE")
}

// SuperuserPasskey is a stored WebAuthn credential for the control-plane
// superuser. Data holds the marshaled webauthn.Credential.
type SuperuserPasskey struct {
	CredentialID string    `json:"credentialId"`
	Label        string    `json:"label"`
	Data         []byte    `json:"-"`
	CreatedAt    time.Time `json:"created"`
}

// AddSuperuserPasskey stores a new credential for the superuser.
func (s *Store) AddSuperuserPasskey(ctx context.Context, credentialID, label string, data []byte) error {
	now := s.now().UTC().Format(time.RFC3339Nano)
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO superuser_passkeys (credential_id, label, data, created_at) VALUES (?, ?, ?, ?)`,
		credentialID, label, string(data), now)
	if err != nil {
		if isUniqueViolation(err) {
			return ErrExists
		}
		return fmt.Errorf("controlplane: add superuser passkey: %w", err)
	}
	return nil
}

// ListSuperuserPasskeys returns all stored superuser credentials.
func (s *Store) ListSuperuserPasskeys(ctx context.Context) ([]SuperuserPasskey, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT credential_id, label, data, created_at FROM superuser_passkeys ORDER BY created_at`)
	if err != nil {
		return nil, fmt.Errorf("controlplane: list superuser passkeys: %w", err)
	}
	defer rows.Close()

	var out []SuperuserPasskey
	for rows.Next() {
		var (
			pk         SuperuserPasskey
			data       string
			createdRaw string
		)
		if err := rows.Scan(&pk.CredentialID, &pk.Label, &data, &createdRaw); err != nil {
			return nil, err
		}
		pk.Data = []byte(data)
		if pk.CreatedAt, err = parseTime(createdRaw); err != nil {
			return nil, fmt.Errorf("parse created_at: %w", err)
		}
		out = append(out, pk)
	}
	return out, rows.Err()
}

// UpdateSuperuserPasskey replaces the stored credential data (e.g. to persist
// an updated sign-count after a successful assertion).
func (s *Store) UpdateSuperuserPasskey(ctx context.Context, credentialID string, data []byte) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE superuser_passkeys SET data = ? WHERE credential_id = ?`,
		string(data), credentialID)
	if err != nil {
		return fmt.Errorf("controlplane: update superuser passkey: %w", err)
	}
	return requireAffected(res)
}

// DeleteSuperuserPasskey removes a stored credential.
func (s *Store) DeleteSuperuserPasskey(ctx context.Context, credentialID string) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM superuser_passkeys WHERE credential_id = ?`, credentialID)
	if err != nil {
		return fmt.Errorf("controlplane: delete superuser passkey: %w", err)
	}
	return requireAffected(res)
}

// HasSuperuserPasskeys reports whether any superuser credential is registered.
func (s *Store) HasSuperuserPasskeys(ctx context.Context) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM superuser_passkeys`).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("controlplane: count superuser passkeys: %w", err)
	}
	return n > 0, nil
}

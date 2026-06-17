// Package controlplane implements the persistent registry of projects that
// backs the Aviary control plane. It is the source of truth for which projects
// exist and their administrative state, independent of whether a given
// project's PocketBase app is currently booted.
package controlplane

import (
	"context"
	"database/sql"
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

// Delete removes a project record. Returns ErrNotFound if it did not exist.
//
// Note: this only removes the registry entry; tearing down the project's data
// directory is the caller's (provisioning) responsibility.
func (s *Store) Delete(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM projects WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("controlplane: delete %q: %w", id, err)
	}
	return requireAffected(res)
}

// --- helpers ---

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

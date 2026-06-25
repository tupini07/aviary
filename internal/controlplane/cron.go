package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// CronJob is a scheduled invocation owned by the control plane. The control
// plane (not the project) keeps the schedule and wakes the project's cage on
// demand to run the job, so scheduled work survives idle eviction. A job targets
// a path served by the project (by convention a POST /cron/... route registered
// from the project's JS hooks).
type CronJob struct {
	ID         string     `json:"id"`
	ProjectID  string     `json:"projectId"`
	Schedule   string     `json:"schedule"` // 5-field cron expression or @macro
	Path       string     `json:"path"`     // target path, e.g. /cron/cleanup
	Enabled    bool       `json:"enabled"`
	CreatedAt  time.Time  `json:"created"`
	UpdatedAt  time.Time  `json:"updated"`
	LastRunAt  *time.Time `json:"lastRun,omitempty"`
	LastStatus int        `json:"lastStatus"` // last HTTP status; 0 = never run
	LastError  string     `json:"lastError,omitempty"`
}

// CreateCronJob stores a new cron job for projectID.
func (s *Store) CreateCronJob(ctx context.Context, id, projectID, schedule, path string, enabled bool) error {
	now := formatTime(s.now().UTC())
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO cron_jobs (id, project_id, schedule, path, enabled, created_at, updated_at, last_run_at, last_status, last_error)
		 VALUES (?, ?, ?, ?, ?, ?, ?, NULL, 0, '')`,
		id, projectID, schedule, path, boolToInt(enabled), now, now)
	if err != nil {
		if isUniqueViolation(err) {
			return ErrExists
		}
		return fmt.Errorf("controlplane: create cron job: %w", err)
	}
	return nil
}

// ListCronJobs returns the cron jobs for a project, newest first.
func (s *Store) ListCronJobs(ctx context.Context, projectID string) ([]CronJob, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, project_id, schedule, path, enabled, created_at, updated_at, last_run_at, last_status, last_error
		   FROM cron_jobs WHERE project_id = ? ORDER BY created_at DESC`, projectID)
	if err != nil {
		return nil, fmt.Errorf("controlplane: list cron jobs: %w", err)
	}
	defer rows.Close()
	return scanCronJobs(rows)
}

// ListAllCronJobs returns every cron job across all projects. It is used by the
// scheduler to (re)load jobs at startup.
func (s *Store) ListAllCronJobs(ctx context.Context) ([]CronJob, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, project_id, schedule, path, enabled, created_at, updated_at, last_run_at, last_status, last_error
		   FROM cron_jobs ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("controlplane: list all cron jobs: %w", err)
	}
	defer rows.Close()
	return scanCronJobs(rows)
}

// GetCronJob returns a single cron job scoped to its project, or ErrNotFound.
func (s *Store) GetCronJob(ctx context.Context, projectID, id string) (*CronJob, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, project_id, schedule, path, enabled, created_at, updated_at, last_run_at, last_status, last_error
		   FROM cron_jobs WHERE id = ? AND project_id = ?`, id, projectID)
	j, err := scanCronJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("controlplane: get cron job: %w", err)
	}
	return j, nil
}

// UpdateCronJob replaces the schedule, path and enabled flag of an existing job,
// scoped to its project. Returns ErrNotFound if no such job exists.
func (s *Store) UpdateCronJob(ctx context.Context, projectID, id, schedule, path string, enabled bool) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE cron_jobs SET schedule = ?, path = ?, enabled = ?, updated_at = ?
		   WHERE id = ? AND project_id = ?`,
		schedule, path, boolToInt(enabled), formatTime(s.now().UTC()), id, projectID)
	if err != nil {
		return fmt.Errorf("controlplane: update cron job: %w", err)
	}
	return requireAffected(res)
}

// RecordCronRun stores the outcome of a job invocation (its run time, the HTTP
// status the target returned, and any transport-level error). Best-effort:
// callers may ignore the error.
func (s *Store) RecordCronRun(ctx context.Context, id string, runAt time.Time, status int, runErr string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE cron_jobs SET last_run_at = ?, last_status = ?, last_error = ? WHERE id = ?`,
		formatTime(runAt.UTC()), status, runErr, id)
	if err != nil {
		return fmt.Errorf("controlplane: record cron run: %w", err)
	}
	return nil
}

// DeleteCronJob removes a job scoped to its project. Returns ErrNotFound if no
// such job exists.
func (s *Store) DeleteCronJob(ctx context.Context, projectID, id string) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM cron_jobs WHERE id = ? AND project_id = ?`, id, projectID)
	if err != nil {
		return fmt.Errorf("controlplane: delete cron job: %w", err)
	}
	return requireAffected(res)
}

func scanCronJobs(rows *sql.Rows) ([]CronJob, error) {
	out := make([]CronJob, 0)
	for rows.Next() {
		j, err := scanCronJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *j)
	}
	return out, rows.Err()
}

func scanCronJob(sc scanner) (*CronJob, error) {
	var (
		j                      CronJob
		enabled                int
		createdRaw, updatedRaw string
		lastRunRaw             sql.NullString
	)
	if err := sc.Scan(&j.ID, &j.ProjectID, &j.Schedule, &j.Path, &enabled,
		&createdRaw, &updatedRaw, &lastRunRaw, &j.LastStatus, &j.LastError); err != nil {
		return nil, err
	}
	j.Enabled = enabled != 0

	var err error
	if j.CreatedAt, err = parseTime(createdRaw); err != nil {
		return nil, fmt.Errorf("parse created_at: %w", err)
	}
	if j.UpdatedAt, err = parseTime(updatedRaw); err != nil {
		return nil, fmt.Errorf("parse updated_at: %w", err)
	}
	if j.LastRunAt, err = parseNullableTime(lastRunRaw); err != nil {
		return nil, fmt.Errorf("parse last_run_at: %w", err)
	}
	return &j, nil
}

package aviary

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/cron"

	"github.com/tupini07/aviary/internal/controlplane"
)

// CronJob is a control-plane-owned scheduled invocation of a project route.
type CronJob = controlplane.CronJob

// cronJobTimeout bounds how long a single cron invocation may run before its
// request context is cancelled. Jobs are fired in their own goroutine, so this
// does not block the scheduler tick.
const cronJobTimeout = 30 * time.Second

// startCron creates the scheduler, loads every enabled job from the store and
// starts ticking. It is called once from New(), after the reaper is running.
func (a *Aviary) startCron() error {
	a.cron = cron.New()
	jobs, err := a.store.ListAllCronJobs(context.Background())
	if err != nil {
		return err
	}
	for _, j := range jobs {
		if !j.Enabled {
			continue
		}
		if err := a.scheduleCronJob(j); err != nil {
			a.log.Warn("skipping invalid cron job", "id", j.ID, "project", j.ProjectID, "error", err)
		}
	}
	a.cron.Start()
	return nil
}

// scheduleCronJob registers (or replaces) a job in the in-memory scheduler.
func (a *Aviary) scheduleCronJob(j CronJob) error {
	projectID, id := j.ProjectID, j.ID
	return a.cron.Add(id, j.Schedule, func() { a.runCronJob(projectID, id) })
}

// syncCronJob reconciles the scheduler with a job's current state: register it
// when enabled, remove it otherwise. Safe to call before the scheduler exists.
func (a *Aviary) syncCronJob(j *CronJob) {
	if a.cron == nil {
		return
	}
	if j.Enabled {
		if err := a.scheduleCronJob(*j); err != nil {
			a.log.Warn("failed to schedule cron job", "id", j.ID, "error", err)
		}
		return
	}
	a.cron.Remove(j.ID)
}

// unscheduleCronJob drops a job from the scheduler (e.g. after deletion).
func (a *Aviary) unscheduleCronJob(id string) {
	if a.cron != nil {
		a.cron.Remove(id)
	}
}

// runCronJob executes a job with per-job single-flight and records the outcome.
// A tick is skipped (logged) when the previous run of the same job is still in
// flight, matching the "no overlap" semantics of typical cron runners.
func (a *Aviary) runCronJob(projectID, id string) {
	a.cronMu.Lock()
	if _, busy := a.cronRunning[id]; busy {
		a.cronMu.Unlock()
		a.log.Info("cron job still running; skipping tick", "id", id, "project", projectID)
		return
	}
	a.cronRunning[id] = struct{}{}
	a.cronMu.Unlock()
	defer func() {
		a.cronMu.Lock()
		delete(a.cronRunning, id)
		a.cronMu.Unlock()
	}()

	status, runErr := a.executeCronJob(context.Background(), projectID, id)
	msg := ""
	if runErr != nil {
		msg = runErr.Error()
		a.log.Warn("cron job failed", "id", id, "project", projectID, "status", status, "error", msg)
	} else {
		a.log.Info("cron job ran", "id", id, "project", projectID, "status", status)
	}
	if err := a.store.RecordCronRun(context.Background(), id, time.Now().UTC(), status, msg); err != nil {
		a.log.Warn("failed to record cron run", "id", id, "error", err)
	}
}

// executeCronJob wakes the project's cage and invokes the job's target route
// in-process, authenticated with a freshly minted superuser token. It returns
// the HTTP status the route produced (0 when the cage could not be reached).
func (a *Aviary) executeCronJob(ctx context.Context, projectID, id string) (int, error) {
	job, err := a.store.GetCronJob(ctx, projectID, id)
	if err != nil {
		return 0, err // deleted between scheduling and firing
	}
	p, err := a.store.Get(ctx, projectID)
	if err != nil {
		return 0, fmt.Errorf("load project: %w", err)
	}
	if p.Status != StatusActive {
		return 0, errors.New("project not active")
	}

	cage, err := a.getCage(projectID, p.SPA)
	if err != nil {
		return 0, fmt.Errorf("start project: %w", err)
	}

	su, err := a.GetSuperuser(ctx)
	if err != nil {
		return 0, errors.New("no control-plane superuser configured")
	}
	record, err := cage.app.FindAuthRecordByEmail(core.CollectionNameSuperusers, su.Email)
	if err != nil {
		return 0, fmt.Errorf("mint token: %w", err)
	}
	record.IgnoreEmailVisibility(true)
	token, err := record.NewAuthToken()
	if err != nil {
		return 0, fmt.Errorf("mint token: %w", err)
	}

	reqCtx, cancel := context.WithTimeout(ctx, cronJobTimeout)
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, job.Path, nil).WithContext(reqCtx)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	cage.handler.ServeHTTP(rec, req)

	if rec.Code >= http.StatusBadRequest {
		return rec.Code, fmt.Errorf("target returned %d", rec.Code)
	}
	return rec.Code, nil
}

// --- validation helpers ---

// normalizeCronPath enforces the convention that cron targets are POST routes
// under /cron/. A bare name (no leading slash) is treated as relative to /cron/.
func normalizeCronPath(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", errors.New("path is required")
	}
	if !strings.HasPrefix(p, "/") {
		p = "/cron/" + p
	}
	if strings.Contains(p, "..") {
		return "", errors.New("path must not contain '..'")
	}
	if !strings.HasPrefix(p, "/cron/") || strings.TrimPrefix(p, "/cron/") == "" {
		return "", errors.New("cron target path must be under /cron/ (e.g. /cron/cleanup)")
	}
	return p, nil
}

// validateCronSchedule rejects empty or syntactically invalid cron expressions.
func validateCronSchedule(expr string) error {
	if strings.TrimSpace(expr) == "" {
		return errors.New("schedule is required")
	}
	if _, err := cron.NewSchedule(expr); err != nil {
		return errors.New("invalid cron schedule")
	}
	return nil
}

// newCronJobID mints a short, globally unique identifier for a cron job.
func newCronJobID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// --- HTTP handlers (control plane) ---

type cronJobRequest struct {
	Schedule string `json:"schedule"`
	Path     string `json:"path"`
	Enabled  *bool  `json:"enabled,omitempty"`
}

// apiListCrons returns a project's cron jobs.
func (a *Aviary) apiListCrons(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := a.authorizeProjectAdmin(w, r, id); !ok {
		return
	}
	jobs, err := a.store.ListCronJobs(r.Context(), id)
	if err != nil {
		a.apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, jobs)
}

// apiCreateCron registers a new cron job for a project.
func (a *Aviary) apiCreateCron(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := a.authorizeProjectAdmin(w, r, id); !ok {
		return
	}
	if _, err := a.store.Get(r.Context(), id); err != nil {
		a.apiError(w, http.StatusNotFound, "project not found")
		return
	}
	var req cronJobRequest
	if !decodeJSON(w, r, &req, a) {
		return
	}
	if err := validateCronSchedule(req.Schedule); err != nil {
		a.apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	path, err := normalizeCronPath(req.Path)
	if err != nil {
		a.apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	jobID, err := newCronJobID()
	if err != nil {
		a.apiError(w, http.StatusInternalServerError, "failed to generate id")
		return
	}
	if err := a.store.CreateCronJob(r.Context(), jobID, id, strings.TrimSpace(req.Schedule), path, enabled); err != nil {
		a.apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	job, err := a.store.GetCronJob(r.Context(), id, jobID)
	if err != nil {
		a.apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	a.syncCronJob(job)
	writeJSON(w, http.StatusCreated, job)
}

// apiUpdateCron edits an existing cron job (schedule/path/enabled). Omitted or
// blank fields are left unchanged.
func (a *Aviary) apiUpdateCron(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	cronID := r.PathValue("cronId")
	if _, ok := a.authorizeProjectAdmin(w, r, id); !ok {
		return
	}
	existing, err := a.store.GetCronJob(r.Context(), id, cronID)
	if errors.Is(err, controlplane.ErrNotFound) {
		a.apiError(w, http.StatusNotFound, "cron job not found")
		return
	}
	if err != nil {
		a.apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	var req cronJobRequest
	if !decodeJSON(w, r, &req, a) {
		return
	}

	schedule := existing.Schedule
	if strings.TrimSpace(req.Schedule) != "" {
		if err := validateCronSchedule(req.Schedule); err != nil {
			a.apiError(w, http.StatusBadRequest, err.Error())
			return
		}
		schedule = strings.TrimSpace(req.Schedule)
	}
	path := existing.Path
	if strings.TrimSpace(req.Path) != "" {
		if path, err = normalizeCronPath(req.Path); err != nil {
			a.apiError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	enabled := existing.Enabled
	if req.Enabled != nil {
		enabled = *req.Enabled
	}

	if err := a.store.UpdateCronJob(r.Context(), id, cronID, schedule, path, enabled); err != nil {
		a.apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	job, err := a.store.GetCronJob(r.Context(), id, cronID)
	if err != nil {
		a.apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	a.syncCronJob(job)
	writeJSON(w, http.StatusOK, job)
}

// apiDeleteCron removes a cron job.
func (a *Aviary) apiDeleteCron(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	cronID := r.PathValue("cronId")
	if _, ok := a.authorizeProjectAdmin(w, r, id); !ok {
		return
	}
	if err := a.store.DeleteCronJob(r.Context(), id, cronID); err != nil {
		if errors.Is(err, controlplane.ErrNotFound) {
			a.apiError(w, http.StatusNotFound, "cron job not found")
			return
		}
		a.apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	a.unscheduleCronJob(cronID)
	w.WriteHeader(http.StatusNoContent)
}

// apiRunCron triggers a job immediately (for testing), regardless of its enabled
// flag, and returns the job with its refreshed last-run outcome.
func (a *Aviary) apiRunCron(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	cronID := r.PathValue("cronId")
	if _, ok := a.authorizeProjectAdmin(w, r, id); !ok {
		return
	}
	if _, err := a.store.GetCronJob(r.Context(), id, cronID); err != nil {
		if errors.Is(err, controlplane.ErrNotFound) {
			a.apiError(w, http.StatusNotFound, "cron job not found")
			return
		}
		a.apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	a.runCronJob(id, cronID)
	job, err := a.store.GetCronJob(r.Context(), id, cronID)
	if err != nil {
		a.apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, job)
}

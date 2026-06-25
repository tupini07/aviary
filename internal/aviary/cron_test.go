package aviary

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

// createCron is a small helper that creates a cron job through the control-plane
// API and returns the decoded job.
func createCron(t *testing.T, av *Aviary, project string, sess *http.Cookie, body cronJobRequest) CronJob {
	t.Helper()
	rec := doControl(t, av, http.MethodPost, "/api/projects/"+project+"/crons", body, sess)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create cron: status %d body %s", rec.Code, rec.Body.String())
	}
	var job CronJob
	if err := json.Unmarshal(rec.Body.Bytes(), &job); err != nil {
		t.Fatalf("decode created cron: %v", err)
	}
	return job
}

// TestCronCRUD exercises the full lifecycle of a cron job through the API.
func TestCronCRUD(t *testing.T) {
	av := newTestAviary(t)
	sess := loginAs(t, av, "admin@example.com", "password123")
	doControl(t, av, http.MethodPost, "/api/projects", createProjectRequest{ID: "alpha"}, sess)

	// Empty project => empty listing.
	rec := doControl(t, av, http.MethodGet, "/api/projects/alpha/crons", nil, sess)
	if rec.Code != http.StatusOK {
		t.Fatalf("list empty: status %d body %s", rec.Code, rec.Body.String())
	}
	var list []CronJob
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list) != 0 {
		t.Fatalf("expected empty listing, got %+v", list)
	}

	// Create. A bare name is normalized under /cron/.
	job := createCron(t, av, "alpha", sess, cronJobRequest{Schedule: "@daily", Path: "cleanup"})
	if job.Path != "/cron/cleanup" {
		t.Fatalf("path = %q, want /cron/cleanup", job.Path)
	}
	if !job.Enabled {
		t.Fatalf("job should default to enabled")
	}
	if job.LastStatus != 0 {
		t.Fatalf("new job lastStatus = %d, want 0", job.LastStatus)
	}

	// List shows it.
	rec = doControl(t, av, http.MethodGet, "/api/projects/alpha/crons", nil, sess)
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list) != 1 || list[0].ID != job.ID {
		t.Fatalf("listing = %+v, want one job %s", list, job.ID)
	}

	// Update schedule + disable.
	disabled := false
	rec = doControl(t, av, http.MethodPatch, "/api/projects/alpha/crons/"+job.ID,
		cronJobRequest{Schedule: "@hourly", Enabled: &disabled}, sess)
	if rec.Code != http.StatusOK {
		t.Fatalf("update: status %d body %s", rec.Code, rec.Body.String())
	}
	var updated CronJob
	_ = json.Unmarshal(rec.Body.Bytes(), &updated)
	if updated.Schedule != "@hourly" || updated.Enabled {
		t.Fatalf("update result = %+v, want @hourly disabled", updated)
	}
	if updated.Path != "/cron/cleanup" {
		t.Fatalf("omitted path should be unchanged, got %q", updated.Path)
	}

	// Delete.
	rec = doControl(t, av, http.MethodDelete, "/api/projects/alpha/crons/"+job.ID, nil, sess)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete: status %d body %s", rec.Code, rec.Body.String())
	}
	rec = doControl(t, av, http.MethodGet, "/api/projects/alpha/crons", nil, sess)
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list) != 0 {
		t.Fatalf("listing after delete = %+v, want empty", list)
	}
}

// TestCronValidation rejects bad schedules and out-of-convention target paths.
func TestCronValidation(t *testing.T) {
	av := newTestAviary(t)
	sess := loginAs(t, av, "admin@example.com", "password123")
	doControl(t, av, http.MethodPost, "/api/projects", createProjectRequest{ID: "alpha"}, sess)

	cases := []struct {
		name string
		body cronJobRequest
	}{
		{"bad schedule", cronJobRequest{Schedule: "not a cron", Path: "/cron/x"}},
		{"empty schedule", cronJobRequest{Schedule: "", Path: "/cron/x"}},
		{"empty path", cronJobRequest{Schedule: "@daily", Path: ""}},
		{"traversal path", cronJobRequest{Schedule: "@daily", Path: "/cron/../etc"}},
	}
	for _, tc := range cases {
		rec := doControl(t, av, http.MethodPost, "/api/projects/alpha/crons", tc.body, sess)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status %d, want 400 (body %s)", tc.name, rec.Code, rec.Body.String())
		}
	}
}

// TestCronRejectAPIKey verifies cron management is owner-only: project-scoped
// API keys must not list, create, or run jobs (they execute server-side code).
func TestCronRejectAPIKey(t *testing.T) {
	av := newTestAviary(t)
	sess := loginAs(t, av, "admin@example.com", "password123")
	doControl(t, av, http.MethodPost, "/api/projects", createProjectRequest{ID: "alpha"}, sess)
	key := mintKey(t, av, "alpha", sess, createAPIKeyRequest{Label: "ci"})

	rec := doControlBearer(t, av, http.MethodGet, "/api/projects/alpha/crons", nil, key.Token)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("bearer crons list: status %d, want 403", rec.Code)
	}
	rec = doControlBearer(t, av, http.MethodPost, "/api/projects/alpha/crons",
		cronJobRequest{Schedule: "@daily", Path: "/cron/x"}, key.Token)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("bearer crons create: status %d, want 403", rec.Code)
	}
}

// TestCronRunInvokesRoute is the end-to-end proof of the invoker: it registers a
// /cron/ route via JS hooks that requires authentication, creates a job, then
// triggers it with "run now" and asserts the route ran with a valid superuser
// token (HTTP 200, no error recorded).
func TestCronRunInvokesRoute(t *testing.T) {
	av := newTestAviary(t)
	sess := loginAs(t, av, "admin@example.com", "password123")
	doControl(t, av, http.MethodPost, "/api/projects", createProjectRequest{ID: "alpha"}, sess)

	// The hook returns 401 unless the request carries an auth identity, proving
	// the invoker authenticates with a minted superuser token.
	hook := `routerAdd("POST", "/cron/ping", (e) => {
		if (!e.auth) { return e.json(401, { ok: false }) }
		return e.json(200, { ok: true })
	})`
	rec := doControl(t, av, http.MethodPut, "/api/projects/alpha/hooks/content",
		fileContent{Path: "main.pb.js", Content: hook}, sess)
	if rec.Code != http.StatusOK {
		t.Fatalf("write hook: status %d body %s", rec.Code, rec.Body.String())
	}

	job := createCron(t, av, "alpha", sess, cronJobRequest{Schedule: "@daily", Path: "/cron/ping"})

	rec = doControl(t, av, http.MethodPost, "/api/projects/alpha/crons/"+job.ID+"/run", nil, sess)
	if rec.Code != http.StatusOK {
		t.Fatalf("run cron: status %d body %s", rec.Code, rec.Body.String())
	}
	var ran CronJob
	_ = json.Unmarshal(rec.Body.Bytes(), &ran)
	if ran.LastStatus != http.StatusOK {
		t.Fatalf("lastStatus = %d (err %q), want 200", ran.LastStatus, ran.LastError)
	}
	if ran.LastError != "" {
		t.Fatalf("lastError = %q, want empty", ran.LastError)
	}
	if ran.LastRunAt == nil {
		t.Fatalf("lastRun not recorded")
	}
}

// TestCronRunRecordsFailure ensures a failing target is reflected in the job's
// last-run status/error.
func TestCronRunRecordsFailure(t *testing.T) {
	av := newTestAviary(t)
	sess := loginAs(t, av, "admin@example.com", "password123")
	doControl(t, av, http.MethodPost, "/api/projects", createProjectRequest{ID: "alpha"}, sess)

	// No hook registers /cron/missing, so the route 404s.
	job := createCron(t, av, "alpha", sess, cronJobRequest{Schedule: "@daily", Path: "/cron/missing"})
	rec := doControl(t, av, http.MethodPost, "/api/projects/alpha/crons/"+job.ID+"/run", nil, sess)
	if rec.Code != http.StatusOK {
		t.Fatalf("run cron: status %d body %s", rec.Code, rec.Body.String())
	}
	var ran CronJob
	_ = json.Unmarshal(rec.Body.Bytes(), &ran)
	if ran.LastStatus != http.StatusNotFound {
		t.Fatalf("lastStatus = %d, want 404", ran.LastStatus)
	}
	if ran.LastError == "" {
		t.Fatalf("expected a recorded error for a 404 target")
	}
}

// TestCronCascadeDelete verifies that deleting a project removes its cron jobs.
func TestCronCascadeDelete(t *testing.T) {
	av := newTestAviary(t)
	sess := loginAs(t, av, "admin@example.com", "password123")
	doControl(t, av, http.MethodPost, "/api/projects", createProjectRequest{ID: "alpha"}, sess)
	createCron(t, av, "alpha", sess, cronJobRequest{Schedule: "@daily", Path: "/cron/x"})

	rec := doControl(t, av, http.MethodDelete, "/api/projects/alpha", nil, sess)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete project: status %d body %s", rec.Code, rec.Body.String())
	}
	jobs, err := av.store.ListAllCronJobs(context.Background())
	if err != nil {
		t.Fatalf("list all crons: %v", err)
	}
	if len(jobs) != 0 {
		t.Fatalf("cron jobs not cascaded: %+v", jobs)
	}
}

// TestCronSchedulerRegistersEnabled verifies enabled jobs are registered in the
// scheduler at startup and disabled jobs are not.
func TestCronSchedulerRegistersEnabled(t *testing.T) {
	av := newTestAviary(t)
	sess := loginAs(t, av, "admin@example.com", "password123")
	doControl(t, av, http.MethodPost, "/api/projects", createProjectRequest{ID: "alpha"}, sess)

	enabledJob := createCron(t, av, "alpha", sess, cronJobRequest{Schedule: "@daily", Path: "/cron/a"})
	disabledFlag := false
	disabledJob := createCron(t, av, "alpha", sess, cronJobRequest{Schedule: "@daily", Path: "/cron/b", Enabled: &disabledFlag})

	scheduled := map[string]bool{}
	for _, j := range av.cron.Jobs() {
		scheduled[j.Id()] = true
	}
	if !scheduled[enabledJob.ID] {
		t.Fatalf("enabled job %s not scheduled", enabledJob.ID)
	}
	if scheduled[disabledJob.ID] {
		t.Fatalf("disabled job %s should not be scheduled", disabledJob.ID)
	}
}

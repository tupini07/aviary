package aviary

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newTestAviary(t *testing.T, opts ...func(*Config)) *Aviary {
	t.Helper()
	cfg := Config{
		DataDir: t.TempDir(),
		IdleTTL: time.Minute,
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	av, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(av.Shutdown)
	return av
}

// withDashboardPassword enables PocketBase native superuser password login on
// projects, for tests that exercise the propagated-credential path directly.
func withDashboardPassword(c *Config) { c.AllowDashboardPassword = true }

func TestCreateProjectMakesDirAndRecord(t *testing.T) {
	av := newTestAviary(t)
	ctx := context.Background()

	p, err := av.CreateProject(ctx, "alpha", "Alpha")
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if p.ID != "alpha" || p.Status != StatusActive {
		t.Fatalf("unexpected project: %+v", p)
	}

	if fi, err := os.Stat(av.projectPath("alpha")); err != nil || !fi.IsDir() {
		t.Fatalf("project dir not created: err=%v", err)
	}

	got, err := av.GetProject(ctx, "alpha")
	if err != nil || got.ID != "alpha" {
		t.Fatalf("GetProject: got %+v err %v", got, err)
	}
}

func TestCreateProjectValidation(t *testing.T) {
	av := newTestAviary(t)
	ctx := context.Background()

	if _, err := av.CreateProject(ctx, "Bad ID", ""); !errors.Is(err, ErrInvalidID) {
		t.Fatalf("invalid id: got %v, want ErrInvalidID", err)
	}
	if _, err := av.CreateProject(ctx, "dup", ""); err != nil {
		t.Fatalf("first create: %v", err)
	}
	if _, err := av.CreateProject(ctx, "dup", ""); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate: got %v, want ErrExists", err)
	}
}

func TestDeleteProjectRemovesDirAndRecord(t *testing.T) {
	av := newTestAviary(t)
	ctx := context.Background()

	if _, err := av.CreateProject(ctx, "alpha", ""); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	// Drop a file in the project dir to prove the whole tree is removed.
	if err := os.WriteFile(filepath.Join(av.projectPath("alpha"), "data.db"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := av.DeleteProject(ctx, "alpha"); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}
	if _, err := os.Stat(av.projectPath("alpha")); !os.IsNotExist(err) {
		t.Fatalf("project dir still exists: err=%v", err)
	}
	if _, err := av.GetProject(ctx, "alpha"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetProject after delete: got %v, want ErrNotFound", err)
	}
	if err := av.DeleteProject(ctx, "alpha"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete missing: got %v, want ErrNotFound", err)
	}
}

func TestServeHTTPRoutingDecisions(t *testing.T) {
	av := newTestAviary(t)
	ctx := context.Background()

	if _, err := av.CreateProject(ctx, "disabled", ""); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if err := av.SetProjectStatus(ctx, "disabled", StatusDisabled); err != nil {
		t.Fatalf("SetProjectStatus: %v", err)
	}

	cases := []struct {
		name string
		host string
		want int
	}{
		{"unknown project", "ghost.localhost", http.StatusNotFound},
		{"invalid id", "BAD_ID.localhost", http.StatusBadRequest},
		{"disabled project", "disabled.localhost", http.StatusForbidden},
		{"control plane landing", "localhost", http.StatusOK},
		{"control plane via _console subdomain", "_console.apps.example.com", http.StatusOK},
		{"control plane via www subdomain", "www.apps.example.com", http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
			req.Host = tc.host
			rec := httptest.NewRecorder()
			av.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("host %q: status = %d, want %d (body: %s)", tc.host, rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

// TestServeHTTPBootsProvisionedProject is an integration test that boots a real
// PocketBase app for a provisioned project and checks its REST API responds.
func TestServeHTTPBootsProvisionedProject(t *testing.T) {
	av := newTestAviary(t)
	ctx := context.Background()

	if _, err := av.CreateProject(ctx, "alpha", "Alpha"); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	req.Host = "alpha.localhost"
	rec := httptest.NewRecorder()
	av.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("health status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	// A second project must get its own isolated data dir / database.
	if _, err := av.CreateProject(ctx, "beta", "Beta"); err != nil {
		t.Fatalf("CreateProject beta: %v", err)
	}
	req2 := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	req2.Host = "beta.localhost"
	rec2 := httptest.NewRecorder()
	av.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("beta health status = %d, want 200", rec2.Code)
	}

	for _, id := range []string{"alpha", "beta"} {
		if _, err := os.Stat(filepath.Join(av.projectPath(id), "data.db")); err != nil {
			t.Errorf("expected isolated data.db for %q: %v", id, err)
		}
	}
}

package aviary

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// authProjectSuperuser attempts a superuser auth-with-password against a
// project's PocketBase API via the Aviary front. Returns the HTTP status.
func authProjectSuperuser(t *testing.T, av *Aviary, project, email, password string) int {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"identity": email, "password": password})
	req := httptest.NewRequest(http.MethodPost,
		"/api/collections/_superusers/auth-with-password", strings.NewReader(string(body)))
	req.Host = project + ".localhost"
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	av.ServeHTTP(rec, req)
	return rec.Code
}

func TestSetAndAuthenticateSuperuser(t *testing.T) {
	av := newTestAviary(t)
	ctx := context.Background()

	if ok, _ := av.AuthenticateSuperuser(ctx, "admin@example.com", "password123"); ok {
		t.Fatal("expected auth to fail before superuser is set")
	}

	if err := av.SetSuperuser(ctx, "Admin@Example.com", "password123"); err != nil {
		t.Fatalf("SetSuperuser: %v", err)
	}

	su, err := av.GetSuperuser(ctx)
	if err != nil {
		t.Fatalf("GetSuperuser: %v", err)
	}
	if su.Email != "admin@example.com" { // normalized to lower-case
		t.Errorf("email = %q, want normalized lower-case", su.Email)
	}

	ok, err := av.AuthenticateSuperuser(ctx, "admin@example.com", "password123")
	if err != nil || !ok {
		t.Fatalf("AuthenticateSuperuser: ok=%v err=%v", ok, err)
	}
	if ok, _ := av.AuthenticateSuperuser(ctx, "admin@example.com", "wrong"); ok {
		t.Error("expected auth to fail with wrong password")
	}
}

func TestSetSuperuserValidation(t *testing.T) {
	av := newTestAviary(t)
	ctx := context.Background()
	if err := av.SetSuperuser(ctx, "", "password123"); err == nil {
		t.Error("expected error for empty email")
	}
	if err := av.SetSuperuser(ctx, "a@b.com", "short"); err == nil {
		t.Error("expected error for short password")
	}
}

// TestSuperuserPropagatedOnBoot verifies that a superuser configured before a
// project boots can immediately authenticate against that project's dashboard.
func TestSuperuserPropagatedOnBoot(t *testing.T) {
	av := newTestAviary(t, withDashboardPassword)
	ctx := context.Background()

	if err := av.SetSuperuser(ctx, "admin@example.com", "password123"); err != nil {
		t.Fatalf("SetSuperuser: %v", err)
	}
	if _, err := av.CreateProject(ctx, "alpha", "Alpha"); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	// First request boots the project and seeds the superuser.
	if code := authProjectSuperuser(t, av, "alpha", "admin@example.com", "password123"); code != http.StatusOK {
		t.Fatalf("superuser auth on alpha = %d, want 200", code)
	}
	// Wrong password must be rejected by the project.
	if code := authProjectSuperuser(t, av, "alpha", "admin@example.com", "nope"); code == http.StatusOK {
		t.Fatal("expected wrong password to be rejected by project")
	}
}

// TestSuperuserPropagatedLive verifies that changing the control-plane
// superuser propagates to an already-running project.
func TestSuperuserPropagatedLive(t *testing.T) {
	av := newTestAviary(t, withDashboardPassword)
	ctx := context.Background()

	if _, err := av.CreateProject(ctx, "alpha", "Alpha"); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	// Boot the project before any superuser exists.
	req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	req.Host = "alpha.localhost"
	av.ServeHTTP(httptest.NewRecorder(), req)

	// Now set the superuser; it must propagate to the running project.
	if err := av.SetSuperuser(ctx, "admin@example.com", "password123"); err != nil {
		t.Fatalf("SetSuperuser: %v", err)
	}
	if code := authProjectSuperuser(t, av, "alpha", "admin@example.com", "password123"); code != http.StatusOK {
		t.Fatalf("superuser auth after live propagation = %d, want 200", code)
	}
}

// TestSuperuserSharedAcrossProjects verifies the same credentials work on two
// independent projects.
func TestSuperuserSharedAcrossProjects(t *testing.T) {
	av := newTestAviary(t, withDashboardPassword)
	ctx := context.Background()

	if err := av.SetSuperuser(ctx, "admin@example.com", "password123"); err != nil {
		t.Fatalf("SetSuperuser: %v", err)
	}
	for _, id := range []string{"alpha", "beta"} {
		if _, err := av.CreateProject(ctx, id, ""); err != nil {
			t.Fatalf("CreateProject %q: %v", id, err)
		}
		if code := authProjectSuperuser(t, av, id, "admin@example.com", "password123"); code != http.StatusOK {
			t.Fatalf("superuser auth on %q = %d, want 200", id, code)
		}
	}
}

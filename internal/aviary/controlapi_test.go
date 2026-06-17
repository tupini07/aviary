package aviary

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// doControl issues a request to the control-plane handler (bare host).
func doControl(t *testing.T, av *Aviary, method, path string, body any, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Host = "localhost" // no project subdomain => control plane
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	av.ServeHTTP(rec, req)
	return rec
}

// loginAs configures a superuser (first-run bootstrap) and returns a valid
// session cookie for authenticated control-plane requests.
func loginAs(t *testing.T, av *Aviary, email, password string) *http.Cookie {
	t.Helper()
	// Bootstrap the first superuser (allowed unauthenticated).
	rec := doControl(t, av, http.MethodPut, "/api/superuser",
		putSuperuserRequest{Email: email, Password: password})
	if rec.Code != http.StatusOK {
		t.Fatalf("bootstrap superuser: status %d body %s", rec.Code, rec.Body.String())
	}
	rec = doControl(t, av, http.MethodPost, "/api/auth/login",
		loginRequest{Email: email, Password: password})
	if rec.Code != http.StatusOK {
		t.Fatalf("login: status %d body %s", rec.Code, rec.Body.String())
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			return c
		}
	}
	t.Fatal("login did not set a session cookie")
	return nil
}

func TestAPICreateListGetProject(t *testing.T) {
	av := newTestAviary(t)
	sess := loginAs(t, av, "admin@example.com", "password123")

	// initially empty
	rec := doControl(t, av, http.MethodGet, "/api/projects", nil, sess)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: status %d", rec.Code)
	}
	var list []Project
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("unmarshal list: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("expected empty list, got %d", len(list))
	}

	// create
	rec = doControl(t, av, http.MethodPost, "/api/projects", createProjectRequest{ID: "alpha", Name: "Alpha"}, sess)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: status %d body %s", rec.Code, rec.Body.String())
	}
	var created Project
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal created: %v", err)
	}
	if created.ID != "alpha" || created.Status != StatusActive {
		t.Fatalf("unexpected created project: %+v", created)
	}

	// get
	rec = doControl(t, av, http.MethodGet, "/api/projects/alpha", nil, sess)
	if rec.Code != http.StatusOK {
		t.Fatalf("get: status %d", rec.Code)
	}

	// duplicate => 409
	rec = doControl(t, av, http.MethodPost, "/api/projects", createProjectRequest{ID: "alpha"}, sess)
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate: status %d, want 409", rec.Code)
	}

	// invalid id => 400
	rec = doControl(t, av, http.MethodPost, "/api/projects", createProjectRequest{ID: "Bad ID"}, sess)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid id: status %d, want 400", rec.Code)
	}
}

func TestAPIGetMissing(t *testing.T) {
	av := newTestAviary(t)
	sess := loginAs(t, av, "admin@example.com", "password123")
	rec := doControl(t, av, http.MethodGet, "/api/projects/ghost", nil, sess)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("get missing: status %d, want 404", rec.Code)
	}
}

func TestAPIPatchStatus(t *testing.T) {
	av := newTestAviary(t)
	sess := loginAs(t, av, "admin@example.com", "password123")
	doControl(t, av, http.MethodPost, "/api/projects", createProjectRequest{ID: "alpha"}, sess)

	// disable
	rec := doControl(t, av, http.MethodPatch, "/api/projects/alpha", map[string]string{"status": "disabled"}, sess)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch: status %d body %s", rec.Code, rec.Body.String())
	}
	var p Project
	_ = json.Unmarshal(rec.Body.Bytes(), &p)
	if p.Status != StatusDisabled {
		t.Fatalf("status = %q, want disabled", p.Status)
	}

	// invalid status
	rec = doControl(t, av, http.MethodPatch, "/api/projects/alpha", map[string]string{"status": "bogus"}, sess)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid status: %d, want 400", rec.Code)
	}

	// empty patch (no fields) => 400
	rec = doControl(t, av, http.MethodPatch, "/api/projects/alpha", map[string]string{}, sess)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty patch: %d, want 400", rec.Code)
	}

	// patch missing
	rec = doControl(t, av, http.MethodPatch, "/api/projects/ghost", map[string]string{"status": "active"}, sess)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("patch missing: %d, want 404", rec.Code)
	}
}

func TestAPIPatchName(t *testing.T) {
	av := newTestAviary(t)
	sess := loginAs(t, av, "admin@example.com", "password123")
	doControl(t, av, http.MethodPost, "/api/projects", createProjectRequest{ID: "alpha", Name: "Old"}, sess)

	rec := doControl(t, av, http.MethodPatch, "/api/projects/alpha", map[string]string{"name": "New Name"}, sess)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch name: status %d body %s", rec.Code, rec.Body.String())
	}
	var p Project
	_ = json.Unmarshal(rec.Body.Bytes(), &p)
	if p.Name != "New Name" {
		t.Fatalf("name = %q, want %q", p.Name, "New Name")
	}
}

func TestAPIListIncludesRunning(t *testing.T) {
	av := newTestAviary(t)
	sess := loginAs(t, av, "admin@example.com", "password123")
	doControl(t, av, http.MethodPost, "/api/projects", createProjectRequest{ID: "alpha"}, sess)

	// Not booted yet => running:false.
	rec := doControl(t, av, http.MethodGet, "/api/projects", nil, sess)
	var views []map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &views)
	if len(views) != 1 || views[0]["running"] != false {
		t.Fatalf("expected running:false before boot, got %v", views)
	}

	// Boot it via a request, then it should report running:true.
	boot := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	boot.Host = "alpha.localhost"
	av.ServeHTTP(httptest.NewRecorder(), boot)

	rec = doControl(t, av, http.MethodGet, "/api/projects", nil, sess)
	views = nil
	_ = json.Unmarshal(rec.Body.Bytes(), &views)
	if len(views) != 1 || views[0]["running"] != true {
		t.Fatalf("expected running:true after boot, got %v", views)
	}
}

func TestAPIDeleteProject(t *testing.T) {
	av := newTestAviary(t)
	sess := loginAs(t, av, "admin@example.com", "password123")
	doControl(t, av, http.MethodPost, "/api/projects", createProjectRequest{ID: "alpha"}, sess)

	rec := doControl(t, av, http.MethodDelete, "/api/projects/alpha", nil, sess)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete: status %d", rec.Code)
	}
	rec = doControl(t, av, http.MethodGet, "/api/projects/alpha", nil, sess)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("get after delete: %d, want 404", rec.Code)
	}
	rec = doControl(t, av, http.MethodDelete, "/api/projects/alpha", nil, sess)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("delete missing: %d, want 404", rec.Code)
	}
}

func TestAPIRejectsUnknownFields(t *testing.T) {
	av := newTestAviary(t)
	sess := loginAs(t, av, "admin@example.com", "password123")
	rec := doControl(t, av, http.MethodPost, "/api/projects", map[string]any{"id": "alpha", "bogus": 1}, sess)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown field: status %d, want 400", rec.Code)
	}
}

func TestAPISuperuserGetPut(t *testing.T) {
	av := newTestAviary(t)

	// Before setup, the public session endpoint reports unconfigured.
	rec := doControl(t, av, http.MethodGet, "/api/auth/session", nil)
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got["configured"] != false || got["authenticated"] != false {
		t.Fatalf("pre-setup session = %v", got)
	}

	// Bootstrap the first superuser unauthenticated, then log in.
	sess := loginAs(t, av, "Admin@Example.com", "password123")

	// Authenticated GET returns the email, no password leaked.
	rec = doControl(t, av, http.MethodGet, "/api/superuser", nil, sess)
	if rec.Code != http.StatusOK {
		t.Fatalf("get superuser: status %d", rec.Code)
	}
	got = nil
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got["configured"] != true || got["email"] != "admin@example.com" {
		t.Fatalf("get superuser: %v", got)
	}
	if _, leaked := got["password"]; leaked {
		t.Fatal("password must not be exposed")
	}

	// Unauthenticated GET is rejected now that a superuser exists.
	rec = doControl(t, av, http.MethodGet, "/api/superuser", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauth get superuser: status %d, want 401", rec.Code)
	}

	// Unauthenticated PUT is rejected once configured.
	rec = doControl(t, av, http.MethodPut, "/api/superuser",
		putSuperuserRequest{Email: "other@example.com", Password: "password123"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauth put superuser: status %d, want 401", rec.Code)
	}
}

func TestAPISuperuserValidation(t *testing.T) {
	av := newTestAviary(t)

	// Bootstrap validation (no superuser yet, PUT allowed but values invalid).
	rec := doControl(t, av, http.MethodPut, "/api/superuser",
		putSuperuserRequest{Email: "", Password: "password123"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty email: status %d, want 400", rec.Code)
	}
	rec = doControl(t, av, http.MethodPut, "/api/superuser",
		putSuperuserRequest{Email: "a@b.com", Password: "short"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("short password: status %d, want 400", rec.Code)
	}
}

func TestAPIAuthLoginLogout(t *testing.T) {
	av := newTestAviary(t)
	sess := loginAs(t, av, "admin@example.com", "password123")

	// Authenticated session reports the email.
	rec := doControl(t, av, http.MethodGet, "/api/auth/session", nil, sess)
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got["authenticated"] != true || got["email"] != "admin@example.com" {
		t.Fatalf("session = %v", got)
	}

	// Wrong password is rejected.
	rec = doControl(t, av, http.MethodPost, "/api/auth/login",
		loginRequest{Email: "admin@example.com", Password: "nope"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad login: status %d, want 401", rec.Code)
	}

	// Tampered cookie is rejected.
	bad := &http.Cookie{Name: sessionCookie, Value: sess.Value + "x"}
	rec = doControl(t, av, http.MethodGet, "/api/projects", nil, bad)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("tampered cookie: status %d, want 401", rec.Code)
	}

	// Logout clears the cookie.
	rec = doControl(t, av, http.MethodPost, "/api/auth/logout", nil, sess)
	if rec.Code != http.StatusOK {
		t.Fatalf("logout: status %d", rec.Code)
	}
}

func TestAPIRequiresAuth(t *testing.T) {
	av := newTestAviary(t)
	// No superuser configured yet: still must not allow project access.
	rec := doControl(t, av, http.MethodGet, "/api/projects", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauth list: status %d, want 401", rec.Code)
	}
}

func TestControlUIServed(t *testing.T) {
	av := newTestAviary(t)
	rec := doControl(t, av, http.MethodGet, "/", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("landing: status %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Fatalf("landing content-type = %q", ct)
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("Aviary")) {
		t.Fatal("landing did not render the Aviary UI")
	}
}

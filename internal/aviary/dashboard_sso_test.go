package aviary

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestProjectHostFromControl(t *testing.T) {
	cases := []struct {
		name        string
		controlHost string
		id          string
		want        string
	}{
		{"bare localhost", "localhost", "p1", "p1.localhost"},
		{"localhost with port", "localhost:8090", "p1", "p1.localhost:8090"},
		{"ipv4 falls back to localhost", "127.0.0.1:8090", "p1", "p1.localhost:8090"},
		{"reserved console label", "aviary-console.apps.example.com", "p1", "p1.apps.example.com"},
		{"legacy console label", "_console.apps.example.com", "p1", "p1.apps.example.com"},
		{"reserved www label", "www.apps.example.com:443", "p1", "p1.apps.example.com:443"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := projectHostFromControl(tc.controlHost, tc.id); got != tc.want {
				t.Fatalf("projectHostFromControl(%q, %q) = %q, want %q", tc.controlHost, tc.id, got, tc.want)
			}
		})
	}
}

// TestDashboardPasswordLoginDisabledByDefault verifies that PocketBase's native
// superuser password login is rejected on projects unless explicitly allowed,
// removing the brute-force surface.
func TestDashboardPasswordLoginDisabledByDefault(t *testing.T) {
	av := newTestAviary(t)
	ctx := context.Background()

	if err := av.SetSuperuser(ctx, "admin@example.com", "password123"); err != nil {
		t.Fatalf("SetSuperuser: %v", err)
	}
	if _, err := av.CreateProject(ctx, "alpha", "Alpha"); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	if code := authProjectSuperuser(t, av, "alpha", "admin@example.com", "password123"); code != http.StatusForbidden {
		t.Fatalf("password login on alpha = %d, want 403 (disabled)", code)
	}
}

// superuserCookie returns a signed control-plane session cookie for a superuser.
func superuserCookie(av *Aviary, email string) *http.Cookie {
	return &http.Cookie{
		Name:  sessionCookie,
		Value: av.signSession(email, roleSuperuser, time.Now().Add(time.Hour)),
	}
}

func TestDashboardSSOFlow(t *testing.T) {
	av := newTestAviary(t)
	ctx := context.Background()

	if err := av.SetSuperuser(ctx, "admin@example.com", "password123"); err != nil {
		t.Fatalf("SetSuperuser: %v", err)
	}
	if _, err := av.CreateProject(ctx, "alpha", "Alpha"); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	// Step 1: control plane mints a ticket and redirects to the project host.
	req := httptest.NewRequest(http.MethodGet, "/api/projects/alpha/dashboard", nil)
	req.Host = "localhost:8090"
	req.AddCookie(superuserCookie(av, "admin@example.com"))
	rec := httptest.NewRecorder()
	av.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("dashboard SSO = %d, want 302 (body: %s)", rec.Code, rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("parse redirect %q: %v", loc, err)
	}
	if u.Host != "alpha.localhost:8090" || u.Path != ssoPath {
		t.Fatalf("redirect = %q, want host alpha.localhost:8090 path %s", loc, ssoPath)
	}
	ticket := u.Query().Get("ticket")
	if ticket == "" {
		t.Fatal("redirect missing ticket")
	}

	// Step 2: redeeming the ticket on the project host seeds the dashboard auth.
	ssoReq := httptest.NewRequest(http.MethodGet, ssoPath+"?ticket="+url.QueryEscape(ticket), nil)
	ssoReq.Host = "alpha.localhost:8090"
	ssoRec := httptest.NewRecorder()
	av.ServeHTTP(ssoRec, ssoReq)

	if ssoRec.Code != http.StatusOK {
		t.Fatalf("ticket redemption = %d, want 200 (body: %s)", ssoRec.Code, ssoRec.Body.String())
	}
	body := ssoRec.Body.String()
	if !strings.Contains(body, `__pb_superusers__/_`) {
		t.Fatalf("bootstrap page missing __pb_superusers__ seed: %s", body)
	}
	if !strings.Contains(body, "pocketbase_auth") {
		t.Fatalf("bootstrap page missing pocketbase_auth fallback seed: %s", body)
	}
	var dashSet bool
	for _, ck := range ssoRec.Result().Cookies() {
		if ck.Name == dashboardCookie && ck.Value != "" {
			dashSet = true
		}
	}
	if !dashSet {
		t.Fatal("bootstrap response did not set the dashboard cookie")
	}

	// Step 3: a ticket is single-use; a second redemption must be rejected.
	replay := httptest.NewRequest(http.MethodGet, ssoPath+"?ticket="+url.QueryEscape(ticket), nil)
	replay.Host = "alpha.localhost:8090"
	replayRec := httptest.NewRecorder()
	av.ServeHTTP(replayRec, replay)
	if replayRec.Code != http.StatusForbidden {
		t.Fatalf("ticket replay = %d, want 403", replayRec.Code)
	}
}

// TestDashboardSSORequiresAccess verifies a ticket minted for one project cannot
// be redeemed against another, and unauthenticated callers are rejected.
func TestDashboardSSORejectsForeignTicket(t *testing.T) {
	av := newTestAviary(t)
	ctx := context.Background()

	if err := av.SetSuperuser(ctx, "admin@example.com", "password123"); err != nil {
		t.Fatalf("SetSuperuser: %v", err)
	}
	for _, id := range []string{"alpha", "beta"} {
		if _, err := av.CreateProject(ctx, id, ""); err != nil {
			t.Fatalf("CreateProject %q: %v", id, err)
		}
	}

	ticket, err := av.signSSOTicket("admin@example.com", "alpha", time.Now().Add(ssoTicketTTL))
	if err != nil {
		t.Fatalf("signSSOTicket: %v", err)
	}

	// Redeem the alpha ticket against beta: must be rejected.
	req := httptest.NewRequest(http.MethodGet, ssoPath+"?ticket="+url.QueryEscape(ticket), nil)
	req.Host = "beta.localhost"
	rec := httptest.NewRecorder()
	av.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("foreign ticket = %d, want 403", rec.Code)
	}
}

func TestDashboardSSORequiresAuth(t *testing.T) {
	av := newTestAviary(t)
	ctx := context.Background()
	if _, err := av.CreateProject(ctx, "alpha", ""); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/projects/alpha/dashboard", nil)
	req.Host = "localhost"
	rec := httptest.NewRecorder()
	av.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated dashboard SSO = %d, want 401", rec.Code)
	}
}

// TestDashboardSSORedirectsBrowserToLogin verifies that an unauthenticated
// browser navigation (Accept: text/html) is sent to the control-plane login
// page instead of receiving a JSON 401, so the /_/ gate can bounce visitors here.
func TestDashboardSSORedirectsBrowserToLogin(t *testing.T) {
	av := newTestAviary(t)
	ctx := context.Background()
	if _, err := av.CreateProject(ctx, "alpha", ""); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/projects/alpha/dashboard", nil)
	req.Host = "aviary-console.localhost"
	req.Header.Set("Accept", "text/html")
	rec := httptest.NewRecorder()
	av.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/" {
		t.Fatalf("unauthenticated browser dashboard = %d loc %q, want 302 to /", rec.Code, rec.Header().Get("Location"))
	}
}

func TestControlHostFromProject(t *testing.T) {
	cases := []struct{ name, projectHost, want string }{
		{"bare localhost", "alpha.localhost", "aviary-console.localhost"},
		{"localhost with port", "alpha.localhost:8090", "aviary-console.localhost:8090"},
		{"real domain", "alpha.apps.example.com", "aviary-console.apps.example.com"},
		{"real domain with port", "alpha.apps.example.com:443", "aviary-console.apps.example.com:443"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := controlHostFromProject(tc.projectHost); got != tc.want {
				t.Fatalf("controlHostFromProject(%q) = %q, want %q", tc.projectHost, got, tc.want)
			}
		})
	}
}

func TestVerifyDashboardSession(t *testing.T) {
	av := newTestAviary(t)
	good := av.signDashboardSession("admin@example.com", "alpha", time.Now().Add(time.Hour))
	if !av.verifyDashboardSession(good, "alpha") {
		t.Fatal("valid dashboard session rejected")
	}
	if av.verifyDashboardSession(good, "beta") {
		t.Fatal("dashboard session accepted for the wrong project")
	}
	expired := av.signDashboardSession("admin@example.com", "alpha", time.Now().Add(-time.Minute))
	if av.verifyDashboardSession(expired, "alpha") {
		t.Fatal("expired dashboard session accepted")
	}
	if av.verifyDashboardSession(good+"x", "alpha") {
		t.Fatal("tampered dashboard session accepted")
	}
}

// TestDashboardUIGate verifies that the PocketBase admin UI (/_/) is bounced to
// the control-plane SSO flow when reached without a dashboard cookie, and served
// normally once a valid cookie is present.
func TestDashboardUIGate(t *testing.T) {
	av := newTestAviary(t)
	ctx := context.Background()
	if err := av.SetSuperuser(ctx, "admin@example.com", "password123"); err != nil {
		t.Fatalf("SetSuperuser: %v", err)
	}
	if _, err := av.CreateProject(ctx, "alpha", "Alpha"); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/_/", nil)
	req.Host = "alpha.localhost:8090"
	rec := httptest.NewRecorder()
	av.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("gated /_/ = %d, want 302 (body: %s)", rec.Code, rec.Body.String())
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse gate redirect: %v", err)
	}
	if loc.Host != "aviary-console.localhost:8090" || loc.Path != "/api/projects/alpha/dashboard" {
		t.Fatalf("gate redirect = %q, want aviary-console.localhost:8090/api/projects/alpha/dashboard", rec.Header().Get("Location"))
	}

	req2 := httptest.NewRequest(http.MethodGet, "/_/", nil)
	req2.Host = "alpha.localhost:8090"
	req2.AddCookie(&http.Cookie{
		Name:  dashboardCookie,
		Value: av.signDashboardSession("admin@example.com", "alpha", time.Now().Add(time.Hour)),
	})
	rec2 := httptest.NewRecorder()
	av.ServeHTTP(rec2, req2)
	if rec2.Code == http.StatusFound && strings.Contains(rec2.Header().Get("Location"), controlLabel) {
		t.Fatalf("authorized /_/ was bounced to the control plane: %q", rec2.Header().Get("Location"))
	}
}

// TestDashboardUIGateOpenWithPasswordLogin verifies that opting into PocketBase
// password login leaves the native dashboard login page accessible (ungated).
func TestDashboardUIGateOpenWithPasswordLogin(t *testing.T) {
	av := newTestAviary(t, withDashboardPassword)
	ctx := context.Background()
	if err := av.SetSuperuser(ctx, "admin@example.com", "password123"); err != nil {
		t.Fatalf("SetSuperuser: %v", err)
	}
	if _, err := av.CreateProject(ctx, "alpha", "Alpha"); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/_/", nil)
	req.Host = "alpha.localhost:8090"
	rec := httptest.NewRecorder()
	av.ServeHTTP(rec, req)
	if rec.Code == http.StatusFound && strings.Contains(rec.Header().Get("Location"), controlLabel) {
		t.Fatalf("password-login mode should not gate /_/, but bounced to %q", rec.Header().Get("Location"))
	}
}

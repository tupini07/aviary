package aviary

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/pocketbase/pocketbase/core"
)

// marshalDoc round-trips an OpenAPI doc through JSON into a generic map, which
// both exercises serializability and gives uniform access for assertions.
func marshalDoc(t *testing.T, doc oa) map[string]any {
	t.Helper()
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal openapi doc: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal openapi doc: %v", err)
	}
	return out
}

func paths(t *testing.T, doc map[string]any) map[string]any {
	t.Helper()
	p, ok := doc["paths"].(map[string]any)
	if !ok {
		t.Fatalf("doc has no paths object")
	}
	return p
}

func TestControlOpenAPI(t *testing.T) {
	doc := marshalDoc(t, controlOpenAPI("http://localhost:8090"))

	if doc["openapi"] != "3.1.0" {
		t.Fatalf("openapi = %v, want 3.1.0", doc["openapi"])
	}
	p := paths(t, doc)
	for _, want := range []string{
		"/api/openapi.json", "/api/auth/login", "/api/projects",
		"/api/projects/{id}", "/api/projects/{id}/dashboard", "/api/invitations",
	} {
		if _, ok := p[want]; !ok {
			t.Errorf("control spec missing path %q", want)
		}
	}

	// Public endpoints must override security with an empty array (no auth),
	// not an array containing an empty object — regression for invalid 3.1.
	login := p["/api/auth/login"].(map[string]any)["post"].(map[string]any)
	sec, ok := login["security"].([]any)
	if !ok || len(sec) != 0 {
		t.Errorf("login security = %#v, want empty array []", login["security"])
	}

	schemas := doc["components"].(map[string]any)["schemas"].(map[string]any)
	if _, ok := schemas["Project"]; !ok {
		t.Errorf("control spec missing Project schema")
	}
}

func TestControlOpenAPIEndpoint(t *testing.T) {
	av := newTestAviary(t)
	rec := doControl(t, av, http.MethodGet, "/api/openapi.json", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/openapi.json = %d, want 200", rec.Code)
	}
	var doc map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if doc["openapi"] != "3.1.0" {
		t.Fatalf("served openapi = %v, want 3.1.0", doc["openapi"])
	}
}

func TestProjectOpenAPI(t *testing.T) {
	av := newTestAviary(t)
	ctx := context.Background()
	if err := av.SetSuperuser(ctx, "admin@example.com", "password123"); err != nil {
		t.Fatalf("SetSuperuser: %v", err)
	}
	if _, err := av.CreateProject(ctx, "demo", "Demo"); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	app := bootProject(t, av, "demo")

	doc := marshalDoc(t, mustProjectOpenAPI(t, app))
	if doc["openapi"] != "3.1.0" {
		t.Fatalf("openapi = %v, want 3.1.0", doc["openapi"])
	}

	p := paths(t, doc)
	for _, want := range []string{
		"/api/collections/users/records",
		"/api/collections/users/records/{id}",
		"/api/collections/users/auth-with-password",
		"/api/aviary/passkey/login/begin",
	} {
		if _, ok := p[want]; !ok {
			t.Errorf("project spec missing path %q", want)
		}
	}

	// Internal/system collections (including Aviary's _passkeys store) must not
	// leak into the public API description.
	for path := range p {
		if strings.Contains(path, "_passkeys") || strings.Contains(path, "_superusers") {
			t.Errorf("project spec exposes internal collection path %q", path)
		}
	}

	// The users record schema must expose visible fields but never hidden ones.
	schemas := doc["components"].(map[string]any)["schemas"].(map[string]any)
	rec, ok := schemas["Record_users"].(map[string]any)
	if !ok {
		t.Fatalf("missing Record_users schema")
	}
	props := rec["properties"].(map[string]any)
	if _, ok := props["email"]; !ok {
		t.Errorf("Record_users missing visible field 'email'")
	}
	for _, hidden := range []string{"password", "tokenKey"} {
		if _, ok := props[hidden]; ok {
			t.Errorf("Record_users exposes hidden field %q", hidden)
		}
	}
}

func TestProjectOpenAPIEndpoint(t *testing.T) {
	av := newTestAviary(t)
	ctx := context.Background()
	if err := av.SetSuperuser(ctx, "admin@example.com", "password123"); err != nil {
		t.Fatalf("SetSuperuser: %v", err)
	}
	if _, err := av.CreateProject(ctx, "demo", "Demo"); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	rec := doProject(t, av, "demo", http.MethodGet, openapiPath, "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200", openapiPath, rec.Code)
	}
	var doc map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if doc["info"].(map[string]any)["title"] != "Aviary project: demo" {
		t.Fatalf("unexpected title %v", doc["info"].(map[string]any)["title"])
	}
}

func mustProjectOpenAPI(t *testing.T, app core.App) oa {
	t.Helper()
	doc, err := projectOpenAPI(app, "http://demo.localhost", "demo")
	if err != nil {
		t.Fatalf("projectOpenAPI: %v", err)
	}
	return doc
}

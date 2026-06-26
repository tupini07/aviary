package aviary

import (
	"net/http"
	"strings"
	"testing"
)

func TestLlmsTxtEndpoint(t *testing.T) {
	av := newTestAviary(t)
	rec := doControl(t, av, http.MethodGet, "/llms.txt", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /llms.txt = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("Content-Type = %q, want text/plain", ct)
	}
	body := rec.Body.String()
	// The index must lead with the H1 and link to canonical sources rather than
	// restating them. The OpenAPI link is resolved against the request host.
	for _, want := range []string{
		"# Aviary",
		"http://localhost/api/openapi.json",
		"github.com/tupini07/aviary",
		"pocketbase.io/docs/js-overview",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("/llms.txt missing %q\n---\n%s", want, body)
		}
	}
}

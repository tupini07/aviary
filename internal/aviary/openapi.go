package aviary

import (
	"encoding/json"
	"net/http"
)

// oa is a terse alias for the free-form maps used to assemble OpenAPI documents.
type oa = map[string]any

// openapiVersion is the version reported in the generated specs' info block. It
// tracks the shape of the documented API surface, not the Aviary build.
const openapiVersion = "1.0.0"

// openapiPath is the project-subdomain path that serves the on-the-fly OpenAPI
// description of that project's PocketBase API. It is namespaced under
// /__aviary/ so it can never collide with a user-defined collection route.
const openapiPath = "/__aviary/openapi.json"

// requestBaseURL reconstructs the absolute origin (scheme + host) the caller
// used to reach Aviary, so generated specs advertise a working server URL.
func requestBaseURL(r *http.Request) string {
	scheme := "http"
	if isHTTPS(r) {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// writeOpenAPI serializes an OpenAPI document as indented JSON. The output is
// deterministic (Go sorts map keys), which keeps it cache- and diff-friendly.
func writeOpenAPI(w http.ResponseWriter, doc oa) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(doc)
}

// jsonBody wraps a schema as an application/json request/response content block.
func jsonBody(schema oa) oa {
	return oa{"content": oa{"application/json": oa{"schema": schema}}}
}

// ref returns a JSON Schema reference to a named component.
func ref(name string) oa {
	return oa{"$ref": "#/components/schemas/" + name}
}

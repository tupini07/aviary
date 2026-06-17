package aviary

import (
	"net/http"
	"strings"

	"github.com/pocketbase/pocketbase/core"
)

// handleProjectOpenAPI serves an OpenAPI 3.1 description of a single project's
// PocketBase API, generated on the fly from that project's current collection
// schema. It is reachable at openapiPath on the project's subdomain and bypasses
// PocketBase auth so clients (and agents) can discover the API up front.
func (a *Aviary) handleProjectOpenAPI(w http.ResponseWriter, r *http.Request, id string, c *cage) {
	doc, err := projectOpenAPI(c.app, requestBaseURL(r), id)
	if err != nil {
		http.Error(w, "failed to generate OpenAPI: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeOpenAPI(w, doc)
}

// projectOpenAPI builds the OpenAPI document for a project by reflecting over
// its non-system collections. Each collection contributes a record schema plus
// the records CRUD paths; auth collections additionally contribute the standard
// authentication endpoints.
func projectOpenAPI(app core.App, serverURL, projectID string) (oa, error) {
	cols, err := app.FindAllCollections()
	if err != nil {
		return nil, err
	}

	paths := oa{
		openapiPath: oa{
			"get": oa{
				"tags": []any{"meta"}, "summary": "This OpenAPI document",
				"responses": oa{"200": oa{"description": "OpenAPI 3.1 document"}},
			},
		},
	}
	schemas := oa{
		"Error": oa{
			"type": "object",
			"properties": oa{
				"status":  oa{"type": "integer"},
				"message": oa{"type": "string"},
				"data":    oa{"type": "object"},
			},
		},
		"AuthResponse": oa{
			"type": "object",
			"properties": oa{
				"token":  oa{"type": "string"},
				"record": oa{"type": "object"},
			},
		},
	}

	hasUsersAuth := false
	for _, col := range cols {
		// Skip PocketBase system collections and any "_"-prefixed collection
		// (a reserved namespace that includes Aviary's API-locked _passkeys
		// store) — these are internal infrastructure, not the public API.
		if col.System || strings.HasPrefix(col.Name, "_") {
			continue
		}
		recName := "Record_" + col.Name
		schemas[recName] = recordResponseSchema(col)
		listName := "List_" + col.Name
		schemas[listName] = oa{
			"type": "object",
			"properties": oa{
				"page":       oa{"type": "integer"},
				"perPage":    oa{"type": "integer"},
				"totalItems": oa{"type": "integer"},
				"totalPages": oa{"type": "integer"},
				"items":      oa{"type": "array", "items": ref(recName)},
			},
		}

		addRecordPaths(paths, col, recName, listName)

		if col.Type == core.CollectionTypeAuth {
			reqName := "Write_" + col.Name
			schemas[reqName] = recordWriteSchema(col)
			addAuthPaths(paths, col)
			if col.Name == "users" {
				hasUsersAuth = true
			}
		} else if col.Type == core.CollectionTypeBase {
			reqName := "Write_" + col.Name
			schemas[reqName] = recordWriteSchema(col)
		}
	}

	if hasUsersAuth {
		addPasskeyPaths(paths)
	}

	return oa{
		"openapi": "3.1.0",
		"info": oa{
			"title":       "Aviary project: " + projectID,
			"version":     openapiVersion,
			"description": "Generated description of the PocketBase records and auth API for project \"" + projectID + "\". Covers the records CRUD and authentication endpoints derived from this project's current collections. Realtime, batch, file-token and OAuth2 endpoints are part of PocketBase but omitted here; see https://pocketbase.io/docs/ for the full reference.",
		},
		"servers": []any{oa{"url": serverURL}},
		"tags": []any{
			oa{"name": "records", "description": "Collection records CRUD"},
			oa{"name": "auth", "description": "Auth collection sign-in"},
			oa{"name": "passkey", "description": "Aviary WebAuthn passkeys"},
			oa{"name": "meta", "description": "Discovery"},
		},
		"security": []any{oa{"bearerAuth": []any{}}},
		"components": oa{
			"securitySchemes": oa{
				"bearerAuth": oa{
					"type": "http", "scheme": "bearer",
					"description": "PocketBase auth token from an auth-with-password (or passkey) response, sent as Authorization: Bearer <token>.",
				},
			},
			"schemas": schemas,
		},
		"paths": paths,
	}, nil
}

// listQueryParams are the standard PocketBase list/search query parameters.
func listQueryParams() []any {
	p := func(name, typ, desc string) oa {
		return oa{"name": name, "in": "query", "description": desc, "schema": oa{"type": typ}}
	}
	return []any{
		p("page", "integer", "Page number (default 1)."),
		p("perPage", "integer", "Items per page (default 30, max 500)."),
		p("sort", "string", "Comma-separated sort expression, e.g. \"-created,name\"."),
		p("filter", "string", "Filter expression, e.g. \"status='active' && created>'2024-01-01'\"."),
		p("expand", "string", "Comma-separated relations to expand."),
		p("fields", "string", "Comma-separated subset of fields to return."),
		p("skipTotal", "boolean", "Skip counting totalItems/totalPages for speed."),
	}
}

// addRecordPaths registers the records CRUD endpoints for a collection. View
// collections are read-only; base and auth collections are fully writable.
func addRecordPaths(paths oa, col *core.Collection, recName, listName string) {
	base := "/api/collections/" + col.Name + "/records"
	writable := col.Type != core.CollectionTypeView
	reqRef := ref("Write_" + col.Name)
	idParam := oa{"name": "id", "in": "path", "required": true, "description": "Record id.", "schema": oa{"type": "string"}}
	notFound := oa{"description": "Record not found", "content": jsonBody(ref("Error"))["content"]}

	collOps := oa{
		"get": oa{
			"tags": []any{"records"}, "summary": "List/search " + col.Name + " records",
			"parameters": listQueryParams(),
			"responses":  oa{"200": oa{"description": "Paginated list", "content": jsonBody(ref(listName))["content"]}},
		},
	}
	if writable {
		collOps["post"] = oa{
			"tags": []any{"records"}, "summary": "Create a " + col.Name + " record",
			"requestBody": jsonBody(reqRef),
			"responses":   oa{"200": oa{"description": "Created record", "content": jsonBody(ref(recName))["content"]}, "400": oa{"description": "Validation error", "content": jsonBody(ref("Error"))["content"]}},
		}
	}
	paths[base] = collOps

	itemOps := oa{
		"get": oa{
			"tags": []any{"records"}, "summary": "View a " + col.Name + " record",
			"parameters": []any{idParam, oa{"name": "expand", "in": "query", "schema": oa{"type": "string"}}},
			"responses":  oa{"200": oa{"description": "Record", "content": jsonBody(ref(recName))["content"]}, "404": notFound},
		},
	}
	if writable {
		itemOps["patch"] = oa{
			"tags": []any{"records"}, "summary": "Update a " + col.Name + " record",
			"parameters":  []any{idParam},
			"requestBody": jsonBody(reqRef),
			"responses":   oa{"200": oa{"description": "Updated record", "content": jsonBody(ref(recName))["content"]}, "404": notFound},
		}
		itemOps["delete"] = oa{
			"tags": []any{"records"}, "summary": "Delete a " + col.Name + " record",
			"parameters": []any{idParam},
			"responses":  oa{"204": oa{"description": "Deleted"}, "404": notFound},
		}
	}
	paths[base+"/{id}"] = itemOps
}

// addAuthPaths registers the authentication endpoints for an auth collection.
func addAuthPaths(paths oa, col *core.Collection) {
	base := "/api/collections/" + col.Name
	paths[base+"/auth-with-password"] = oa{
		"post": oa{
			"tags": []any{"auth"}, "summary": "Authenticate a " + col.Name + " record with identity + password",
			"security": []any{},
			"requestBody": jsonBody(oa{
				"type": "object",
				"properties": oa{
					"identity": oa{"type": "string", "description": "Email or other configured identity field."},
					"password": oa{"type": "string"},
				},
				"required": []any{"identity", "password"},
			}),
			"responses": oa{"200": oa{"description": "Auth token + record", "content": jsonBody(ref("AuthResponse"))["content"]}, "400": oa{"description": "Failed to authenticate", "content": jsonBody(ref("Error"))["content"]}},
		},
	}
	paths[base+"/auth-refresh"] = oa{
		"post": oa{
			"tags": []any{"auth"}, "summary": "Refresh the auth token for the current " + col.Name + " record",
			"responses": oa{"200": oa{"description": "New auth token + record", "content": jsonBody(ref("AuthResponse"))["content"]}},
		},
	}
	paths[base+"/request-password-reset"] = oa{
		"post": oa{
			"tags": []any{"auth"}, "summary": "Send a password-reset email to a " + col.Name + " record",
			"security":    []any{},
			"requestBody": jsonBody(oa{"type": "object", "properties": oa{"email": oa{"type": "string", "format": "email"}}, "required": []any{"email"}}),
			"responses":   oa{"204": oa{"description": "Reset email sent (if the record exists)"}},
		},
	}
}

// addPasskeyPaths registers the Aviary-added WebAuthn endpoints, which the
// passkey extension mounts for the "users" collection on every project.
func addPasskeyPaths(paths oa) {
	pub := []any{}
	paths["/api/aviary/passkey/register/begin"] = oa{"post": oa{"tags": []any{"passkey"}, "summary": "Begin registering a passkey for the current user", "responses": oa{"200": oa{"description": "WebAuthn creation options + ceremony token"}}}}
	paths["/api/aviary/passkey/register/finish"] = oa{"post": oa{"tags": []any{"passkey"}, "summary": "Complete a passkey registration", "responses": oa{"200": oa{"description": "Passkey registered"}}}}
	paths["/api/aviary/passkey/login/begin"] = oa{"post": oa{"tags": []any{"passkey"}, "summary": "Begin a passwordless passkey sign-in", "security": pub, "responses": oa{"200": oa{"description": "WebAuthn assertion options + ceremony token"}}}}
	paths["/api/aviary/passkey/login/finish"] = oa{"post": oa{"tags": []any{"passkey"}, "summary": "Complete a passkey sign-in", "security": pub, "responses": oa{"200": oa{"description": "Auth token + record", "content": jsonBody(ref("AuthResponse"))["content"]}}}}
	paths["/api/aviary/passkey"] = oa{"get": oa{"tags": []any{"passkey"}, "summary": "List the current user's passkeys", "responses": oa{"200": oa{"description": "Registered passkeys"}}}}
	paths["/api/aviary/passkey/{id}"] = oa{"delete": oa{"tags": []any{"passkey"}, "summary": "Delete one of the current user's passkeys", "parameters": []any{oa{"name": "id", "in": "path", "required": true, "schema": oa{"type": "string"}}}, "responses": oa{"204": oa{"description": "Deleted"}}}}
}

// recordResponseSchema builds the JSON Schema of a record as returned by the
// API: the always-present id plus collection metadata and every visible field.
func recordResponseSchema(col *core.Collection) oa {
	props := oa{
		"collectionId":   oa{"type": "string"},
		"collectionName": oa{"type": "string"},
	}
	for _, f := range col.Fields {
		if f.GetHidden() {
			continue // hidden fields (e.g. password, tokenKey) are never serialized
		}
		props[f.GetName()] = fieldSchema(f)
	}
	return oa{"type": "object", "properties": props}
}

// recordWriteSchema builds the JSON Schema for create/update request bodies:
// the writable (non-system, non-hidden) fields, plus password fields for auth
// collections. PATCH accepts any subset of these.
func recordWriteSchema(col *core.Collection) oa {
	props := oa{}
	for _, f := range col.Fields {
		if f.GetSystem() || f.GetHidden() {
			continue
		}
		props[f.GetName()] = fieldSchema(f)
	}
	if col.Type == core.CollectionTypeAuth {
		props["password"] = oa{"type": "string", "writeOnly": true}
		props["passwordConfirm"] = oa{"type": "string", "writeOnly": true}
	}
	return oa{"type": "object", "properties": props}
}

// fieldSchema maps a PocketBase field to a JSON Schema fragment. It relies only
// on the stable Field interface (type + name), so it is resilient across field
// implementations: multi-value-capable kinds are described as string-or-array.
func fieldSchema(f core.Field) oa {
	stringOrArray := oa{"oneOf": []any{
		oa{"type": "string"},
		oa{"type": "array", "items": oa{"type": "string"}},
	}}
	switch f.Type() {
	case "number":
		return oa{"type": "number"}
	case "bool":
		return oa{"type": "boolean"}
	case "date", "autodate":
		return oa{"type": "string", "format": "date-time"}
	case "email":
		return oa{"type": "string", "format": "email"}
	case "url":
		return oa{"type": "string", "format": "uri"}
	case "json":
		return oa{"description": "Arbitrary JSON value."}
	case "geoPoint":
		return oa{"type": "object", "properties": oa{"lon": oa{"type": "number"}, "lat": oa{"type": "number"}}}
	case "select", "relation", "file":
		return stringOrArray
	default:
		return oa{"type": "string"}
	}
}

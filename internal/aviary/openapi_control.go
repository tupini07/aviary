package aviary

import "net/http"

// apiControlOpenAPI serves an OpenAPI 3.1 description of the Aviary
// control-plane API (project management, auth, collaborators, invitations).
// The control-plane surface is a small, fixed set of routes, so the document is
// authored by hand here and kept in lockstep with controlHandler.
func (a *Aviary) apiControlOpenAPI(w http.ResponseWriter, r *http.Request) {
	writeOpenAPI(w, controlOpenAPI(requestBaseURL(r)))
}

// controlOpenAPI builds the control-plane OpenAPI document for the given server
// origin.
func controlOpenAPI(serverURL string) oa {
	// cookieAuth secures most endpoints; public ones override security to [].
	public := []any{}

	errResp := func(desc string) oa {
		return oa{"description": desc, "content": jsonBody(ref("Error"))["content"]}
	}
	idParam := oa{
		"name": "id", "in": "path", "required": true,
		"description": "Project id (its subdomain label).",
		"schema":      oa{"type": "string"},
	}
	emailParam := oa{
		"name": "email", "in": "path", "required": true,
		"description": "Collaborator email address.",
		"schema":      oa{"type": "string", "format": "email"},
	}
	filePathParam := oa{
		"name": "path", "in": "query", "required": true,
		"description": "File path relative to pb_public, forward-slash separated (e.g. index.html or css/app.css).",
		"schema":      oa{"type": "string"},
	}

	paths := oa{
		"/api/openapi.json": oa{
			"get": oa{
				"tags": []any{"meta"}, "summary": "This OpenAPI document",
				"security":  public,
				"responses": oa{"200": oa{"description": "OpenAPI 3.1 document"}},
			},
		},
		"/api/auth/login": oa{
			"post": oa{
				"tags": []any{"auth"}, "summary": "Sign in with email and password",
				"security":    public,
				"requestBody": jsonBody(ref("Credentials")),
				"responses": oa{
					"200": oa{"description": "Session established (sets the aviary_session cookie)", "content": jsonBody(ref("Session"))["content"]},
					"401": errResp("Invalid credentials"),
				},
			},
		},
		"/api/auth/logout": oa{
			"post": oa{
				"tags": []any{"auth"}, "summary": "Sign out and clear the session cookie",
				"responses": oa{"204": oa{"description": "Signed out"}},
			},
		},
		"/api/auth/session": oa{
			"get": oa{
				"tags": []any{"auth"}, "summary": "Describe the current session",
				"security":  public,
				"responses": oa{"200": oa{"description": "Current session state", "content": jsonBody(ref("Session"))["content"]}},
			},
		},
		"/api/auth/passkey/login/begin": oa{
			"post": oa{
				"tags": []any{"auth", "passkey"}, "summary": "Begin a passwordless passkey sign-in",
				"security":  public,
				"responses": oa{"200": oa{"description": "WebAuthn assertion options + ceremony token"}},
			},
		},
		"/api/auth/passkey/login/finish": oa{
			"post": oa{
				"tags": []any{"auth", "passkey"}, "summary": "Complete a passkey sign-in",
				"security":  public,
				"responses": oa{"200": oa{"description": "Session established", "content": jsonBody(ref("Session"))["content"]}, "401": errResp("Assertion rejected")},
			},
		},
		"/api/auth/passkey/register/begin": oa{
			"post": oa{
				"tags": []any{"passkey"}, "summary": "Begin registering a superuser passkey",
				"responses": oa{"200": oa{"description": "WebAuthn creation options + ceremony token"}, "401": errResp("Authentication required")},
			},
		},
		"/api/auth/passkey/register/finish": oa{
			"post": oa{
				"tags": []any{"passkey"}, "summary": "Complete a superuser passkey registration",
				"responses": oa{"200": oa{"description": "Passkey registered"}, "401": errResp("Authentication required")},
			},
		},
		"/api/auth/passkey": oa{
			"get": oa{
				"tags": []any{"passkey"}, "summary": "List the superuser's registered passkeys",
				"responses": oa{"200": oa{"description": "Registered passkeys", "content": jsonBody(oa{"type": "array", "items": ref("Passkey")})["content"]}, "401": errResp("Authentication required")},
			},
		},
		"/api/auth/passkey/{id}": oa{
			"delete": oa{
				"tags": []any{"passkey"}, "summary": "Delete a superuser passkey",
				"parameters": []any{oa{"name": "id", "in": "path", "required": true, "description": "Credential id of the passkey.", "schema": oa{"type": "string"}}},
				"responses":  oa{"204": oa{"description": "Deleted"}, "401": errResp("Authentication required")},
			},
		},
		"/api/projects": oa{
			"get": oa{
				"tags": []any{"projects"}, "summary": "List accessible projects",
				"description": "Superusers see all projects; collaborators see only projects they have been granted.",
				"responses":   oa{"200": oa{"description": "Projects with live runtime state", "content": jsonBody(oa{"type": "array", "items": ref("ProjectView")})["content"]}, "401": errResp("Authentication required")},
			},
			"post": oa{
				"tags": []any{"projects"}, "summary": "Create a project (superuser only)",
				"requestBody": jsonBody(ref("CreateProject")),
				"responses": oa{
					"201": oa{"description": "Project created", "content": jsonBody(ref("Project"))["content"]},
					"400": errResp("Invalid project id"),
					"409": errResp("Project already exists"),
				},
			},
		},
		"/api/projects/{id}": oa{
			"get": oa{
				"tags": []any{"projects"}, "summary": "Get a single project",
				"parameters": []any{idParam},
				"responses":  oa{"200": oa{"description": "Project", "content": jsonBody(ref("Project"))["content"]}, "404": errResp("Project not found")},
			},
			"patch": oa{
				"tags": []any{"projects"}, "summary": "Update a project's name and/or status (superuser only)",
				"parameters":  []any{idParam},
				"requestBody": jsonBody(ref("PatchProject")),
				"responses":   oa{"200": oa{"description": "Updated project", "content": jsonBody(ref("ProjectView"))["content"]}, "400": errResp("Nothing to update or invalid status"), "404": errResp("Project not found")},
			},
			"delete": oa{
				"tags": []any{"projects"}, "summary": "Delete a project and its data (superuser only)",
				"parameters": []any{idParam},
				"responses":  oa{"204": oa{"description": "Deleted"}, "404": errResp("Project not found")},
			},
		},
		"/api/projects/{id}/dashboard": oa{
			"get": oa{
				"tags": []any{"projects"}, "summary": "One-click dashboard SSO redirect",
				"description": "Mints a single-use ticket and 302-redirects into the project's PocketBase admin dashboard, already logged in.",
				"parameters":  []any{idParam},
				"responses":   oa{"302": oa{"description": "Redirect to the project SSO handoff"}, "401": errResp("Authentication required"), "403": errResp("No dashboard access to this project")},
			},
		},
		"/api/projects/{id}/files": oa{
			"get": oa{
				"tags": []any{"files"}, "summary": "List static files served from the project's pb_public directory",
				"description": "Superusers may list any project's files; collaborators only projects they have been granted.",
				"parameters":  []any{idParam},
				"responses":   oa{"200": oa{"description": "Flat, sorted file listing", "content": jsonBody(oa{"type": "array", "items": ref("FileEntry")})["content"]}, "401": errResp("Authentication required"), "403": errResp("No access to this project"), "404": errResp("Project not found")},
			},
		},
		"/api/projects/{id}/files/content": oa{
			"get": oa{
				"tags": []any{"files"}, "summary": "Read a single pb_public file",
				"parameters": []any{idParam, filePathParam},
				"responses":  oa{"200": oa{"description": "File content", "content": jsonBody(ref("FileContent"))["content"]}, "400": errResp("Invalid path"), "401": errResp("Authentication required"), "403": errResp("No access to this project"), "404": errResp("File not found"), "413": errResp("File is too large to edit")},
			},
			"put": oa{
				"tags": []any{"files"}, "summary": "Create or overwrite a pb_public file",
				"description": "Creates any missing parent directories. Files are served live, so changes are visible immediately at the project URL.",
				"parameters":  []any{idParam},
				"requestBody": jsonBody(ref("FileContent")),
				"responses":   oa{"200": oa{"description": "File written", "content": jsonBody(ref("FileEntry"))["content"]}, "400": errResp("Invalid path"), "401": errResp("Authentication required"), "403": errResp("No access to this project"), "404": errResp("Project not found"), "413": errResp("Content is too large")},
			},
			"delete": oa{
				"tags": []any{"files"}, "summary": "Delete a single pb_public file",
				"parameters": []any{idParam, filePathParam},
				"responses":  oa{"204": oa{"description": "Deleted"}, "400": errResp("Invalid path"), "401": errResp("Authentication required"), "403": errResp("No access to this project"), "404": errResp("File not found")},
			},
		},
		"/api/superuser": oa{
			"get": oa{
				"tags": []any{"superuser"}, "summary": "Describe the platform superuser (superuser only)",
				"responses": oa{"200": oa{"description": "Superuser info"}},
			},
			"put": oa{
				"tags": []any{"superuser"}, "summary": "Create or update the platform superuser",
				"description": "Propagates the credentials to every project. Allowed unauthenticated only during first-run setup (when no superuser exists yet).",
				"requestBody": jsonBody(ref("Credentials")),
				"responses":   oa{"200": oa{"description": "Superuser saved"}, "400": errResp("Validation error"), "401": errResp("Authentication required")},
			},
		},
		"/api/collaborators": oa{
			"get": oa{
				"tags": []any{"collaborators"}, "summary": "List collaborators (superuser only)",
				"responses": oa{"200": oa{"description": "Collaborators", "content": jsonBody(oa{"type": "array", "items": ref("Collaborator")})["content"]}},
			},
		},
		"/api/collaborators/{email}": oa{
			"delete": oa{
				"tags": []any{"collaborators"}, "summary": "Remove a collaborator entirely (superuser only)",
				"parameters": []any{emailParam},
				"responses":  oa{"204": oa{"description": "Removed"}},
			},
		},
		"/api/collaborators/{email}/projects/{id}": oa{
			"delete": oa{
				"tags": []any{"collaborators"}, "summary": "Revoke a collaborator's access to one project (superuser only)",
				"parameters": []any{emailParam, idParam},
				"responses":  oa{"204": oa{"description": "Revoked"}},
			},
		},
		"/api/invitations": oa{
			"get": oa{
				"tags": []any{"invitations"}, "summary": "List pending invitations (superuser only)",
				"responses": oa{"200": oa{"description": "Pending invitations", "content": jsonBody(oa{"type": "array", "items": ref("Invitation")})["content"]}},
			},
			"post": oa{
				"tags": []any{"invitations"}, "summary": "Invite a collaborator to a project (superuser only)",
				"requestBody": jsonBody(ref("CreateInvitation")),
				"responses":   oa{"200": oa{"description": "Invitation created (returns an accept link)"}},
			},
		},
		"/api/invitations/accept": oa{
			"post": oa{
				"tags": []any{"invitations"}, "summary": "Accept an invitation",
				"security":    public,
				"requestBody": jsonBody(ref("AcceptInvitation")),
				"responses":   oa{"200": oa{"description": "Invitation accepted"}, "400": errResp("Invalid or expired token")},
			},
		},
	}

	return oa{
		"openapi": "3.1.0",
		"info": oa{
			"title":       "Aviary Control Plane API",
			"version":     openapiVersion,
			"description": "Management API for the Aviary multi-tenant PocketBase host: projects (cages), the platform superuser, collaborators, invitations, and dashboard SSO. Each project additionally exposes its own PocketBase API at https://<project-id>.<host>/ — fetch /__aviary/openapi.json on that subdomain for a generated description of it.",
		},
		"servers": []any{oa{"url": serverURL}},
		"tags": []any{
			oa{"name": "auth", "description": "Session authentication"},
			oa{"name": "passkey", "description": "WebAuthn passkeys"},
			oa{"name": "projects", "description": "Project (cage) lifecycle"},
			oa{"name": "files", "description": "Per-project static files (pb_public)"},
			oa{"name": "superuser", "description": "Platform superuser"},
			oa{"name": "collaborators", "description": "Per-project collaborators"},
			oa{"name": "invitations", "description": "Collaborator invitations"},
			oa{"name": "meta", "description": "Discovery"},
		},
		"security": []any{oa{"cookieAuth": []any{}}},
		"paths":    paths,
		"components": oa{
			"securitySchemes": oa{
				"cookieAuth": oa{
					"type": "apiKey", "in": "cookie", "name": sessionCookie,
					"description": "Session cookie set by POST /api/auth/login or a passkey sign-in.",
				},
			},
			"schemas": oa{
				"Error": oa{
					"type": "object",
					"properties": oa{
						"error": oa{"type": "string"},
						"code":  oa{"type": "integer"},
					},
					"required": []any{"error", "code"},
				},
				"Credentials": oa{
					"type": "object",
					"properties": oa{
						"email":    oa{"type": "string", "format": "email"},
						"password": oa{"type": "string"},
					},
					"required": []any{"email", "password"},
				},
				"Session": oa{
					"type": "object",
					"properties": oa{
						"authenticated": oa{"type": "boolean"},
						"configured":    oa{"type": "boolean", "description": "Whether a superuser exists yet."},
						"email":         oa{"type": "string"},
						"role":          oa{"type": "string", "enum": []any{"superuser", "collaborator"}},
					},
				},
				"Project": oa{
					"type": "object",
					"properties": oa{
						"id":      oa{"type": "string"},
						"name":    oa{"type": "string"},
						"status":  oa{"type": "string", "enum": []any{"active", "disabled"}},
						"created": oa{"type": "string", "format": "date-time"},
						"updated": oa{"type": "string", "format": "date-time"},
					},
				},
				"ProjectView": oa{
					"allOf": []any{
						ref("Project"),
						oa{"type": "object", "properties": oa{"running": oa{"type": "boolean", "description": "Whether the cage is currently booted."}}},
					},
				},
				"CreateProject": oa{
					"type": "object",
					"properties": oa{
						"id":   oa{"type": "string", "description": "Subdomain-safe id."},
						"name": oa{"type": "string"},
					},
					"required": []any{"id"},
				},
				"PatchProject": oa{
					"type": "object",
					"properties": oa{
						"name":   oa{"type": "string"},
						"status": oa{"type": "string", "enum": []any{"active", "disabled"}},
					},
				},
				"Passkey": oa{
					"type": "object",
					"properties": oa{
						"credentialId": oa{"type": "string"},
						"label":        oa{"type": "string"},
						"created":      oa{"type": "string", "format": "date-time"},
					},
				},
				"Collaborator": oa{
					"type": "object",
					"properties": oa{
						"email":    oa{"type": "string", "format": "email"},
						"projects": oa{"type": "array", "items": oa{"type": "string"}},
					},
				},
				"FileEntry": oa{
					"type": "object",
					"properties": oa{
						"path":     oa{"type": "string", "description": "Path relative to pb_public, forward-slash separated."},
						"size":     oa{"type": "integer", "format": "int64", "description": "File size in bytes."},
						"modified": oa{"type": "string", "format": "date-time"},
					},
				},
				"FileContent": oa{
					"type":     "object",
					"required": []any{"path", "content"},
					"properties": oa{
						"path":    oa{"type": "string", "description": "Path relative to pb_public, forward-slash separated."},
						"content": oa{"type": "string", "description": "UTF-8 text content of the file."},
					},
				},
				"Invitation": oa{
					"type": "object",
					"properties": oa{
						"email":     oa{"type": "string", "format": "email"},
						"projectId": oa{"type": "string"},
						"expires":   oa{"type": "string", "format": "date-time"},
					},
				},
				"CreateInvitation": oa{
					"type": "object",
					"properties": oa{
						"email":     oa{"type": "string", "format": "email"},
						"projectId": oa{"type": "string"},
					},
					"required": []any{"email", "projectId"},
				},
				"AcceptInvitation": oa{
					"type": "object",
					"properties": oa{
						"token":    oa{"type": "string"},
						"password": oa{"type": "string"},
					},
					"required": []any{"token"},
				},
			},
		},
	}
}

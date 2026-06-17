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
	// cookieOrKey marks endpoints reachable either with an interactive session
	// cookie or a project-scoped API key (Authorization: Bearer av_...).
	cookieOrKey := []any{oa{"cookieAuth": []any{}}, oa{"bearerAuth": []any{}}}

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
	keyIDParam := oa{
		"name": "keyId", "in": "path", "required": true,
		"description": "API key id (the public identifier, not the secret token).",
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
					"400": errResp("Invalid or reserved project id"),
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
				"tags": []any{"projects"}, "summary": "Update a project's name, status and/or SPA mode (superuser only)",
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
				"description": "Mints a single-use ticket and 302-redirects into the project's PocketBase admin dashboard, already logged in. Unauthenticated browser navigations (Accept: text/html) are 302-redirected to the control-plane login page instead of receiving a 401.",
				"parameters":  []any{idParam},
				"responses":   oa{"302": oa{"description": "Redirect to the project SSO handoff (or to the login page when unauthenticated in a browser)"}, "401": errResp("Authentication required"), "403": errResp("No dashboard access to this project")},
			},
		},
		"/api/projects/{id}/files": oa{
			"get": oa{
				"tags": []any{"files"}, "summary": "List static files served from the project's pb_public directory",
				"description": "Superusers may list any project's files; collaborators only projects they have been granted. Accepts either a session cookie or a project-scoped API key.",
				"security":    cookieOrKey,
				"parameters":  []any{idParam},
				"responses":   oa{"200": oa{"description": "Flat, sorted file listing", "content": jsonBody(oa{"type": "array", "items": ref("FileEntry")})["content"]}, "401": errResp("Authentication required"), "403": errResp("No access to this project"), "404": errResp("Project not found")},
			},
		},
		"/api/projects/{id}/files/content": oa{
			"get": oa{
				"tags": []any{"files"}, "summary": "Read a single pb_public file",
				"security":   cookieOrKey,
				"parameters": []any{idParam, filePathParam},
				"responses":  oa{"200": oa{"description": "File content", "content": jsonBody(ref("FileContent"))["content"]}, "400": errResp("Invalid path"), "401": errResp("Authentication required"), "403": errResp("No access to this project"), "404": errResp("File not found"), "413": errResp("File is too large to edit")},
			},
			"put": oa{
				"tags": []any{"files"}, "summary": "Create or overwrite a pb_public file",
				"description": "Creates any missing parent directories. Files are served live, so changes are visible immediately at the project URL. Accepts either a session cookie or a project-scoped API key.",
				"security":    cookieOrKey,
				"parameters":  []any{idParam},
				"requestBody": jsonBody(ref("FileContent")),
				"responses":   oa{"200": oa{"description": "File written", "content": jsonBody(ref("FileEntry"))["content"]}, "400": errResp("Invalid path"), "401": errResp("Authentication required"), "403": errResp("No access to this project"), "404": errResp("Project not found"), "413": errResp("Content is too large")},
			},
			"delete": oa{
				"tags": []any{"files"}, "summary": "Delete a single pb_public file",
				"security":   cookieOrKey,
				"parameters": []any{idParam, filePathParam},
				"responses":  oa{"204": oa{"description": "Deleted"}, "400": errResp("Invalid path"), "401": errResp("Authentication required"), "403": errResp("No access to this project"), "404": errResp("File not found")},
			},
		},
		"/api/projects/{id}/deploy": oa{
			"post": oa{
				"tags": []any{"files"}, "summary": "Deploy a built site archive into pb_public",
				"description": "Upload a .tar.gz (gzip) or .zip of a built site; Aviary extracts it and publishes the result into the project's pb_public directory in a single atomic swap (never half-deployed). By default the archive is overlaid on existing files; pass ?clean=true to replace the directory wholesale. The body is the raw archive bytes. Accepts a session cookie or a project-scoped API key — the intended target for CI.",
				"security":    cookieOrKey,
				"parameters":  []any{idParam, oa{"name": "clean", "in": "query", "required": false, "description": "Replace pb_public entirely instead of overlaying.", "schema": oa{"type": "boolean", "default": false}}},
				"requestBody": oa{"required": true, "content": oa{"application/gzip": oa{"schema": oa{"type": "string", "format": "binary"}}, "application/zip": oa{"schema": oa{"type": "string", "format": "binary"}}, "application/octet-stream": oa{"schema": oa{"type": "string", "format": "binary"}}}},
				"responses":   oa{"200": oa{"description": "Deploy published", "content": jsonBody(ref("DeployResult"))["content"]}, "400": errResp("Invalid or unsupported archive"), "401": errResp("Authentication required"), "403": errResp("No access to this project"), "404": errResp("Project not found"), "413": errResp("Archive exceeds the upload limit")},
			},
		},
		"/api/projects/{id}/metrics": oa{
			"get": oa{
				"tags": []any{"files"}, "summary": "Storage usage and quota for a project",
				"description": "Reports the project's total data-directory size, its pb_public static-file usage and file count, the remaining PocketBase data size, the configured pb_public quota (0 = unlimited), and whether the cage is currently booted. Accepts a session cookie or a project-scoped API key.",
				"security":    cookieOrKey,
				"parameters":  []any{idParam},
				"responses":   oa{"200": oa{"description": "Project metrics", "content": jsonBody(ref("ProjectMetrics"))["content"]}, "401": errResp("Authentication required"), "403": errResp("No access to this project"), "404": errResp("Project not found")},
			},
		},
		"/api/projects/{id}/keys": oa{
			"get": oa{
				"tags": []any{"keys"}, "summary": "List a project's API keys",
				"description": "Returns key metadata only — the secret token is shown once at creation and never again. Owner-only (superuser or granted collaborator); API keys cannot manage keys.",
				"parameters":  []any{idParam},
				"responses":   oa{"200": oa{"description": "API keys", "content": jsonBody(oa{"type": "array", "items": ref("APIKey")})["content"]}, "401": errResp("Authentication required"), "403": errResp("No access to this project"), "404": errResp("Project not found")},
			},
			"post": oa{
				"tags": []any{"keys"}, "summary": "Create a project-scoped API key",
				"description": "Mints a non-interactive credential for agents/CI to drive this project's file (and future deploy) endpoints via Authorization: Bearer. The raw token is returned exactly once.",
				"parameters":  []any{idParam},
				"requestBody": jsonBody(ref("CreateAPIKey")),
				"responses":   oa{"201": oa{"description": "Key created (token shown once)", "content": jsonBody(ref("CreatedAPIKey"))["content"]}, "400": errResp("Invalid request"), "401": errResp("Authentication required"), "403": errResp("No access to this project"), "404": errResp("Project not found")},
			},
		},
		"/api/projects/{id}/keys/{keyId}": oa{
			"delete": oa{
				"tags": []any{"keys"}, "summary": "Revoke an API key",
				"parameters": []any{idParam, keyIDParam},
				"responses":  oa{"204": oa{"description": "Revoked"}, "401": errResp("Authentication required"), "403": errResp("No access to this project"), "404": errResp("API key not found")},
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
			oa{"name": "keys", "description": "Per-project API keys for agents/CI"},
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
				"bearerAuth": oa{
					"type": "http", "scheme": "bearer",
					"description": "A project-scoped API key (token of the form av_...) sent as 'Authorization: Bearer <token>'. Authorizes only that project's file and deploy endpoints.",
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
						"id":         oa{"type": "string"},
						"name":       oa{"type": "string"},
						"status":     oa{"type": "string", "enum": []any{"active", "disabled"}},
						"spa":        oa{"type": "boolean", "description": "Serve index.html for unmatched paths (single-page-app fallback)."},
						"quotaBytes": oa{"type": "integer", "format": "int64", "description": "pb_public storage quota in bytes; 0 means unlimited."},
						"created":    oa{"type": "string", "format": "date-time"},
						"updated":    oa{"type": "string", "format": "date-time"},
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
						"id":   oa{"type": "string", "description": "Subdomain-safe id (lower-case alphanumerics and hyphens, leading alnum, 1-40 chars). Reserved labels such as 'aviary-console' and 'www' are rejected.", "pattern": "^[a-z0-9][a-z0-9-]{0,39}$"},
						"name": oa{"type": "string"},
					},
					"required": []any{"id"},
				},
				"PatchProject": oa{
					"type": "object",
					"properties": oa{
						"name":       oa{"type": "string"},
						"status":     oa{"type": "string", "enum": []any{"active", "disabled"}},
						"spa":        oa{"type": "boolean", "description": "Toggle single-page-app fallback; reboots the project."},
						"quotaBytes": oa{"type": "integer", "format": "int64", "description": "pb_public storage quota in bytes; 0 means unlimited."},
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
				"APIKey": oa{
					"type": "object",
					"properties": oa{
						"id":        oa{"type": "string", "description": "Public key id (used to revoke; not the secret token)."},
						"projectId": oa{"type": "string"},
						"label":     oa{"type": "string"},
						"created":   oa{"type": "string", "format": "date-time"},
						"lastUsed":  oa{"type": "string", "format": "date-time", "description": "When the key was last used to authenticate, if ever."},
						"expires":   oa{"type": "string", "format": "date-time", "description": "Expiry, if the key was created with one."},
					},
				},
				"CreateAPIKey": oa{
					"type": "object",
					"properties": oa{
						"label":         oa{"type": "string", "description": "Human-readable label (e.g. 'github-actions')."},
						"expiresInDays": oa{"type": "integer", "description": "Optional lifetime in days; omit for a non-expiring key."},
					},
				},
				"CreatedAPIKey": oa{
					"allOf": []any{
						ref("APIKey"),
						oa{"type": "object", "required": []any{"token"}, "properties": oa{"token": oa{"type": "string", "description": "The secret token, shown exactly once. Send it as 'Authorization: Bearer <token>'."}}},
					},
				},
				"DeployResult": oa{
					"type": "object",
					"properties": oa{
						"mode":  oa{"type": "string", "enum": []any{"overlay", "replace"}, "description": "Whether the archive was overlaid on existing files or replaced them."},
						"files": oa{"type": "integer", "description": "Number of files extracted and published."},
						"bytes": oa{"type": "integer", "format": "int64", "description": "Total uncompressed bytes written."},
					},
				},
				"ProjectMetrics": oa{
					"type": "object",
					"properties": oa{
						"running":       oa{"type": "boolean", "description": "Whether the project's cage is currently booted."},
						"lastActive":    oa{"type": []any{"string", "null"}, "format": "date-time", "description": "When the cage last served a request, or null if not running."},
						"storageBytes":  oa{"type": "integer", "format": "int64", "description": "Total size of the project's data directory."},
						"publicBytes":   oa{"type": "integer", "format": "int64", "description": "Bytes used by pb_public static files."},
						"databaseBytes": oa{"type": "integer", "format": "int64", "description": "Bytes used by PocketBase databases, logs and hooks (storage minus public)."},
						"publicFiles":   oa{"type": "integer", "description": "Number of regular files under pb_public."},
						"quotaBytes":    oa{"type": "integer", "format": "int64", "description": "Configured pb_public quota in bytes; 0 means unlimited."},
						"overQuota":     oa{"type": "boolean", "description": "Whether pb_public usage exceeds a non-zero quota."},
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

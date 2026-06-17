# Aviary

A self-hosted, low-footprint backend platform: **many isolated projects per
instance**, in the spirit of Firebase project management — built on top of
[PocketBase](https://github.com/pocketbase/pocketbase).

> Status: proof-of-concept. This first milestone proves the core architectural
> premise — running many fully-isolated PocketBase apps (`core.App`) inside a
> single process, routed by subdomain, started lazily and evicted when idle.

## Why

PocketBase is a single-project-per-instance backend. Aviary keeps PocketBase's
single-binary, low-memory ergonomics but lets one instance host many projects,
each with its own SQLite database, files and settings — so you update and
operate one thing instead of one process per project.

## How it works

```
                       ┌──────────────────────────────────────────┐
   alpha.localhost ──► │ Aviary front (http.Handler, by subdomain) │
   beta.localhost  ──► │                                           │
                       │   cages: map[projectID]*PocketBase app    │
                       │     alpha ─► ./projects/alpha (data.db)    │
                       │     beta  ─► ./projects/beta  (data.db)    │
                       └──────────────────────────────────────────┘
```

* Each project is a real PocketBase `core.App` bound to its own data dir.
* `buildHandler` reproduces `apis.Serve` minus the TCP listener, so every
  project's REST API **and** admin dashboard are served in-memory — no
  per-project port or proxy hop.
* Projects are explicitly provisioned (registry entry + data dir), then boot
  lazily on first request and are evicted after `--idle-ttl`.

## Data layout

```
<data>/                 # --data, default ./data
  control.db            # control-plane registry of projects
  projects/
    alpha/              # isolated PocketBase data dir (data.db, files, ...)
    beta/
```

## Run

Projects must be provisioned before they serve traffic. For local development
the `--seed` flag auto-provisions a comma-separated list on startup:

```sh
go run . --addr 127.0.0.1:8090 --idle-ttl 5m --seed alpha,beta
```

Every flag has an environment-variable fallback (the flag wins when both are
set), which makes headless/container deployment easy:

| Flag         | Env var             | Default            |
| ------------ | ------------------- | ------------------ |
| `--addr`     | `AVIARY_ADDR`       | `127.0.0.1:8090`   |
| `--data`     | `AVIARY_DATA`       | `./data`           |
| `--idle-ttl` | `AVIARY_IDLE_TTL`   | `5m`               |
| `--seed`     | `AVIARY_SEED`       | _(empty)_          |
| `--allow-dashboard-password` | `AVIARY_PB_PASSWORD_LOGIN` | `false` |

Set `AVIARY_SUPERUSER_EMAIL` and `AVIARY_SUPERUSER_PASSWORD` to bootstrap the
control-plane superuser on first run without using the web setup flow (ignored
once a superuser already exists).

Hit two isolated projects (each with its own database):

```sh
curl -s -H 'Host: alpha.localhost' http://127.0.0.1:8090/api/health
curl -s -H 'Host: beta.localhost'  http://127.0.0.1:8090/api/health
```

Unprovisioned subdomains return `404`; disabled projects return `403`.

## Building & releases

Aviary uses pure-Go SQLite (`modernc.org/sqlite`), so it cross-compiles without
CGO or a C toolchain. Build a binary locally with:

```sh
CGO_ENABLED=0 go build -o aviary .
./aviary -version
```

Two GitHub workflows automate this:

- **CI** (`.github/workflows/ci.yml`) runs `go vet`, `go build` and the test
  suite (`go test -race`) on every push and pull request to `main`.
- **Release** (`.github/workflows/release.yml`) runs [GoReleaser] on every
  pushed `v*` tag. It cross-compiles binaries for Linux, macOS and Windows
  (amd64/arm64/arm) and publishes a GitHub Release with zip archives and a
  `checksums.txt`.

Cutting a release is just pushing a tag:

```sh
git tag v0.1.0
git push origin v0.1.0
```

The version is stamped into the binary at build time
(`-ldflags "-X main.version=..."`) and surfaced via `aviary -version`. To
dry-run the release pipeline locally, install GoReleaser and run
`goreleaser release --snapshot --clean`.

[GoReleaser]: https://goreleaser.com

### Updating

Once a release is published, upgrade in place with the built-in self-updater —
no need to fetch artifacts by hand:

```sh
aviary update            # download + verify + install the latest release
aviary update --check    # report the latest version without installing
aviary update --version v0.2.0   # pin a specific version
```

`update` queries the GitHub releases API, picks the archive matching the running
`GOOS`/`GOARCH`, verifies its SHA-256 against the release `checksums.txt`, then
**atomically** swaps the new binary over the current executable (keeping a backup
for rollback). It refuses to run from a non-release `(untracked)` build unless
you pass `--force`, and points at `tupini07/aviary` by default (override with
`--repo owner/name` for forks). A running server must be restarted to pick up the
new binary — the update replaces the file, it does not hot-reload the process.

## Control plane (web UI + API)

The control plane is served on the bare host (no project subdomain), e.g.
`http://127.0.0.1:8090/`. Behind a reverse proxy on a real domain, reach it via
the reserved **`aviary-console`** subdomain (e.g. `aviary-console.apps.example.com`);
`www` is also a reserved alias. Both are rejected as project ids so they can never
collide with a tenant. (`_console` and `_` remain reserved as legacy aliases for
local testing via an explicit `Host` header, but are unreachable over real DNS
since underscores are invalid in hostnames.) It ships a single-page web UI to:

* create / delete / disable / enable projects,
* open any project's admin dashboard,
* manage the platform **superuser**.

On first run the UI prompts you to create the initial superuser. All API and UI
actions (except first-run setup and login) require an authenticated superuser
session (a signed, HTTP-only cookie).

### Federated superuser & dashboard SSO

There is **one** control-plane superuser. Its identity is automatically
propagated into every project's `_superusers` collection — on project boot and
whenever the password changes — so the **same account administers every
project**. You never create per-project admin accounts.

You don't log into a project dashboard with a password. Instead, the control
plane signs you in: clicking **Dashboard** (or hitting
`GET /api/projects/{id}/dashboard`) mints a **single-use, short-lived ticket**,
redirects you to the project's subdomain, where Aviary mints a PocketBase
superuser **auth token** and drops you into `/_/` already logged in. Your
control-plane passkey therefore effectively unlocks every project dashboard too.

The admin UI (`/_/`) is **gated**: any browser that reaches it without a valid,
control-plane-issued dashboard session is bounced through this SSO flow first, so
you never see PocketBase's own login page. If you are already signed into the
control plane the round-trip is invisible (a couple of redirects); if not, you
land on the control-plane login page. A short-lived, signed, host-only cookie
(`aviary_dash`) marks a browser as authorized for that one project's dashboard so
subsequent loads aren't re-bounced.

For security, PocketBase's native superuser **password login is disabled** on
every project by default — with no password endpoint there is nothing to
brute-force, and the only way in is an Aviary-minted token. Set
`AVIARY_PB_PASSWORD_LOGIN=true` (or `--allow-dashboard-password`) to re-enable
native password login if you need it; doing so also leaves the `/_/` login page
ungated so you can use it.

```sh
# first-run setup (only allowed while no superuser exists)
curl -s -X PUT http://127.0.0.1:8090/api/superuser \
  -H 'Content-Type: application/json' \
  -d '{"email":"admin@example.com","password":"password123"}'

# log in (stores the session cookie)
curl -s -c cj -X POST http://127.0.0.1:8090/api/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"email":"admin@example.com","password":"password123"}'

# manage projects (authenticated)
curl -s -b cj -X POST http://127.0.0.1:8090/api/projects \
  -H 'Content-Type: application/json' -d '{"id":"alpha","name":"Alpha"}'

# one-click into the project dashboard (302 → project subdomain, already logged in)
curl -s -b cj -i http://127.0.0.1:8090/api/projects/alpha/dashboard
```

| Method & path                     | Auth            | Purpose                          |
| --------------------------------- | --------------- | -------------------------------- |
| `GET /`                           | public          | Control-plane web UI             |
| `GET /api/openapi.json`           | public          | OpenAPI 3.1 spec of this API     |
| `GET /api/auth/session`           | public          | Current auth + setup state       |
| `POST /api/auth/login`            | public          | Log in, set session cookie       |
| `POST /api/auth/logout`           | public          | Clear session cookie             |
| `PUT /api/superuser`              | superuser²      | Create/update the superuser      |
| `GET /api/superuser`              | superuser       | Get the superuser email          |
| `POST /api/projects`              | superuser       | Create a project                 |
| `GET /api/projects/{id}`          | any¹            | Get a project                    |
| `GET /api/projects/{id}/dashboard`| any¹            | SSO into the project dashboard   |
| `PATCH /api/projects/{id}`        | superuser       | Enable/disable/rename/SPA-toggle a project |
| `DELETE /api/projects/{id}`       | superuser       | Delete a project + its data      |
| `GET /api/projects/{id}/files`    | any¹            | List the project's pb_public files |
| `GET /api/projects/{id}/files/content?path=…` | any¹ | Read a pb_public file            |
| `PUT /api/projects/{id}/files/content` | any¹       | Create/overwrite a pb_public file |
| `DELETE /api/projects/{id}/files/content?path=…` | any¹ | Delete a pb_public file       |
| `POST /api/projects/{id}/deploy`  | any¹            | Atomically deploy a `.tar.gz`/`.zip` into pb_public |
| `GET /api/projects/{id}/keys`     | owner³          | List a project's API keys        |
| `POST /api/projects/{id}/keys`    | owner³          | Mint a project-scoped API key (token shown once) |
| `DELETE /api/projects/{id}/keys/{keyId}` | owner³   | Revoke an API key                |
| `GET /api/projects`               | any¹            | List projects (scoped by access) |
| `POST /api/invitations`           | superuser       | Invite a collaborator to a project |
| `GET /api/invitations`            | superuser       | List pending invitations         |
| `POST /api/invitations/accept`    | public          | Redeem an invitation token       |
| `GET /api/collaborators`          | superuser       | List collaborators + their grants |
| `DELETE /api/collaborators/{email}` | superuser     | Remove a collaborator entirely   |
| `POST /api/auth/passkey/login/*`  | public          | Superuser passwordless sign-in   |
| `POST /api/auth/passkey/register/*` | superuser     | Enroll a superuser passkey       |
| `PUT /api/auth/security`          | superuser       | Toggle passkey-only sign-in      |

¹ `GET /api/projects`, `GET /api/projects/{id}`, the dashboard SSO and the
per-project `files` endpoints are available to collaborators too, but only for
the projects they have been granted; instance-wide mutations are superuser-only.
The `files` endpoints additionally accept a **project-scoped API key** (see
[API keys](#api-keys-for-agents--ci)). So does `POST /api/projects/{id}/deploy`
(see [Deploying a built site](#deploying-a-built-site)).

² `PUT /api/superuser` is allowed unauthenticated **only** for first-run setup
(while no superuser exists); afterwards it requires a session.

³ *owner* = a superuser or a collaborator granted that project. Key management is
deliberately **not** reachable with an API key, so a leaked deploy key cannot
mint further keys.

### API documentation (OpenAPI 3.1)

Aviary serves machine-readable OpenAPI 3.1 specs so tools — and AI agents — can
discover the API without external docs. They are generated/served as JSON, no
build step required:

- **Control plane:** `GET /api/openapi.json` on the control host. A small,
  hand-authored document describing project management, auth, collaborators and
  invitations.
- **Per project:** `GET /__aviary/openapi.json` on each project's subdomain
  (e.g. `https://alpha.example.com/__aviary/openapi.json`). Generated **on the
  fly** by reflecting over that project's current PocketBase collections, so it
  always matches the live schema. It documents the records CRUD endpoints (with
  a JSON Schema per collection) plus the auth and Aviary passkey endpoints.
  Internal/system collections (the `_`-prefixed namespace, including Aviary's
  `_passkeys` store) are excluded.

```bash
# control-plane API spec
curl -s http://127.0.0.1:8090/api/openapi.json | jq .info

# a project's live API spec (records + auth, derived from its collections)
curl -s http://alpha.localhost:8090/__aviary/openapi.json | jq '.paths | keys'
```

Realtime, batch, file-token and OAuth2 endpoints are part of PocketBase but are
not enumerated in the per-project spec; see <https://pocketbase.io/docs/> for
the full PocketBase reference.

The control plane also renders this spec in-app: the **Docs** section reads
`/api/openapi.json` and lists every control-plane endpoint (grouped by tag, with
parameters, responses and auth requirements), alongside links to the raw JSON
and to each project's live `/__aviary/openapi.json`.

### Static file hosting & editor (pb_public)

Each project can serve static assets (HTML/CSS/JS, a landing page, a full SPA)
straight from its own `pb_public` directory, exactly like a stock PocketBase
install. The files live at `projects/<id>/pb_public/` and are served at the
project's public root:

```
https://<id>.<host>/            → projects/<id>/pb_public/index.html
https://<id>.<host>/css/app.css → projects/<id>/pb_public/css/app.css
```

Files are read live from disk, so edits show up immediately with no project
reboot. The API (`/api/...`) and admin dashboard (`/_/...`) always take
precedence over the static fallback.

**Single-page-app mode.** Each project has a per-project **SPA** toggle (in the
Files view, or `PATCH /api/projects/{id}` with `{"spa": true}`). When off (the
default), an unmatched path returns a plain 404. When on, the static server
serves `index.html` for any path that doesn't resolve to a file, so client-side
routers (React, Vue, SvelteKit…) own deep links and page reloads. Toggling it
reboots the project so the new mode takes effect on the next request.

You can manage these files without server/SSH access: the control-plane UI has a
per-project **Files** button (in the Projects table) that opens a simple editor —
list, view, edit, create and delete files — backed by the
`/api/projects/{id}/files` endpoints above. Access mirrors the dashboard:
superusers may edit any project, collaborators only the projects they've been
granted. Paths are confined to `pb_public` (traversal outside it is rejected),
and editable files are capped at 5 MiB.

```bash
# upload/overwrite a file
curl -s -b cj -X PUT http://127.0.0.1:8090/api/projects/alpha/files/content \
  -H 'Content-Type: application/json' \
  -d '{"path":"index.html","content":"<h1>hello</h1>"}'

# it is now served at the project root
curl -s http://alpha.localhost:8090/
```

### API keys (for agents & CI)

Editing files through the UI is convenient for humans, but agents and CI want a
non-interactive credential. Each project can mint **project-scoped API keys**: a
key authorizes exactly one project's `files` and `deploy` endpoints and nothing
else — never instance-wide operations, and never key management itself, so a
leaked deploy key cannot escalate by minting more keys.

Create and revoke keys from the **Files** view (the *API keys* card), or via the
API. The raw token is shown **once**, at creation; only its SHA-256 hash is
stored. Pass it as a bearer token:

```bash
# mint a key (owner session: superuser or granted collaborator)
curl -s -b cj -X POST http://127.0.0.1:8090/api/projects/alpha/keys \
  -H 'Content-Type: application/json' \
  -d '{"label":"github-actions","expiresInDays":90}'
# → {"id":"…","token":"av_…","label":"github-actions", …}   (copy the token now!)

# an agent/CI then publishes with just the token — no cookie, no login
curl -s -X PUT http://127.0.0.1:8090/api/projects/alpha/files/content \
  -H 'Authorization: Bearer av_…' \
  -H 'Content-Type: application/json' \
  -d '{"path":"index.html","content":"<h1>shipped from CI</h1>"}'
```

Keys may carry an optional expiry (`expiresInDays`); omit it for a non-expiring
key. Revoking a key (or deleting its project) invalidates it immediately.

### Deploying a built site

Writing files one-by-one is fine for small edits, but a real web app is a whole
build output (`dist/`). `POST /api/projects/{id}/deploy` takes an entire site as
a single `.tar.gz` or `.zip` (format auto-detected) and swaps it into `pb_public`
**atomically**: the archive is extracted into a staging directory alongside
`pb_public`, then renamed into place, so a request never sees a half-written
site, and a failed/corrupt upload leaves the live site untouched. The static
route reads live from disk, so the new site is served immediately — no reboot.

- **Overlay (default):** the archive is layered over the current `pb_public`,
  so files you don't include are kept.
- **Clean replace:** add `?clean=true` to replace `pb_public` wholesale.

Authenticate with an owner session **or** a project API key. Safety caps apply:
50 MiB compressed upload, 250 MiB uncompressed, 5000 files; path traversal
(`../`, absolute paths) is rejected.

```bash
# tar the *contents* of dist (-C dist .) so paths land at the pb_public root
tar -C dist -czf - . | curl -s -X POST \
  -H 'Authorization: Bearer av_…' \
  -H 'Content-Type: application/gzip' \
  --data-binary @- \
  http://127.0.0.1:8090/api/projects/alpha/deploy
```

The [`examples/`](examples/) directory has a portable `deploy.sh` and a ready-to-copy
[GitHub Actions workflow](examples/github-actions-deploy.yml) that builds a web app
in CI and pushes the artifact to Aviary on every push to `main` — the typical
pattern: build in CI, deploy the static output, with the project's own cage as
the PocketBase backend.

### Storage quotas & metrics

Each project exposes a usage snapshot at `GET /api/projects/{id}/metrics`
(same auth as the file endpoints — session cookie or project API key):

```json
{
  "running": false,
  "lastActive": null,
  "storageBytes": 81920,
  "publicBytes": 14336,
  "databaseBytes": 67584,
  "publicFiles": 7,
  "quotaBytes": 1048576,
  "overQuota": false
}
```

`storageBytes` is the whole data directory; `publicBytes`/`publicFiles` cover the
`pb_public` static files; `databaseBytes` is the rest (PocketBase databases, logs,
hooks). `running`/`lastActive` reflect whether the cage is currently booted.

You can cap a project's `pb_public` storage with a **quota** (superuser only),
either from the project **Settings** tab or via
`PATCH /api/projects/{id}` with `{"quotaBytes": 1048576}` (`0` = unlimited). The
quota is enforced at write time: a file write or a `deploy` that would push
`pb_public` past the limit is rejected with `507 Insufficient Storage`, and a
rejected deploy never touches the live site (the limit is checked against the
fully-staged tree before the atomic swap).

### Passkeys (WebAuthn)

Project users (records in each project's `users` collection) sign in with
passkeys via `internal/passkey` (built on `go-webauthn`), mounted on every
project under `/api/aviary/passkey/*`:

| Method & path                              | Auth          | Purpose                          |
| ------------------------------------------ | ------------- | -------------------------------- |
| `POST /api/aviary/passkey/register/begin`  | user          | Start enrolling a passkey        |
| `POST /api/aviary/passkey/register/finish` | user          | Finish enrollment                |
| `POST /api/aviary/passkey/login/begin`     | public        | Start passwordless login         |
| `POST /api/aviary/passkey/login/finish`    | public        | Finish login → user auth token   |
| `GET /api/aviary/passkey`                  | user          | List the caller's own passkeys   |
| `DELETE /api/aviary/passkey/{id}`          | user          | Delete one of the caller's passkeys |

Login is discoverable/passwordless; registration and self-management require the
user's bearer token, and management is scoped to the caller so a user only ever
sees or deletes their own credentials. Credentials live in each project's
API-locked `_passkeys` collection.

The control-plane **superuser** can also register a passkey and sign in to the
dashboard without a password — see the "Sign in with a passkey" button on the
login screen and the Passkeys card once signed in.

**Passkey-only sign-in.** Once at least one superuser passkey is enrolled, the
Passkeys card exposes a *Passkey-only sign-in* toggle (`PUT /api/auth/security`
with `{"passwordLoginDisabled": true}`). When enabled, the superuser's
username/password login is refused (`403`) so a guessed or leaked password is
useless on its own — only a passkey unlocks the account, shrinking the
brute-force / credential-stuffing surface. As a lockout safeguard the toggle
only engages while a passkey exists: removing your last passkey automatically
re-enables password sign-in for recovery. (Project collaborators always sign in
with a password; the toggle applies only to the superuser.)

### Collaborators & invitations

Beyond the single federated superuser, you can invite **collaborators** scoped
to individual projects. The superuser creates a single-use invitation (only its
SHA-256 hash is stored); the invitee accepts it with a password to create a
project-scoped account. A collaborator's credentials propagate only to the
projects they were granted (into those projects' `_superusers`), and the control
plane shows them just their projects — they cannot create, delete, or otherwise
administer the instance. Revoking a grant also removes them from the running
project.

Open a project's admin dashboard from the control plane via the **Dashboard**
button (recommended — it logs you in automatically). Direct navigation to
`http://alpha.localhost:8090/_/` works too (`*.localhost` resolves to loopback in
most browsers): because the admin UI is gated, you'll be bounced through the
control-plane SSO handoff and dropped in already logged in (or sent to the
control-plane login page first if you have no session).

## Layout

| Path                                | Responsibility                                  |
| ----------------------------------- | ----------------------------------------------- |
| `main.go`                           | Flags + front HTTP server + `--seed`            |
| `selfupdate.go`                     | `aviary update` (download + checksum + atomic binary swap) |
| `internal/aviary/aviary.go`         | Registry, subdomain routing, idle eviction      |
| `internal/aviary/provisioning.go`   | Create/list/delete/disable projects             |
| `internal/aviary/controlapi.go`     | Control-plane JSON API (projects + superuser)   |
| `internal/aviary/auth.go`           | Session cookies + superuser auth middleware     |
| `internal/aviary/superuser.go`      | Control-plane superuser identity                 |
| `internal/aviary/superuser_propagation.go` | Propagate superuser into each project    |
| `internal/aviary/superuser_passkey.go` | Superuser WebAuthn ceremonies (control plane) |
| `internal/aviary/dashboard_sso.go`  | One-click dashboard SSO (ticket → minted token) |
| `internal/aviary/files.go`          | Per-project pb_public file editor API (list/read/write/delete) |
| `internal/aviary/deploy.go`         | Atomic archive deploy (.tar.gz/.zip → pb_public swap) |
| `internal/aviary/metrics.go`        | Per-project storage metrics + pb_public quota enforcement |
| `internal/aviary/apikeys.go`        | Project-scoped API keys (mint/list/revoke) + bearer auth |
| `internal/aviary/openapi.go` + `openapi_control.go` + `openapi_project.go` | OpenAPI 3.1 specs (control plane + on-the-fly per project) |
| `internal/aviary/collaborators.go` + `collaborator_api.go` | Invitations + project-scoped collaborators |
| `internal/aviary/ui.go` + `web/`    | Embedded control-plane web UI                   |
| `internal/aviary/cage.go`           | One isolated PocketBase project (lifecycle)     |
| `internal/aviary/handler.go`        | Build a PocketBase handler without a listener   |
| `internal/passkey/`                 | Reusable per-project passkey/WebAuthn extension |
| `internal/controlplane/store.go`    | Persistent project registry (SQLite)            |
| `internal/controlplane/collaborators.go` | Collaborator + invitation persistence      |
| `internal/controlplane/apikeys.go`  | API-key persistence (hashed tokens)             |

## Roadmap

- [x] Many isolated projects per process, routed by subdomain
- [x] Control-plane store + provisioning (create/list/delete/disable projects)
- [x] Control-plane HTTP API + auth (signed session cookies)
- [x] Control-plane web UI
- [x] Federated superuser (one credential unlocks every project dashboard)
- [x] Passkey / WebAuthn auth extension (`go-webauthn`), upstreamable to PocketBase
- [x] Passkey login for superusers (dashboard)
- [x] Env-var configuration + headless superuser bootstrap
- [x] Invitation flow with project-scoped collaborators
- [x] One-click dashboard SSO + gated `/_/` (no PocketBase login page) + disabled password login (no brute-force surface)
- [x] Auto-generated OpenAPI 3.1 specs (control plane + on-the-fly per project)
- [x] Static file hosting per project (`pb_public`) + in-browser file editor + SPA fallback toggle
- [x] Project-scoped API keys for agents/CI (bearer auth on the file/deploy endpoints)
- [x] Atomic archive deploy endpoint + GitHub Action (build in CI, push artifact)
- [x] Self-update command (`aviary update` — download + verify the matching GitHub release, atomic binary swap)
- [x] Per-project storage quotas and metrics (pb_public usage + quota enforcement on write/deploy)

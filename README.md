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

Hit two isolated projects (each with its own database):

```sh
curl -s -H 'Host: alpha.localhost' http://127.0.0.1:8090/api/health
curl -s -H 'Host: beta.localhost'  http://127.0.0.1:8090/api/health
```

Unprovisioned subdomains return `404`; disabled projects return `403`.

The control-plane landing page (no project subdomain) lists projects and which
are currently booted:

```sh
curl -s http://127.0.0.1:8090/
```

Open a project's admin dashboard in a browser at `http://alpha.localhost:8090/_/`
(`*.localhost` resolves to loopback in most browsers).

## Layout

| Path                                | Responsibility                                  |
| ----------------------------------- | ----------------------------------------------- |
| `main.go`                           | Flags + front HTTP server + `--seed`            |
| `internal/aviary/aviary.go`         | Registry, subdomain routing, idle eviction      |
| `internal/aviary/provisioning.go`   | Create/list/delete/disable projects             |
| `internal/aviary/cage.go`           | One isolated PocketBase project (lifecycle)     |
| `internal/aviary/handler.go`        | Build a PocketBase handler without a listener   |
| `internal/controlplane/store.go`    | Persistent project registry (SQLite)            |

## Roadmap

- [x] Many isolated projects per process, routed by subdomain
- [x] Control-plane store + provisioning (create/list/delete/disable projects)
- [ ] Control-plane HTTP API + auth
- [ ] Passkey / WebAuthn auth extension (`go-webauthn`), upstreamable to PocketBase
- [ ] Passkey login for superusers (dashboard)
- [ ] Per-project quotas and metrics

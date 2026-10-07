# Guarded Aviary VPS deployments

The repository updater builds **reviewed, committed runtime-only patched source**,
not a byte-identical vendor executable. The version is `0.4.0-managed.1`; revision
is the full committed HEAD. The original v0.4.0 executable and directory remain
untouched. Dependencies must match `go.mod` and `go.sum` at approved vendor source
`8a7d60b6e43a1a7a3417a80e5038b1a74e61aafe` exactly. No `go mod tidy`, npm build,
dependency upgrade, vendor updater, git-pull loop or generated UI bundle is used.

## CLI

Run from a native Linux amd64 machine with Python 3.11+, Git, OpenSSH, Go
`go1.25.0`, and a working C compiler for the race tests. The target needs Python
3.11+, Linux pidfds and network namespaces, systemd, `nsenter`, Caddy, `ss`,
an existing Backuper lock, and the mounted
`/mnt/volume-hel1-1` volume.

```console
python3 -B scripts/deploy_vps.py [meadow-ubuntu-8gb-hel1-1] --yes --rehearse
python3 -B scripts/deploy_vps.py [meadow-ubuntu-8gb-hel1-1] --yes
```

The optional positional argument is an SSH alias; the default is
`meadow-ubuntu-8gb-hel1-1`. `--yes` is mandatory, including rehearsals. Tracked dirty
files are rejected. Unrelated untracked files are ignored, **not uploaded or
deleted**. All necessary runtime source, embedded `internal/aviary/web/index.html`,
unit, vendor metadata and Python regressions must be committed.

Initial migration (the first command is copied rehearsal only):

```console
python3 -B scripts/deploy_vps.py --yes --rehearse --migrate-tmux \
  --legacy-pid 3944047 --parent-pid 1608151 \
  --legacy-sha256 13200be63d2ba3aaecc5d8d2c8663c11bbe8e3a40e147b7c46fdbf7b3796e019 \
  --vendor-archive /absolute/path/aviary_0.4.0_linux_amd64.zip
python3 -B scripts/deploy_vps.py --yes --migrate-tmux \
  --legacy-pid 3944047 --parent-pid 1608151 \
  --legacy-sha256 13200be63d2ba3aaecc5d8d2c8663c11bbe8e3a40e147b7c46fdbf7b3796e019 \
  --vendor-archive /absolute/path/aviary_0.4.0_linux_amd64.zip \
  --accept-legacy-shutdown-risk \
  --legacy-schedule-review /absolute/path/reviewed-schedule-window.json
```

PIDs above describe the reviewed original and **must be reverified**, not assumed
permanent. The parent shell is identified and rechecked but never signalled;
tmux is not stopped. A pidfd sends SIGTERM only to the exact original process
after matching PID/starttime, parent identity, executable, cwd, argv and hash.
Initial mode requires argv `./aviary`, the intact original directory, and no
`AVIARY_*` environment overrides.

`--vendor-archive` optionally supplies the official zip locally. Without it,
initial mode downloads only the pinned official GitHub URL in `deploy/vendor.json`,
with HTTPS redirects restricted to GitHub's release-asset host. Both zip SHA256
`cd92f7b0b4fb845997d9be9a19e083d63a3256593bb249b9b45ce17a6e64580b` and embedded
binary SHA256 above must match. The zip is retained in the private remote receipt;
it is not executed as the managed build. Legacy flags, vendor archive, schedule
review and risk acknowledgement are rejected outside `--migrate-tmux`.

The build pipeline runs native `CGO_ENABLED=0` non-race Go tests, `CGO_ENABLED=1`
race tests, Go vet, and the Python regression suite, always with
`GOTOOLCHAIN=go1.25.0` and `-mod=readonly`. The final build uses `CGO_ENABLED=0`:

```console
go build -mod=readonly -trimpath -buildvcs=false \
  -ldflags '-X main.version=0.4.0-managed.1 -X main.buildRevision=<40hex revision>' \
  -o <payload>/aviary .
```

`--install /root/aviary-deploy-backups/update-<32hex>` is an internal root-only
interface, invoked from the uploaded **committed** updater by a transient
`aviary-deploy-<32hex>` oneshot unit. Do not run it manually on a pending receipt.
The durable receipt has mode 0700 and contains `source.tar`, `release.tar`,
`request.json`, the updater, initial `official.zip` and optional
`schedule-review.json`, private logs, Caddy before/candidate configurations,
`transaction.json`, `deployment.json`, and the coordinated `state.before` copy.
The installer survives loss of SSH; loss of the waiting client is not evidence
that the installer stopped.

The other internal interface is
`--probe http://127.0.0.1:<port> <40hex revision> <project count> /opt/aviary/releases/<40hex>-<16hex>/control-ui.html`.
It performs copied-mode health (`cronEnabled=false`) and read-only control/UI
checks. The installer executes it through `nsenter --target <copied MainPID> --net`
inside the copied process's private network namespace. Origins, revision, count
and asset path are strictly validated; this is not an arbitrary URL/file probe.

## Initial legacy shutdown risk and schedule gate

**Quiet sockets and stable databases do not prove that a stripped legacy Go
process has no queued SMTP or other unsocketed asynchronous work.** The original
has no graceful drain API; the updater neither uses SIGSTOP, forced SIGKILL nor
Delve to manufacture that guarantee. Initial activation requires the operator's
explicit `--accept-legacy-shutdown-risk` acknowledgement: any remaining background
task may be interrupted by the original release's SIGTERM exit. This is a
one-time initial-migration risk acceptance, not a claim that pending work is
absent or permission to bypass the other guards. A separate, explicitly reviewed,
short-lived schedule window is also required. Copied rehearsal needs neither.

`--legacy-schedule-review` is a JSON object with this exact semantic contract:

| Key | Required value |
| --- | --- |
| `format` | `1` |
| `legacy` | `{"app": <process_identity(legacy_pid)>, "parent": <process_identity(parent_pid)>}` |
| `state_manifest` | Complete `state_manifest(original/data)` result |
| `not_before`, `not_after` | Unix epoch seconds; already valid and at least 900 seconds remaining at the final guard; total window at most 3600 seconds |
| `legacy_shutdown_risk_accepted` | `true`, recording explicit operator approval of the disclosed residual risk |
| `scheduled_writers_excluded` | `true`, only after independent schedule-window review |
| `reviewed_schedules` | At least three nonempty review descriptions covering PocketBase backup, optimization and delayed log flushing, including relevant settings and actual schedules |

`process_identity()` returns `pid`, `parent`, `start` (Linux starttime string),
`exe`, `cwd`, `argv` (base64-encoded argument bytes), and `sha256`. The review
is bound to those exact identities and all logical state/file contents. It is
**schedule review evidence and risk acknowledgement, not proof of an inaccessible
Go queue**. Do not invent a `no_pending_async_work` assertion. There is no
stale-state allowance or override for process, schedule, socket or state checks.

Under maintenance the updater also requires zero control cron jobs, no project
JS-hook files, and repeatedly identical complete logical state. It inspects
every Aviary-owned IPv4/IPv6 TCP and UDP socket, not just port 8090. TCP listeners
are permitted, but every other TCP state and every UDP socket prevent a
continuous 60-second quiet interval. Busy sockets can wait up to six minutes;
any logical state change invalidates the review immediately. The final review
window is checked again immediately before exact-process stop, and stopped state
must equal the reviewed quiet state.

Backend and copied UI checks require the exact complete committed HTML bytes.
Public checks additionally permit only the reviewed Cloudflare analytics/JS
detection insertion before `</body>`, pinned by a normalized SHA256 after
replacing dynamic challenge parameters and the public beacon identifier.
Every original HTML byte must remain unchanged. Other injections, changed
assets or future Cloudflare script changes fail closed; this does not accept
arbitrary scripts or HTML and does not change Cloudflare settings.

## Managed layout and transaction

The runtime unit is committed in `deploy/aviary.service`. The installer uses:

| Path | Purpose |
| --- | --- |
| `/opt/aviary/releases/<revision>-<manifest hash prefix>` | Root-owned immutable full-file release: binary, unit, vendor metadata, revision, UI verification bytes, build provenance, manifest |
| `/opt/aviary/current` | Atomically switched release symlink |
| `/etc/aviary/aviary.env` | Root-owned 0640 configuration, group `aviary` |
| `/mnt/volume-hel1-1/aviary-state` | Dedicated `aviary` account's writable control and complete project state |
| `/opt/aviary/deployment-pending.json` | Fail-closed transaction gate |
| `/opt/aviary/deployed.json` | Last successful configuration baseline and receipt summary |

The nonlogin account is `aviary`, home `/var/lib/aviary`. Effective original
defaults are preserved: loopback `127.0.0.1:8090`, idle TTL `5m`, native dashboard
password login disabled, no seeds, and control cron enabled. Only managed data
paths and `AVIARY_REQUIRE_EXISTING=true` are intentional changes.

The shared `/var/lib/backuper/job.lock` is opened **read-only** and locked
nonblocking before `/opt/aviary/.deploy.lock`. Its inode/mode/ownership are not
recreated or changed. Both locks remain held through rehearsal, maintenance,
activation, public control checks and recovery. No Backuper run, retention
operation or code upgrade is performed.

Initial retargeting changes only the paths of Backuper's existing
`aviary-control`, `aviary-projects`, `aviary-auxiliary`, `aviary-project-files`,
`aviary-exports` and `aviary-current-binary` entries. State-relative paths map
from original `data` to `aviary-state`; the binary maps to
`/opt/aviary/current/aviary`. It adds a files entry `aviary-configuration` for
``/etc/aviary/aviary.env`. It also backs up optional PocketBase export `.zip.attrs`
sidecars as `aviary-export-metadata`. All other entries, destinations, exclusion lists,
`required=false`, settings and secret bytes are preserved. The single Backuper
`ReadWritePaths` line retargets exactly the old data root and its
`projects/backuper-info` and `projects/lgspkb` parents; all other permission
tokens remain unchanged. The new root covers future project subdirectories.
Backuper's running code remains unchanged.

Copied snapshots use SQLite's WAL-safe backup API for Aviary's control and
project `data.db`/`auxiliary.db` stores and cover every database
table, schema object, row (including invalid UTF-8 text and blobs), sequence,
implicit rowid and relevant pragma version, plus all directories and other file
bytes in the complete project tree. Uploaded SQLite files and their sidecars
are ordinary files, preserved byte-for-byte rather than opened or normalized.
Only journals belonging to the owned database stores are omitted; orphan
journals are retained. Redirected ancestors,
symlinks, special files, hardlinks, malformed databases, missing control/projects
roots or missing registry-project directories are rejected. A deliberately
provisioned project's empty directory is valid without `data.db`.
Physical source hashes before/after live copies, copied logical-state equality
and source inventory checks reject concurrent multi-database changes; there is
no automatic retry hiding an inconsistent copy.

SQLite backup's destination schema cookie is reset by SQLite itself; the updater
explicitly retains the original `schema_version` on the copied database, never
on the source. Storage classes are hashed separately, so equal byte sequences
stored as invalid-UTF-8 TEXT versus BLOB remain distinct.
Even opening SQLite with `mode=ro` can create or alter source WAL/SHM files.
All SQLite reads therefore use a private byte-for-byte DB/WAL/rollback-journal
staging copy, verified against source hashes before/after copying.
SQLite replays journals and performs its backup API only on those private bytes;
the final complete source/copy checks still reject concurrent multi-DB changes.
Source SHM is never opened by SQLite or used as a persisted data substitute.
After coherent capture, standalone readers use that private snapshot without
requiring the live source to remain physically frozen throughout inspection.
Later live writes or WAL checkpoints cannot change the captured snapshot.
Whole-tree copying retains its outer physical/logical before/after guards;
this is not a retry or an allowance for changed cutover state.

Copied preflight runs under an isolated transient unit on the mounted volume
with `PrivateNetwork=true` (only its own loopback, no external interfaces),
additional deny-all/allow-localhost IP filtering, control cron disabled and
existing-state validation required. Health and the exact embedded control UI
are checked without lazy-booting a tenant. The configured, unauthenticated
`/api/auth/session` response and protected `/api/projects` response are checked
without credentials. The copied process must stop fully
and preserve all logical state; a stuck check retains its copy for inspection.
`--rehearse` ends here: no public Caddy changes, service replacement, production
state writes or legacy signals. Immutable releases, private receipts, copied
check data and the dedicated account/configuration directory may be created.

At cutover Caddy's disk configuration must match its live adaptation. Only the
single reviewed `reverse_proxy :8090` line 75 is replaced with HTTP 503;
other named services are untouched. The candidate is validated, reloaded and
checked live **before** the original is stopped. Caddy disk bytes, attributes
and live response bytes are restored in `finally`. Operator disk/live edits
are never overwritten and require manual route recovery.
Caddy automatically inserts the adapter input filename into `file_server.hide`.
The private candidate's generated hide entries are normalized to the live
Caddyfile path, then checked against adaptation of the actual installed file
before reload. Other hide entries and unrelated routing remain unchanged.

Managed stop uses the runtime's accepted-HTTP/cron drain and termination-hook
flush. `MainPID=0` and an empty remaining service cgroup are required before
snapshot or switch. `TimeoutStopSec=660`, `KillMode=mixed`, and
`SendSIGKILL=no` prohibit forced termination. A coordinated stopped-state snapshot
precedes initial volume-local relocation or a release switch. Startup health
must match revision/version, exact integer project count and `cronEnabled=true`;
**all stopped logical state must match exactly before any public request check**.
Public checks use only the control origin and never boot project hosts.

Healthy identical releases are PID-preserving no-ops: no stop, snapshot,
rehearsal, symlink switch or Caddy reload. Unit/drop-ins, root-owned release
files, running executable/argv/environment/listener and the last approved
configuration are still checked. Operator edits to unit, environment, Backuper
config/unit, or release paths are gated rather than silently adopted.

## Recovery

Inspect the private receipt and transient installer status before any retry.
A pending marker blocks even identical-release operations. Do not bypass it
or rerun a receipt whose installer may still be active.

If a stopped failed activation left **exactly unchanged complete state** and no
operator configuration/release changes, the updater may restore the previous
binary pointer and exact configuration bytes/attributes, using the **current**
state. It never copies the stopped backup over live data.

For an initial failure, automatic fallback additionally requires unchanged
original complete state and the exact official executable in its intact original
directory. Only that executable is launched by a new transient
`aviary-legacy-recovery-<32hex>.service`, not the old shell, tmux or a wrapper.
The unused managed copy is retained; a future initial migration sees that partial
layout and requires deliberate receipt/layout review. The uploaded official zip
is recovery evidence, not permission to overwrite a changed original.
After that review, preserve the unused managed copy in a named private recovery
receipt, confirm original config and routing were restored, and recapture current
original state/schedules. Initial mode can then use the new exact recovery PID,
`--parent-pid 1`, and `--legacy-recovery-unit <receipt's exact unit>`. Only a
matching active non-restarting `aviary-legacy-recovery-<32hex>.service` without
drop-ins is accepted; its exact absolute argv/hash/cwd and original defaults
are verified. The updater never signals init or the preserved tmux shell.

Any accepted database row/schema/file change, new project, state permission
change, operator configuration edit, unknown pointer, altered release or
unconfirmed stop forbids automatic rollback. The current managed service is
stopped/disabled when stop is possible; all current state and the pending receipt
remain. No SIGKILL, stale database restore, opaque row exception or forced config
overwrite is attempted. If drain is stuck, preserve the running process and
pending marker and inspect diagnostics; do not snapshot/switch while it remains
alive. Recover forward from current accepted state after explicit review.

Tenant public GETs can legitimately lazy-boot PocketBase and modify superuser or
auxiliary logs. This updater has no perpetual all-row equality rule after real
tenant traffic and no allowance list concealing those writes. Native tenant
acceptance and final fleet audit are separate operations; once such writes occur,
an old stopped snapshot is never a rollback source.

Run owned regressions locally without connecting to the VPS:

```console
python3 -B -m unittest discover -s scripts -p test_deploy_vps.py -v
```

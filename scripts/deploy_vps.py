"""Build committed Aviary source and install it with fail-closed state guards."""

import argparse
import base64
from contextlib import closing, contextmanager
import copy
import fcntl
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import platform
import pwd
import re
import shlex
import shutil
import signal
import socket
import sqlite3
import stat
import subprocess
import sys
import tarfile
import tempfile
import time
import tomllib
from urllib.error import HTTPError, URLError
from urllib.parse import urlsplit
from urllib.request import HTTPRedirectHandler, ProxyHandler, Request, build_opener
import uuid
import zipfile


LEGACY = Path("/mnt/volume-hel1-1/little-green-notebook-aviary")
DATA = Path("/mnt/volume-hel1-1/aviary-state")
ROOT = Path("/opt/aviary")
ENV = Path("/etc/aviary/aviary.env")
PUBLIC = "https://aviary-console.meadow.cafe"
TOOLCHAIN = "go1.25.0"
VERSION = "0.4.0-managed.1"
VENDOR = {
    "version": "0.4.0",
    "source_revision": "8a7d60b6e43a1a7a3417a80e5038b1a74e61aafe",
    "go_version": "1.25.0",
    "archive_url": "https://github.com/tupini07/aviary/releases/download/v0.4.0/aviary_0.4.0_linux_amd64.zip",
    "archive_sha256": "cd92f7b0b4fb845997d9be9a19e083d63a3256593bb249b9b45ce17a6e64580b",
    "binary_sha256": "13200be63d2ba3aaecc5d8d2c8663c11bbe8e3a40e147b7c46fdbf7b3796e019",
}
PAYLOAD_FILES = {"aviary", "aviary.service", "vendor.json", "revision.txt",
                 "control-ui.html", "build.json"}
CONFIGURATION = (
    "AVIARY_ADDR=127.0.0.1:8090\n"
    "AVIARY_DATA=/mnt/volume-hel1-1/aviary-state\n"
    "AVIARY_IDLE_TTL=5m\n"
    "AVIARY_PB_PASSWORD_LOGIN=false\n"
    "AVIARY_SEED=\n"
    "AVIARY_REQUIRE_EXISTING=true\n"
    "AVIARY_DISABLE_CRON=false\n"
).encode("ascii")
UNSET_LEGACY_ENVIRONMENT = " ".join([
    *(line.split(b"=", 1)[0].decode("ascii") for line in CONFIGURATION.splitlines()),
    "AVIARY_SUPERUSER_EMAIL", "AVIARY_SUPERUSER_PASSWORD",
])
QUIET_SECONDS = 60


class DeploymentError(RuntimeError):
    pass


def require(condition, message):
    if not condition:
        raise DeploymentError(message)


def checksum(path):
    with Path(path).open("rb") as source:
        return hashlib.file_digest(source, "sha256").hexdigest()


def safe_path(path, kind=None):
    path = Path(path).absolute()
    for ancestor in reversed([path, *path.parents]):
        if ancestor.exists() or ancestor.is_symlink():
            require(not ancestor.is_symlink(), "Redirected path: " + str(ancestor))
    if kind:
        require(path.exists(), "Missing required path: " + str(path))
        mode = path.lstat().st_mode
        require(stat.S_ISDIR(mode) if kind == "directory" else stat.S_ISREG(mode),
                "Unexpected path type: " + str(path))
    return path


def sync_directory(path):
    fd = os.open(path, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def private_write(path, contents, mode=0o600, owner=None):
    path = safe_path(path)
    safe_path(path.parent, "directory")
    fd, name = tempfile.mkstemp(prefix=".install-", dir=path.parent)
    temporary = Path(name)
    try:
        with os.fdopen(fd, "wb") as output:
            output.write(contents.encode("utf-8") if isinstance(contents, str) else contents)
            os.fchmod(output.fileno(), mode)
            if owner is not None:
                os.fchown(output.fileno(), *owner)
            output.flush()
            os.fsync(output.fileno())
        temporary.replace(path)
        sync_directory(path.parent)
    finally:
        temporary.unlink(missing_ok=True)


def write_json(path, value):
    private_write(path, json.dumps(value, indent=2, sort_keys=True) + "\n")


def archive_name(name):
    path = PurePosixPath(name)
    require(path.parts and not path.is_absolute() and ".." not in path.parts
            and "\\" not in name and "\0" not in name
            and path.as_posix() == name.rstrip("/"), "Unsafe archive path")
    return path


def extract(archive, destination):
    safe_path(destination, "directory")
    seen = set()
    with tarfile.open(archive) as source:
        for member in source:
            path = archive_name(member.name)
            require(path.as_posix() not in seen and (member.isfile() or member.isdir())
                    and not member.issparse(), "Archive has duplicate paths, links or special files")
            seen.add(path.as_posix())
            target = destination.joinpath(*path.parts)
            safe_path(target)
            target.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
            if member.isdir():
                target.mkdir(mode=0o700, exist_ok=True)
            else:
                with source.extractfile(member) as incoming, target.open("xb") as output:
                    shutil.copyfileobj(incoming, output)
                target.chmod(0o700 if member.mode & 0o111 else 0o600)


def vendor_binary(archive):
    safe_path(archive, "file")
    require(checksum(archive) == VENDOR["archive_sha256"], "Official zip checksum mismatch")
    seen = set()
    binary = None
    with zipfile.ZipFile(archive) as source:
        for member in source.infolist():
            name = archive_name(member.filename)
            mode = member.external_attr >> 16
            require(name.as_posix() not in seen and not stat.S_ISLNK(mode)
                    and (stat.S_IFMT(mode) in (0, stat.S_IFREG, stat.S_IFDIR))
                    and member.file_size <= 256 * 1024**2,
                    "Unsafe official archive")
            seen.add(name.as_posix())
            if member.filename == "aviary":
                require(not member.is_dir(), "Official executable is not a file")
                binary = source.read(member)
    require(binary is not None and hashlib.sha256(binary).hexdigest() == VENDOR["binary_sha256"],
            "Official executable checksum mismatch")
    return binary


class VendorRedirect(HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        parsed = urlsplit(newurl)
        require(parsed.scheme == "https" and parsed.hostname == "release-assets.githubusercontent.com"
                and parsed.port in (None, 443) and not parsed.username and not parsed.password,
                "Official archive redirected outside GitHub release assets")
        return super().redirect_request(req, fp, code, msg, headers, newurl)


def obtain_vendor(destination, supplied=None):
    if supplied is not None:
        safe_path(supplied, "file")
        shutil.copyfile(supplied, destination)
    else:
        with build_opener(ProxyHandler({}), VendorRedirect()).open(
                VENDOR["archive_url"], timeout=60) as response, destination.open("xb") as output:
            require(response.status == 200, "Official archive download failed")
            total = 0
            while block := response.read(1024**2):
                total += len(block)
                require(total <= 256 * 1024**2, "Official archive exceeds size limit")
                output.write(block)
    vendor_binary(destination)


def frame(value):
    value = repr(value).encode("ascii")
    return len(value).to_bytes(8, "big") + value


def database_digest(db):
    original_factory = db.text_factory
    db.text_factory = bytes
    digest = hashlib.sha256()
    try:
        require(db.execute("PRAGMA integrity_check").fetchall() == [(b"ok",)],
                "SQLite integrity check failed")
        schema = db.execute(
            "SELECT type,name,tbl_name,sql FROM sqlite_master ORDER BY type,name"
        ).fetchall()
        digest.update(frame(schema))
        tables = {row[1]: row for row in db.execute("PRAGMA table_list") if row[0] == b"main"}
        for name, sql in db.execute(
                "SELECT name,sql FROM sqlite_master WHERE type='table' ORDER BY name"):
            try:
                quoted = '"' + name.decode("utf-8").replace('"', '""') + '"'
            except UnicodeDecodeError as error:
                raise DeploymentError("SQLite table identifier is not valid UTF-8") from error
            info = list(db.execute("PRAGMA table_xinfo(" + quoted + ")"))
            columns = {row[1].lower() for row in info}
            # Include implicit rowids when they exist, without relying on row ordering or PK uniqueness.
            projection = []
            require(name in tables, "SQLite table inventory is unavailable")
            if tables[name][4] == 0:
                aliases = [x for x in ("rowid", "_rowid_", "oid") if x.encode() not in columns]
                require(aliases, "All hidden-rowid aliases are shadowed; manual state review required")
                projection += ["typeof(" + aliases[0] + ")", aliases[0]]
            for column in info:
                if column[6] == 1:
                    continue
                try:
                    identifier = '"' + column[1].decode("utf-8").replace('"', '""') + '"'
                except UnicodeDecodeError as error:
                    raise DeploymentError("SQLite column identifier is not valid UTF-8") from error
                # text_factory=bytes preserves invalid UTF-8, but typeof is needed to distinguish TEXT/BLOB.
                projection += ["typeof(" + identifier + ")", identifier]
            rows = sorted(hashlib.sha256(frame(row)).digest()
                          for row in db.execute("SELECT " + ",".join(projection) + " FROM " + quoted))
            digest.update(frame(name))
            digest.update(frame(rows))
        for pragma in ("user_version", "schema_version", "application_id", "encoding", "auto_vacuum", "page_size"):
            digest.update(frame((pragma, db.execute("PRAGMA " + pragma).fetchone())))
    finally:
        db.text_factory = original_factory
    return digest.hexdigest()


@contextmanager
def private_database(path):
    safe_path(path, "file")
    paths = [path, Path(str(path) + "-wal"), Path(str(path) + "-journal")]

    def hashes():
        values = {}
        for source in paths:
            safe_path(source)
            if source.exists():
                safe_path(source, "file")
                require(source.stat().st_nlink == 1, "Hardlinked SQLite source")
                values[source.name] = checksum(source)
        return values

    before = hashes()
    with tempfile.TemporaryDirectory(prefix="aviary-sqlite-") as temporary:
        target = Path(temporary) / "database.db"
        for source in paths:
            if source.name in before:
                copied = Path(str(target) + str(source)[len(str(path)):])
                shutil.copyfile(source, copied)
                require(checksum(copied) == before[source.name], "SQLite source changed while copying bytes")
        require(hashes() == before, "SQLite database/journal changed while preparing private read")
        # Even mode=ro can create/change WAL and SHM on the original. Replay and inspect only private bytes.
        with closing(sqlite3.connect(target, timeout=5)) as db:
            db.execute("BEGIN")
            yield db
        require(hashes() == before, "SQLite source changed during private read")


def read_digest(path):
    with private_database(path) as db:
        return database_digest(db)


def sqlite_file(path):
    with path.open("rb") as source:
        return source.read(16) == b"SQLite format 3\0"


def state_entries(state):
    safe_path(state, "directory")
    safe_path(state / "control.db", "file")
    safe_path(state / "projects", "directory")
    require(sqlite_file(state / "control.db"), "Control database is not SQLite")
    entries = {}
    for path in sorted(state.rglob("*")):
        mode = path.lstat().st_mode
        require(stat.S_ISDIR(mode) or stat.S_ISREG(mode),
                "State contains links or special files: " + str(path))
        require(not path.is_file() or path.stat().st_nlink == 1, "Hardlinked state is not supported")
        entries[path.relative_to(state).as_posix()] = path
    with private_database(state / "control.db") as db:
        db.text_factory = bytes
        for name, in db.execute("SELECT id FROM projects ORDER BY id"):
            require(isinstance(name, bytes) and re.fullmatch(b"[a-z0-9][a-z0-9-]{0,39}", name),
                    "Unsafe project registry identifier")
            safe_path(state / "projects" / name.decode("ascii"), "directory")
    return entries


def state_manifest(state, physical=False):
    entries = state_entries(state)
    databases = set()
    for name, path in entries.items():
        parts = PurePosixPath(name).parts
        if name == "control.db" or (len(parts) == 3 and parts[0] == "projects"
                                    and parts[2] in ("data.db", "auxiliary.db")):
            require(path.is_file() and sqlite_file(path), "Invalid SQLite state file: " + name)
            databases.add(name)
    result = {}
    for name, path in entries.items():
        if path.is_dir():
            result[name] = {"kind": "directory"}
            continue
        base = next((name.removesuffix(suffix) for suffix in ("-wal", "-shm", "-journal")
                     if name.endswith(suffix)), None)
        if not physical and base in databases:
            require(not sqlite_file(path), "SQLite/database journal filename collision: " + name)
            continue
        result[name] = {"kind": "sqlite" if name in databases else "file",
                        "sha256": checksum(path) if physical or name not in databases else read_digest(path),
                        "executable": bool(path.stat().st_mode & 0o111)}
    return result


def snapshot(source, destination):
    safe_path(source, "file")
    safe_path(destination)
    fd = os.open(destination, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    os.close(fd)
    deadline = time.monotonic() + 120

    def progress(*_):
        require(time.monotonic() < deadline, "SQLite backup deadline exceeded")

    with private_database(source) as incoming:
        schema_version = incoming.execute("PRAGMA schema_version").fetchone()[0]
        with closing(sqlite3.connect(destination)) as output:
            incoming.backup(output, pages=256, progress=progress, sleep=0.05)
            # SQLite backup resets the destination schema cookie; retain it on the copy, never the source.
            output.execute("PRAGMA schema_version=" + str(schema_version))
            database_digest(output)


def copy_state(source, destination):
    physical_before = state_manifest(source, physical=True)
    before = state_manifest(source)
    safe_path(destination)
    require(not destination.exists(), "Snapshot destination already exists")
    destination.mkdir(mode=0o700)
    for name, item in before.items():
        target = destination / name
        if item["kind"] == "directory":
            target.mkdir(mode=0o700)
        else:
            if item["kind"] == "sqlite":
                snapshot(source / name, target)
            else:
                shutil.copyfile(source / name, target)
            target.chmod(0o700 if item["executable"] else 0o600)
            with target.open("rb") as output:
                os.fsync(output.fileno())
    require(state_manifest(source, physical=True) == physical_before and state_manifest(source) == before,
            "Source changed during multi-database snapshot; discard copy and retry deliberately")
    require(state_manifest(destination) == before, "Complete copied logical state differs")
    for path in sorted([destination, *(x for x in destination.rglob("*") if x.is_dir())],
                       key=lambda x: len(x.parts), reverse=True):
        sync_directory(path)
    sync_directory(destination.parent)
    return before


def project_count(state):
    with private_database(state / "control.db") as db:
        return db.execute("SELECT count(*) FROM projects").fetchone()[0]


def backup_configuration(text, unit):
    original = tomllib.loads(text)
    expected = copy.deepcopy(original)
    for section, label, relative in (
        ("databases", "aviary-control", "control.db"),
        ("databases", "aviary-projects", "projects/*/data.db"),
        ("databases", "aviary-auxiliary", "projects/*/auxiliary.db"),
        ("files", "aviary-project-files", "projects/*"),
        ("files", "aviary-exports", "projects/*/backups/pb_backup*.zip"),
        ("files", "aviary-current-binary", None),
    ):
        old = str(LEGACY / "data" / relative) if relative else str(LEGACY / "aviary")
        new = str(DATA / relative) if relative else str(ROOT / "current/aviary")
        items = [item for item in expected.get(section, []) if item.get("label") == label]
        require(len(items) == 1 and items[0].get("path") == old
                and text.count(json.dumps(old)) == 1, "Backuper Aviary coverage differs: " + label)
        require(not any(item.get("path") == new for item in expected[section]), "Colliding backup source")
        items[0]["path"] = new
        text = text.replace(json.dumps(old), json.dumps(new), 1)
    for addition in (
        {"label": "aviary-configuration", "path": str(ENV)},
        {"label": "aviary-export-metadata", "path": str(DATA / "projects/*/backups/pb_backup*.zip.attrs"),
         "required": False},
    ):
        require(not any(item.get("label") == addition["label"] or item.get("path") == addition["path"]
                        for section in ("databases", "files") for item in expected.get(section, [])),
                "Additional Aviary backup already present: " + addition["label"])
        expected["files"].append(addition)
        text += '\n[[files]]\nlabel = ' + json.dumps(addition["label"]) + '\npath = ' + json.dumps(addition["path"]) + '\n'
        if addition.get("required") is False:
            text += "required = false\n"
    require(tomllib.loads(text) == expected, "Unexpected backup settings/secret changes")
    matches = list(re.finditer(r"(?m)^ReadWritePaths=([^\r\n]*)$", unit))
    require(len(matches) == 1, "Backuper must have one reviewed ReadWritePaths line")
    line = matches[0].group(1)
    tokens = shlex.split(line)
    for relative in ("", "/projects/backuper-info", "/projects/lgspkb"):
        old, new = str(LEGACY / "data") + relative, str(DATA) + relative
        require(tokens.count(old) == 1 and new not in tokens, "Backuper SQLite-parent permissions differ")
        pattern = r'(?<!\S)(?:"' + re.escape(old) + r'"|' + re.escape(old) + r')(?!\S)'
        line, count = re.subn(pattern, lambda _: json.dumps(new), line)
        require(count == 1, "Ambiguous Backuper permission token")
    unit = unit[:matches[0].start(1)] + line + unit[matches[0].end(1):]
    return text, unit


class NoRedirect(HTTPRedirectHandler):
    def redirect_request(self, *args):
        return None


def request(url, content_type=None):
    try:
        response = build_opener(ProxyHandler({}), NoRedirect()).open(
            Request(url, headers={"User-Agent": "AviaryDeploymentCheck/1.0"}), timeout=15)
    except HTTPError as error:
        response = error
    with response:
        require(content_type is None or response.headers.get_content_type() == content_type,
                "Response MIME mismatch: " + url)
        body = response.read(4 * 1024**2 + 1)
        require(len(body) <= 4 * 1024**2, "Oversized response: " + url)
        return response.status, body


def health(origin, revision, cron_enabled, projects):
    code, body = request(origin + "/_aviary/health", "application/json")
    require(code == 200, "Health endpoint unavailable")
    try:
        value = json.loads(body)
    except (ValueError, UnicodeDecodeError) as error:
        raise DeploymentError("Invalid health JSON") from error
    require(isinstance(value, dict) and value.get("status") == "ok"
            and value.get("revision") == revision and value.get("version") == VERSION
            and type(value.get("projects")) is int and value["projects"] >= 0
            and value["projects"] == projects and value.get("cronEnabled") is cron_enabled,
            "Health revision/version/inventory/mode mismatch")


def check_http(origin, ui):
    code, body = request(origin + "/", "text/html")
    require(code == 200 and body == ui and b"<html" in body.lower()
            and b"<script" in body.lower(), "Embedded control UI/assets mismatch")
    code, body = request(origin + "/api/auth/session", "application/json")
    try:
        session = json.loads(body)
    except (ValueError, UnicodeDecodeError) as error:
        raise DeploymentError("Invalid control session JSON") from error
    require(code == 200 and isinstance(session, dict)
            and session.get("authenticated") is False and session.get("configured") is True
            and session.get("email") == "" and session.get("role") == ""
            and type(session.get("hasPasskeys")) is bool
            and type(session.get("passwordLoginDisabled")) is bool,
            "Control session/bootstrap/authentication contract mismatch")
    code, body = request(origin + "/api/projects", "application/json")
    try:
        denied = json.loads(body)
    except (ValueError, UnicodeDecodeError) as error:
        raise DeploymentError("Invalid control authorization JSON") from error
    require(code == 401 and isinstance(denied, dict)
            and denied.get("code") == 401 and denied.get("error") == "authentication required",
            "Unauthenticated project inventory must remain protected")


def validate_source(source):
    for name in ("go.mod", "go.sum", "main.go", "runtime.go", "deploy/aviary.service",
                 "deploy/vendor.json", "scripts/deploy_vps.py", "scripts/test_deploy_vps.py",
                 "internal/aviary/ui.go", "internal/aviary/web/index.html"):
        safe_path(source / name, "file")
        require((source / name).stat().st_size > 0, "Empty committed source asset: " + name)
    require("//go:embed web/index.html" in (source / "internal/aviary/ui.go").read_text()
            and b"<script" in (source / "internal/aviary/web/index.html").read_bytes(),
            "Aviary control assets must be committed and embedded, not generated")
    require(json.loads((source / "deploy/vendor.json").read_text()) == VENDOR,
            "Pinned vendor metadata changed; separate review required")


def validate_unit(text):
    values = {}
    section = None
    for line in text.splitlines():
        if not line.strip() or line.lstrip().startswith("#"):
            continue
        if line.startswith("[") and line.endswith("]"):
            section = line[1:-1]
            continue
        require(section in ("Unit", "Service", "Install") and "=" in line,
                "Unexpected unit syntax")
        key, value = line.split("=", 1)
        values.setdefault((section, key), []).append(value)
    expected = {
        ("Unit", "RequiresMountsFor"): "/mnt/volume-hel1-1",
        ("Service", "Type"): "exec", ("Service", "User"): "aviary", ("Service", "Group"): "aviary",
        ("Service", "WorkingDirectory"): "/opt/aviary/current",
        ("Service", "EnvironmentFile"): "/etc/aviary/aviary.env",
        ("Service", "ExecStart"): "/opt/aviary/current/aviary",
        ("Service", "TimeoutStopSec"): "660", ("Service", "KillMode"): "mixed",
        ("Service", "SendSIGKILL"): "no", ("Service", "UMask"): "0077",
        ("Service", "ProtectSystem"): "strict", ("Service", "ProtectHome"): "true",
        ("Service", "PrivateTmp"): "true", ("Service", "NoNewPrivileges"): "true",
        ("Service", "ReadWritePaths"): "/mnt/volume-hel1-1/aviary-state",
        ("Service", "Restart"): "on-failure", ("Install", "WantedBy"): "multi-user.target",
    }
    require(all(values.get(key) == [value] for key, value in expected.items()),
            "Unit differs from reviewed mounted-volume/drain/security contract")
    require(values.get(("Service", "ExecStartPre")) == [
        "/usr/bin/test -s /mnt/volume-hel1-1/aviary-state/control.db",
        "/usr/bin/test -d /mnt/volume-hel1-1/aviary-state/projects"],
        "Unit must require existing control and project state")
    require(not any(key.startswith(("Exec", "Environment", "Root", "Bind", "ReadWrite", "ReadOnly"))
                    and (section, key) not in expected and key != "ExecStartPre"
                    for section, key in values),
            "Additional unit commands/configuration/path overrides require review")


def validate_binary(path):
    safe_path(path, "file")
    with path.open("rb") as source:
        header = source.read(20)
    require(len(header) == 20 and header[:7] == b"\x7fELF\x02\x01\x01"
            and header[18:20] == b"\x3e\x00" and bool(path.stat().st_mode & 0o111),
            "Managed binary must be executable native Linux amd64 ELF")


def release_files(release):
    safe_path(release, "directory")
    result = {}
    for path in release.rglob("*"):
        require(path.is_file() and not path.is_symlink() and path.stat().st_nlink == 1,
                "Release must contain only regular, unlinked files")
        if path.name != "release.json":
            result[path.relative_to(release).as_posix()] = checksum(path)
    require(set(result) == PAYLOAD_FILES, "Unexpected or missing release files")
    return result


def process_identity(pid):
    require(type(pid) is int and pid > 1, "Invalid process ID")
    path = Path("/proc") / str(pid)
    fields = (path / "stat").read_text().rsplit(") ", 1)[1].split()
    return {"pid": pid, "parent": int(fields[1]), "start": fields[19],
            "exe": os.readlink(path / "exe"), "cwd": os.readlink(path / "cwd"),
            "argv": [base64.b64encode(x).decode("ascii")
                     for x in (path / "cmdline").read_bytes().split(b"\0")[:-1]],
            "sha256": checksum(path / "exe")}


def stop_exact(expected, parent):
    require(hasattr(os, "pidfd_open") and hasattr(signal, "pidfd_send_signal"),
            "Safe initial stop requires Linux pidfd support")
    descriptor = os.pidfd_open(expected["pid"])
    try:
        require(process_identity(expected["pid"]) == expected and process_identity(parent["pid"]) == parent,
                "Legacy PID/starttime/executable/argv/parent/hash changed before stop")
        signal.pidfd_send_signal(descriptor, signal.SIGTERM)
        deadline = time.monotonic() + 300
        while True:
            try:
                fields = Path("/proc", str(expected["pid"]), "stat").read_text().rsplit(") ", 1)[1].split()
            except FileNotFoundError:
                return
            require(fields[19] == expected["start"], "Legacy PID reused")
            if fields[0] == "Z":
                return
            require(time.monotonic() < deadline, "Legacy stop unconfirmed; never force kill")
            time.sleep(0.1)
    finally:
        os.close(descriptor)


def owned_connections(pid, proc_root=Path("/proc")):
    path = proc_root / str(pid)
    sockets = set()
    for fd in (path / "fd").iterdir():
        try:
            value = os.readlink(fd)
        except FileNotFoundError:
            continue
        match = re.fullmatch(r"socket:\[(\d+)\]", value)
        if match:
            sockets.add(match[1])
    connections = []
    for protocol in ("tcp", "tcp6", "udp", "udp6"):
        for line in (path / "net" / protocol).read_text().splitlines()[1:]:
            columns = line.split()
            require(len(columns) >= 10, "Cannot inspect legacy network sockets")
            if columns[9] in sockets:
                # TCP listeners are harmless; every other TCP state and every UDP socket is busy.
                if not protocol.startswith("tcp") or columns[3] != "0A":
                    connections.append((protocol, columns[1], columns[2], columns[3]))
    return connections


def validate_writer_proof(proof, legacy, manifest, now):
    require(isinstance(proof, dict) and proof.get("format") == 1
            and proof.get("legacy") == legacy
            and proof.get("state_manifest") == manifest,
            "Writer review is not bound to this exact legacy process/state")
    begin, end = proof.get("not_before"), proof.get("not_after")
    require(type(begin) in (int, float) and type(end) in (int, float)
            and begin <= now and end >= now + 900 and end - begin <= 3600,
            "Writer review must cover cutover plus 900 seconds, at most one hour")
    require(proof.get("no_pending_async_work") is True
            and proof.get("scheduled_writers_excluded") is True
            and isinstance(proof.get("reviewed_schedules"), list)
            and all(isinstance(x, str) and x.strip() for x in proof["reviewed_schedules"])
            and len(proof["reviewed_schedules"]) >= 3,
            "Review pending SMTP/work and built-in backup, optimization and log schedules explicitly")


def maintenance_configuration(text):
    pattern = r"(?m)^([ \t]*)reverse_proxy :8090[ \t]*(\r?)$"
    matches = list(re.finditer(pattern, text))
    require(len(matches) == 1 and text[:matches[0].start()].count("\n") + 1 == 75,
            "Aviary upstream is not the single reviewed line 75")
    result = re.sub(pattern, lambda m: m[1] + 'respond "Aviary is temporarily unavailable." 503' + m[2], text)
    require("8090" not in result, "Another Caddy route/comment references Aviary; review first")
    return result


@contextmanager
def deployment_locks(root=ROOT, backup_lock=Path("/var/lib/backuper/job.lock")):
    safe_path(backup_lock, "file")
    with backup_lock.open("rb") as backup:
        require(os.fstat(backup.fileno()).st_ino == backup_lock.stat().st_ino,
                "Backuper lock was replaced")
        try:
            fcntl.flock(backup, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError as error:
            raise DeploymentError("Backuper is busy; no deployment started") from error
        safe_path(root)
        existed = root.exists()
        root.mkdir(mode=0o755, parents=True, exist_ok=True)
        if existed:
            require(stat.S_IMODE(root.stat().st_mode) == 0o755, "Operator changed deploy-root permissions")
        else:
            root.chmod(0o755)
        safe_path(root / ".deploy.lock")
        fd = os.open(root / ".deploy.lock", os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW, 0o600)
        with os.fdopen(fd, "rb") as application:
            require(stat.S_ISREG(os.fstat(application.fileno()).st_mode), "Invalid deploy lock")
            try:
                fcntl.flock(application, fcntl.LOCK_EX | fcntl.LOCK_NB)
            except BlockingIOError as error:
                raise DeploymentError("Another Aviary deployment is busy") from error
            yield
            require(os.fstat(backup.fileno()).st_ino == backup_lock.stat().st_ino,
                    "Backuper lock inode changed during deployment")


def file_record(path):
    safe_path(path)
    if not path.exists():
        return None
    safe_path(path, "file")
    info = path.stat()
    return {"body": base64.b64encode(path.read_bytes()).decode("ascii"),
            "mode": stat.S_IMODE(info.st_mode), "uid": info.st_uid, "gid": info.st_gid}


def restore_record(path, record):
    if record is None:
        safe_path(path)
        path.unlink(missing_ok=True)
        sync_directory(path.parent)
    else:
        private_write(path, base64.b64decode(record["body"], validate=True), record["mode"],
                      (record["uid"], record["gid"]))


class Installer:
    def __init__(self, stage, system_root=Path("/")):
        self.stage = stage
        self.system_root = system_root
        self.root = system_root / ROOT.relative_to("/")
        self.data = system_root / DATA.relative_to("/")
        self.legacy = system_root / LEGACY.relative_to("/")
        self.env = system_root / ENV.relative_to("/")
        self.current = self.root / "current"
        self.pending = self.root / "deployment-pending.json"
        self.unit = system_root / "etc/systemd/system/aviary.service"
        self.backup_config = system_root / "etc/backuper/config.toml"
        self.backup_unit = system_root / "etc/systemd/system/backuper.service"
        self.caddy = system_root / "etc/caddy/Caddyfile"
        self.transaction = None
        self.maintenance_records = None

    def run(self, arguments, timeout=180):
        result = subprocess.run(arguments, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=timeout)
        with (self.stage / "install.private.log").open("ab") as log:
            log.write(result.stdout + result.stderr)
        require(result.returncode == 0, arguments[0] + " failed; inspect private receipt")
        return result.stdout.decode("utf-8").strip()

    def check_platform(self):
        require(os.geteuid() == 0 and platform.system() == "Linux" and platform.machine() == "x86_64"
                and os.path.ismount(self.data.parent), "Root, native Linux amd64 and mounted volume required")
        safe_path(self.stage, "directory")
        require(self.stage.stat().st_uid == 0 and stat.S_IMODE(self.stage.stat().st_mode) == 0o700,
                "Receipt must be durable root-private")

    def record(self, status, **values):
        path = self.stage / "deployment.json"
        result = json.loads(path.read_text()) if path.exists() else {}
        result.update(status=status, **values)
        write_json(path, result)

    def watched(self):
        return (self.env, self.unit, self.backup_config, self.backup_unit)

    def configuration_records(self):
        result = {str(path): file_record(path) for path in self.watched()}
        safe_path(self.env.parent, "directory")
        info = self.env.parent.stat()
        result[str(self.env.parent)] = {"directory": True, "mode": stat.S_IMODE(info.st_mode),
                                       "uid": info.st_uid, "gid": info.st_gid}
        return result

    def state_attributes(self):
        return {path.relative_to(self.data).as_posix(): {
            "mode": stat.S_IMODE(path.stat().st_mode), "uid": path.stat().st_uid, "gid": path.stat().st_gid}
            for path in [self.data, *self.data.rglob("*")]
            if not path.name.endswith(("-wal", "-shm", "-journal"))}

    def check_dropins(self):
        for service in ("aviary.service", "backuper.service"):
            require(not self.run(["systemctl", "show", service, "-p", "DropInPaths", "--value"]),
                    "Operator systemd drop-ins require separate review: " + service)
            for parent in ("etc/systemd/system", "run/systemd/system", "usr/lib/systemd/system"):
                folder = self.system_root / parent / (service + ".d")
                safe_path(folder)
                require(not folder.exists() or not any(folder.iterdir()), "Unloaded systemd drop-ins found")
            unit = self.unit if service == "aviary.service" else self.backup_unit
            if unit.exists():
                fragment = self.run(["systemctl", "show", service, "-p", "FragmentPath", "--value"])
                require(fragment == str(unit) or (service == "aviary.service" and not fragment
                                                  and not self.current.exists()),
                        "Systemd loaded another unit fragment: " + service)

    def switch(self, release):
        require(not self.current.exists() or self.current.is_symlink(), "Current is not a release symlink")
        link = self.root / (".current-" + uuid.uuid4().hex)
        link.symlink_to(release)
        link.replace(self.current)
        sync_directory(self.root)

    def stop(self, service="aviary.service"):
        require(self.run(["systemctl", "show", service, "-p", "SendSIGKILL", "--value"]) == "no"
                and self.run(["systemctl", "show", service, "-p", "KillMode", "--value"]) == "mixed"
                and self.run(["systemctl", "show", service, "-p", "KillSignal", "--value"]) in ("15", "SIGTERM"),
                "Loaded service stop contract changed; no potentially forced stop attempted")
        self.run(["systemctl", "stop", service], timeout=680)
        require(self.run(["systemctl", "show", service, "-p", "MainPID", "--value"]) == "0",
                "Stop unconfirmed; preserve pending receipt and all state")
        group = self.run(["systemctl", "show", service, "-p", "ControlGroup", "--value"])
        if group:
            require(group.startswith("/") and ".." not in PurePosixPath(group).parts, "Unsafe service cgroup")
            members = self.system_root / "sys/fs/cgroup" / group.lstrip("/") / "cgroup.procs"
            files = [members, *members.parent.rglob("cgroup.procs")] if members.parent.exists() else []
            require(all(not path.exists() or not path.read_text().strip() for path in files),
                    "Service descendants remain; refusing snapshot/switch")

    def permissions(self, directory):
        state_entries(directory)
        for path in [directory, *directory.rglob("*")]:
            require(not path.is_symlink(), "Redirected state during permission change")
            executable = bool(path.stat().st_mode & 0o111)
            os.chown(path, self.user.pw_uid, self.user.pw_gid)
            path.chmod(0o700 if path.is_dir() or executable else 0o600)

    def wait_health(self, origin, revision, cron_enabled, projects):
        deadline = time.monotonic() + 45
        last_error = None
        while True:
            try:
                health(origin, revision, cron_enabled, projects)
                return
            except (URLError, TimeoutError, ConnectionError, DeploymentError) as error:
                last_error = error
            require(time.monotonic() < deadline, "Health timeout: " + str(last_error))
            time.sleep(0.25)

    def check_process(self, release):
        require(self.run(["systemctl", "is-active", "aviary.service"]) == "active", "Aviary inactive")
        pid = int(self.run(["systemctl", "show", "aviary.service", "-p", "MainPID", "--value"]))
        require(pid > 1 and Path("/proc", str(pid), "exe").resolve() == release / "aviary"
                and Path("/proc", str(pid)).stat().st_uid == self.user.pw_uid,
                "Unexpected running Aviary executable/user")
        process = process_identity(pid)
        require(process["argv"] == [base64.b64encode(b"/opt/aviary/current/aviary").decode("ascii")]
                and process["cwd"] == str(release) and process["sha256"] == checksum(release / "aviary"),
                "Unexpected running Aviary arguments/directory/hash")
        runtime_env = {x for x in Path("/proc", str(pid), "environ").read_bytes().split(b"\0")
                       if x.startswith(b"AVIARY_")}
        require(runtime_env == set(CONFIGURATION.splitlines()), "Running environment differs from managed defaults")
        listeners = self.run(["ss", "-H", "-ltnp", "sport = :8090"]).splitlines()
        require(len(listeners) == 1 and "127.0.0.1:8090" in listeners[0]
                and f"pid={pid}," in listeners[0], "Aviary listener must be owned loopback only")
        revision = (release / "revision.txt").read_text().strip()
        health("http://127.0.0.1:8090", revision, True, project_count(self.data))
        return pid

    def legacy_identity(self, metadata):
        app = process_identity(metadata["legacy_pid"])
        parent = process_identity(metadata["parent_pid"])
        require(app["exe"] == str(self.legacy / "aviary") and app["cwd"] == str(self.legacy)
                and app["argv"] == [base64.b64encode(b"./aviary").decode("ascii")]
                and app["parent"] == parent["pid"]
                and app["sha256"] == metadata["legacy_sha256"] == VENDOR["binary_sha256"],
                "Legacy identity differs from reviewed process")
        environment = Path("/proc", str(app["pid"]), "environ").read_bytes().split(b"\0")
        require(not any(x.startswith(b"AVIARY_") for x in environment), "Legacy AVIARY_* defaults changed")
        require(checksum(self.legacy / "aviary") == app["sha256"], "On-disk original binary changed")
        return {"app": app, "parent": parent}

    def legacy_quiet(self, legacy, proof):
        source = self.legacy / "data"
        baseline = state_manifest(source)
        validate_writer_proof(proof, legacy, baseline, time.time())
        with private_database(source / "control.db") as db:
            require(db.execute("SELECT count(*) FROM cron_jobs").fetchone()[0] == 0,
                    "Initial migration requires zero control cron jobs")
        require(not any(path.is_file() for path in (source / "projects").glob("*/pb_hooks/**/*")),
                "Project JS hooks require separate writer review")
        deadline, quiet_since = time.monotonic() + 360, None
        while True:
            require(process_identity(legacy["app"]["pid"]) == legacy["app"]
                    and process_identity(legacy["parent"]["pid"]) == legacy["parent"],
                    "Legacy identity changed during maintenance")
            current = state_manifest(source)
            idle = not owned_connections(legacy["app"]["pid"])
            require(current == baseline, "Legacy wrote after writer review; preserve original and re-review")
            now = time.monotonic()
            quiet_since = (quiet_since if quiet_since is not None else now) if idle else None
            if quiet_since is not None and now - quiet_since >= QUIET_SECONDS:
                validate_writer_proof(proof, legacy, current, time.time())
                require(not owned_connections(legacy["app"]["pid"]), "Legacy network work resumed")
                return baseline
            require(now < deadline, "All-process TCP/UDP and delayed log work did not quiesce")
            time.sleep(1)

    def caddy_live(self):
        code, value = request("http://127.0.0.1:2019/config/", "application/json")
        require(code == 200, "Cannot read Caddy live configuration")
        return value

    @contextmanager
    def maintenance(self):
        original = file_record(self.caddy)
        require(original is not None, "Caddy configuration missing")
        original_body = base64.b64decode(original["body"])
        candidate = maintenance_configuration(original_body.decode("utf-8")).encode("utf-8")
        before_live = self.caddy_live()
        adapted = json.loads(self.run(["caddy", "adapt", "--config", str(self.caddy), "--adapter", "caddyfile"]))
        require(json.loads(before_live) == adapted, "Caddy disk/live configuration differs")
        private_write(self.stage / "Caddyfile.before", original_body)
        private_write(self.stage / "Caddy.live.before.json", before_live)
        candidate_path = self.stage / "Caddyfile.maintenance"
        private_write(candidate_path, candidate)
        self.run(["caddy", "validate", "--config", str(candidate_path), "--adapter", "caddyfile"])
        candidate_adapted = json.loads(self.run(
            ["caddy", "adapt", "--config", str(candidate_path), "--adapter", "caddyfile"]))
        installed = dict(original, body=base64.b64encode(candidate).decode("ascii"))
        self.maintenance_records = (original, installed, before_live, candidate_adapted)
        try:
            require(file_record(self.caddy) == original and self.caddy_live() == before_live,
                    "Operator changed Caddy before maintenance")
            restore_record(self.caddy, installed)
            self.run(["caddy", "reload", "--config", str(self.caddy), "--adapter", "caddyfile"])
            require(json.loads(self.caddy_live()) == candidate_adapted, "Maintenance live adaptation differs")
            self.record("maintenance")
            yield
        finally:
            require(file_record(self.caddy) in (original, installed)
                    and json.loads(self.caddy_live()) in (adapted, candidate_adapted),
                    "Operator changed Caddy; preserve edit and pending transaction, restore route manually")
            restore_record(self.caddy, original)
            self.run(["caddy", "reload", "--config", str(self.caddy), "--adapter", "caddyfile"])
            require(file_record(self.caddy) == original and self.caddy_live() == before_live,
                    "Caddy disk/live byte-for-byte restoration failed")

    def preflight(self, release, revision, source):
        parent = self.data.parent / ".aviary-deploy-checks"
        safe_path(parent)
        parent.mkdir(mode=0o711, exist_ok=True)
        parent.chmod(0o711)
        trial = Path(tempfile.mkdtemp(prefix="check-", dir=parent))
        name = "aviary-check-" + uuid.uuid4().hex + ".service"
        attempted = False
        stopped = False
        try:
            before = copy_state(source, trial / "state")
            self.permissions(trial / "state")
            os.chown(trial, self.user.pw_uid, self.user.pw_gid)
            trial.chmod(0o700)
            with socket.socket() as listener:
                listener.bind(("127.0.0.1", 0))
                port = listener.getsockname()[1]
            attempted = True
            self.run(["systemd-run", "--quiet", "--unit=" + name,
                      "-p", "Type=exec", "-p", "User=aviary", "-p", "Group=aviary",
                      "-p", "WorkingDirectory=" + str(release), "-p", "UMask=0077",
                      "-p", "ProtectSystem=strict", "-p", "ProtectHome=true",
                      "-p", "PrivateTmp=true", "-p", "PrivateDevices=true", "-p", "PrivateNetwork=true",
                      "-p", "UnsetEnvironment=" + UNSET_LEGACY_ENVIRONMENT,
                      "-p", "NoNewPrivileges=true", "-p", "ReadWritePaths=" + str(trial),
                      "-p", "IPAddressDeny=any", "-p", "IPAddressAllow=localhost",
                      "-p", "TimeoutStopSec=660", "-p", "KillMode=mixed", "-p", "SendSIGKILL=no",
                      "-p", "StandardOutput=append:" + str(self.stage / "preflight.private.log"),
                      "-p", "StandardError=append:" + str(self.stage / "preflight.private.log"),
                      str(release / "aviary"), "--addr", "127.0.0.1:" + str(port),
                      "--data", str(trial / "state"), "--idle-ttl", "5m", "--seed", "",
                      "--allow-dashboard-password=false", "--require-existing", "--disable-cron"])
            origin = "http://127.0.0.1:" + str(port)
            pid = int(self.run(["systemctl", "show", name, "-p", "MainPID", "--value"]))
            require(pid > 1, "Copied instance has no live MainPID")
            deadline = time.monotonic() + 45
            while True:
                try:
                    self.run(["nsenter", "--target", str(pid), "--net", "/usr/bin/python3", "-B",
                              str(self.stage / "deploy_vps.py"), "--probe", origin, revision,
                              str(project_count(trial / "state")), str(release / "control-ui.html")])
                    break
                except DeploymentError as error:
                    require(time.monotonic() < deadline, "Copied health/control probe failed: " + str(error))
                    require(self.run(["systemctl", "show", name, "-p", "MainPID", "--value"]) == str(pid),
                            "Copied instance exited/restarted during health probe")
                    time.sleep(0.25)
            require(self.run(["systemctl", "is-active", name]) == "active", "Copied instance exited")
            require(state_manifest(trial / "state") == before, "Copied startup changed complete state")
            self.stop(name)
            stopped = True
            require(state_manifest(trial / "state") == before, "Copied rehearsal changed complete state")
            self.record("preflight_passed", preflight_manifest=before)
        finally:
            if attempted and not stopped:
                try:
                    self.stop(name)
                except (DeploymentError, subprocess.TimeoutExpired) as error:
                    write_json(self.pending, {"receipt": str(self.stage), "phase": "copied_stop_unconfirmed",
                                              "unit": name, "copy": str(trial)})
                    self.record("needs_recovery", copied_unit=name, copied_state=str(trial / "state"))
                    raise DeploymentError("Copied stop unconfirmed; retain copy and pending receipt") from error
            shutil.rmtree(trial)

    def activate(self, release, revision, legacy, watched, proof=None):
        initial = legacy is not None
        tx = {"initial": initial, "previous": None if initial else str(self.current.resolve()),
              "release": str(release), "before": watched, "changed": {}, "stopped": False,
              "state_before": None, "legacy": legacy,
              "baseline_before": file_record(self.root / "deployed.json")}
        self.transaction = tx
        write_json(self.stage / "transaction.json", tx)
        write_json(self.pending, {"receipt": str(self.stage), "release": str(release), "initial": initial})
        try:
            self.check_dropins()
            require(self.configuration_records() == watched, "Configuration changed before stop")
            if initial:
                tx["legacy_before"] = self.legacy_quiet(legacy, proof)
                stop_exact(legacy["app"], legacy["parent"])
            else:
                self.stop()
            tx["stopped"] = True
            source = self.legacy / "data" if initial else self.data
            tx["state_before"] = copy_state(source, self.stage / "state.before")
            write_json(self.stage / "transaction.json", tx)
            require(not initial or tx["state_before"] == tx["legacy_before"],
                    "Legacy state changed across exact stop; writer guard was not sufficient")
            require(self.configuration_records() == watched, "Configuration changed while stopping")
            if initial:
                copy_state(self.stage / "state.before", self.data)
                config, unit = backup_configuration(
                    base64.b64decode(watched[str(self.backup_config)]["body"]).decode("utf-8"),
                    base64.b64decode(watched[str(self.backup_unit)]["body"]).decode("utf-8"))
                tx["changed"][str(self.env)] = base64.b64encode(CONFIGURATION).decode("ascii")
                tx["changed"][str(self.backup_config)] = base64.b64encode(config.encode("utf-8")).decode("ascii")
                tx["changed"][str(self.backup_unit)] = base64.b64encode(unit.encode("utf-8")).decode("ascii")
            tx["changed"][str(self.unit)] = base64.b64encode((release / "aviary.service").read_bytes()).decode("ascii")
            write_json(self.stage / "transaction.json", tx)
            require(state_manifest(self.data) == tx["state_before"], "Relocated stopped state differs")
            self.record("backed_up", state_manifest=tx["state_before"])
            for name, value in tx["changed"].items():
                path = Path(name)
                require(file_record(path) == watched[name], "Operator configuration changed before replacement")
                old = watched[name]
                mode = 0o640 if path == self.env else 0o644 if path in (self.unit, self.backup_unit) else old["mode"]
                owner = (0, self.user.pw_gid) if path == self.env else (old["uid"], old["gid"]) if old else (0, 0)
                private_write(path, base64.b64decode(value), mode, owner)
            tx["installed"] = self.configuration_records()
            write_json(self.stage / "transaction.json", tx)
            self.permissions(self.data)
            tx["state_attributes"] = self.state_attributes()
            write_json(self.stage / "transaction.json", tx)
            self.switch(release)
            self.run(["systemctl", "daemon-reload"])
            self.run(["systemd-analyze", "verify", str(self.unit)])
            tx["start_attempted"] = True
            write_json(self.stage / "transaction.json", tx)
            self.run(["systemctl", "start", "aviary.service"])
            self.wait_health("http://127.0.0.1:8090", revision, True, project_count(self.data))
            self.check_process(release)
            require(state_manifest(self.data) == tx["state_before"],
                    "Managed startup changed full logical state; forbid rollback if any change was accepted")
            check_http("http://127.0.0.1:8090", (release / "control-ui.html").read_bytes())
            self.run(["systemctl", "enable", "aviary.service"])
            self.record("healthy", revision=revision, release=str(release), relocation_verified=initial)
        except BaseException:
            self.recover()
            raise

    def recover(self):
        tx = self.transaction
        require(tx is not None, "No transaction for recovery")
        tx["recovery_attempted"] = True
        self.record("needs_recovery")
        if not tx["stopped"]:
            # Do not send another signal to an uncertain original or managed stop.
            if not tx["initial"]:
                self.run(["systemctl", "disable", "aviary.service"])
            raise DeploymentError("Stop unconfirmed; pending receipt and original/state retained")
        if tx.get("start_attempted"):
            self.run(["systemctl", "disable", "aviary.service"])
            self.stop()
        self.check_dropins()
        expected = tx["state_before"]
        require(expected is not None, "Stopped snapshot unconfirmed; manual recovery only")
        if self.data.exists():
            require(state_manifest(self.data) == expected,
                    "Accepted database/schema/file/new-project changes forbid rollback; stopped/disabled, no stale restore")
            require("state_attributes" not in tx or self.state_attributes() == tx["state_attributes"],
                    "Operator state permissions/ownership changes forbid rollback")
        current = self.configuration_records()
        installed = tx.get("installed", {})
        for name, original in tx["before"].items():
            allowed = [original]
            if name in installed:
                allowed.append(installed[name])
            elif name in tx["changed"]:
                # A partial configuration write is recognizable only with exact reviewed attributes.
                path = Path(name)
                old = original
                mode = 0o640 if path == self.env else 0o644 if path in (self.unit, self.backup_unit) else old["mode"]
                uid, gid = (0, self.user.pw_gid) if path == self.env else (old["uid"], old["gid"]) if old else (0, 0)
                allowed.append({"body": tx["changed"][name], "mode": mode, "uid": uid, "gid": gid})
            require(current[name] in allowed, "Operator configuration changes forbid rollback; stopped/disabled")
        require(file_record(self.root / "deployed.json") in
                (tx["baseline_before"], tx.get("baseline_installed", tx["baseline_before"])),
                "Operator deployment-baseline changes forbid rollback")
        if self.maintenance_records:
            original, candidate, live, adapted = self.maintenance_records
            require(file_record(self.caddy) in (original, candidate)
                    and (self.caddy_live() == live or json.loads(self.caddy_live()) == adapted),
                    "Operator Caddy changes forbid rollback")
        release = Path(tx["release"])
        self.assert_release(release)
        if tx["initial"]:
            require(state_manifest(self.legacy / "data") == expected
                    and checksum(self.legacy / "aviary") == VENDOR["binary_sha256"],
                    "Original state/artifact changed; no automatic legacy recovery")
        else:
            previous = Path(tx["previous"])
            self.assert_release(previous)
        require((self.current.is_symlink() and str(self.current.resolve()) in
                 {tx["release"], tx["previous"]})
                or (tx["initial"] and not self.current.exists() and not self.current.is_symlink()),
                "Operator changed current release; manual recovery only")
        for name in tx["changed"]:
            restore_record(Path(name), tx["before"][name])
        if "baseline_installed" in tx and file_record(self.root / "deployed.json") != tx["baseline_before"]:
            restore_record(self.root / "deployed.json", tx["baseline_before"])
        if tx["initial"]:
            if self.current.is_symlink():
                self.current.unlink()
                sync_directory(self.root)
        else:
            self.switch(Path(tx["previous"]))
        self.run(["systemctl", "daemon-reload"])
        if tx["initial"]:
            # Launch the exact intact artifact, not tmux, a git-pull wrapper or the parent shell.
            name = "aviary-legacy-recovery-" + uuid.uuid4().hex + ".service"
            self.run(["systemd-run", "--quiet", "--unit=" + name, "-p", "Type=exec",
                      "-p", "WorkingDirectory=" + str(self.legacy), "-p", "Restart=no",
                      "-p", "UnsetEnvironment=" + UNSET_LEGACY_ENVIRONMENT,
                      "-p", "UMask=0077", "-p", "SendSIGKILL=no",
                      str(self.legacy / "aviary")])
            deadline = time.monotonic() + 45
            while True:
                try:
                    check_http("http://127.0.0.1:8090", (release / "control-ui.html").read_bytes())
                    break
                except (URLError, TimeoutError, ConnectionError, DeploymentError) as error:
                    require(time.monotonic() < deadline, "Original recovery not healthy: " + str(error))
                    time.sleep(0.25)
            pid = int(self.run(["systemctl", "show", name, "-p", "MainPID", "--value"]))
            identity = process_identity(pid)
            require(pid > 1 and identity["sha256"] == VENDOR["binary_sha256"]
                    and identity["exe"] == str(self.legacy / "aviary")
                    and identity["cwd"] == str(self.legacy),
                    "Original recovery identity unconfirmed")
            self.record("rolled_back", recovery_unit=name)
        else:
            self.run(["systemctl", "start", "aviary.service"])
            self.check_process(Path(tx["previous"]))
            self.run(["systemctl", "enable", "aviary.service"])
            self.record("rolled_back")
        self.pending.unlink()
        sync_directory(self.root)

    def prepare_release(self, metadata):
        for name, key in (("release.tar", "archive_sha256"), ("source.tar", "source_sha256")):
            safe_path(self.stage / name, "file")
            require(checksum(self.stage / name) == metadata[key], "Uploaded archive checksum mismatch")
        source = self.stage / "source"
        source.mkdir(mode=0o700)
        extract(self.stage / "source.tar", source)
        validate_source(source)
        safe_path(self.stage / "deploy_vps.py", "file")
        require((self.stage / "deploy_vps.py").read_bytes() == (source / "scripts/deploy_vps.py").read_bytes(),
                "Uploaded installer differs from committed source")
        unpacked = self.stage / "unpacked"
        unpacked.mkdir(mode=0o700)
        extract(self.stage / "release.tar", unpacked)
        manifest = json.loads((unpacked / "release.json").read_text())
        files = release_files(unpacked)
        revision = manifest.get("revision")
        require(isinstance(revision, str) and re.fullmatch("[0-9a-f]{40}", revision)
                and revision == metadata["revision"]
                and manifest.get("architecture") == "linux/amd64"
                and manifest.get("version") == VERSION and manifest.get("files") == files
                and (unpacked / "revision.txt").read_text() == revision + "\n",
                "Release manifest/revision/platform mismatch")
        build = json.loads((unpacked / "build.json").read_text())
        require(build == {"toolchain": TOOLCHAIN, "cgo": 0, "version": VERSION, "revision": revision,
                          "source_sha256": metadata["source_sha256"],
                          "go_mod_sha256": checksum(source / "go.mod"),
                          "go_sum_sha256": checksum(source / "go.sum")},
                "Release build/dependency provenance mismatch")
        for runtime, committed in (("aviary.service", "deploy/aviary.service"),
                                   ("vendor.json", "deploy/vendor.json"),
                                   ("control-ui.html", "internal/aviary/web/index.html")):
            require((unpacked / runtime).read_bytes() == (source / committed).read_bytes(),
                    "Release differs from committed source: " + runtime)
        validate_unit((unpacked / "aviary.service").read_text())
        validate_binary(unpacked / "aviary")
        verification = self.stage / "aviary-verify.service"
        private_write(verification, (unpacked / "aviary.service").read_text().replace(
            "/opt/aviary/current", str(unpacked)))
        self.run(["systemd-analyze", "verify", str(verification)])
        releases = safe_path(self.root / "releases")
        releases.mkdir(mode=0o755, exist_ok=True)
        releases.chmod(0o755)
        identity = hashlib.sha256(frame(manifest)).hexdigest()
        release = releases / (revision + "-" + identity[:16])
        if release.exists():
            require(release_files(release) == files
                    and (release / "release.json").read_bytes() == (unpacked / "release.json").read_bytes(),
                    "Existing immutable release differs")
        else:
            shutil.copytree(unpacked, release)
            for path in [release, *release.rglob("*")]:
                os.chown(path, 0, 0)
                path.chmod(0o755 if path.is_dir() else 0o555 if path.name == "aviary" else 0o444)
                if path.is_file():
                    with path.open("rb") as file:
                        os.fsync(file.fileno())
            sync_directory(release)
            sync_directory(releases)
        self.assert_release(release)
        return release, revision

    def assert_release(self, release):
        require(release.parent == self.root / "releases" and not release.is_symlink(),
                "Release is outside immutable release root")
        manifest = json.loads((release / "release.json").read_text())
        identity = hashlib.sha256(frame(manifest)).hexdigest()
        require(release_files(release) == manifest["files"]
                and release.name == manifest["revision"] + "-" + identity[:16]
                and (release / "revision.txt").read_text() == manifest["revision"] + "\n"
                and manifest["architecture"] == "linux/amd64" and manifest["version"] == VERSION,
                "Immutable release manifest/name/provenance changed")
        for path in [release, *release.rglob("*")]:
            info = path.stat()
            require(info.st_uid == 0 and info.st_gid == 0 and not info.st_mode & 0o022
                    and (path.is_dir() or not info.st_mode & 0o222),
                    "Release is not root-owned immutable")

    def install(self):
        self.check_platform()
        os.umask(0o077)
        for path in (self.root, self.data, self.env.parent, self.unit, self.backup_unit, self.backup_config):
            safe_path(path)
        with deployment_locks(self.root, self.system_root / "var/lib/backuper/job.lock"):
            safe_path(self.pending)
            require(not self.pending.exists(), "Pending transaction requires manual receipt review; never bypass")
            metadata = json.loads((self.stage / "request.json").read_text())
            initial = metadata["migrate_tmux"]
            require(type(initial) is bool and initial == (not self.current.exists()),
                    "Migration/update mode differs from current layout")
            self.check_dropins()
            legacy = None
            proof = None
            if initial:
                require(not any(path.exists() or path.is_symlink()
                                for path in (self.data, self.env, self.unit, self.current)),
                        "Partial managed layout requires manual recovery")
                legacy = self.legacy_identity(metadata)
                source = self.legacy / "data"
                backup_configuration(self.backup_config.read_text(), self.backup_unit.read_text())
                vendor_binary(self.stage / "official.zip")
                if metadata.get("writer_proof_sha256"):
                    require(checksum(self.stage / "writer-proof.json") == metadata["writer_proof_sha256"],
                            "Writer-review upload checksum differs")
                    proof = json.loads((self.stage / "writer-proof.json").read_text())
                if not metadata["rehearse"]:
                    validate_writer_proof(proof, legacy, state_manifest(source), time.time())
            else:
                require(self.current.is_symlink(), "Unknown current layout")
                previous = self.current.resolve()
                self.assert_release(previous)
                require(self.unit.read_bytes() == (previous / "aviary.service").read_bytes()
                        and self.env.read_bytes() == CONFIGURATION,
                        "Managed unit/environment differs from reviewed configuration")
                baseline = json.loads((self.root / "deployed.json").read_text())
                require(self.configuration_records() == baseline["configuration"]
                        and baseline["release"] == str(previous),
                        "Operator configuration changed since deployment; review explicitly, do not overwrite")
                source = self.data
            entries = state_entries(source)
            size = sum(path.stat().st_size for path in entries.values() if path.is_file())
            unpack_size = 0
            if (self.stage / "release.tar").exists() and (self.stage / "source.tar").exists():
                for name, multiplier in (("release.tar", 2), ("source.tar", 1)):
                    with tarfile.open(self.stage / name) as archive:
                        unpack_size += multiplier * sum(member.size for member in archive if member.isfile())
            require(shutil.disk_usage(self.root).free > size * 2 + unpack_size + 256 * 1024**2
                    and shutil.disk_usage(self.data.parent).free > size * 3 + 256 * 1024**2,
                    "Insufficient receipt/snapshot/relocation capacity")
            release, revision = self.prepare_release(metadata)
            try:
                self.user = pwd.getpwnam("aviary")
            except KeyError:
                self.run(["useradd", "--system", "--user-group", "--no-create-home",
                          "--home-dir", "/var/lib/aviary", "--shell", "/usr/sbin/nologin", "aviary"])
                self.user = pwd.getpwnam("aviary")
            require(self.user.pw_uid != 0 and self.user.pw_dir == "/var/lib/aviary"
                    and self.user.pw_shell == "/usr/sbin/nologin", "Unexpected Aviary service account")
            if initial:
                self.env.parent.mkdir(mode=0o750, exist_ok=True)
                os.chown(self.env.parent, 0, self.user.pw_gid)
                self.env.parent.chmod(0o750)
            else:
                require(self.configuration_records() == baseline["configuration"],
                        "Operator changed configuration/parent attributes during preparation")
            watched = self.configuration_records()
            if not initial:
                self.check_process(previous)
            if not initial and self.current.resolve() == release:
                pid = self.check_process(release)
                check_http(PUBLIC, (release / "control-ui.html").read_bytes())
                health(PUBLIC, revision, True, project_count(self.data))
                require(self.run(["systemctl", "is-enabled", "aviary.service"]) == "enabled",
                        "Identical release is not enabled")
                require(self.check_process(release) == pid, "Identical release restarted during verification")
                self.record("already_current", revision=revision, release=str(release), pid=pid)
                return
            self.preflight(release, revision, source)
            require(self.configuration_records() == watched, "Configuration changed during rehearsal")
            if metadata["rehearse"]:
                self.record("rehearsal_complete", revision=revision, release=str(release))
                return
            try:
                write_json(self.pending, {"receipt": str(self.stage), "release": str(release),
                                          "initial": initial, "phase": "maintenance_preparing"})
                with self.maintenance():
                    self.activate(release, revision, legacy, watched, proof)
                health(PUBLIC, revision, True, project_count(self.data))
                check_http(PUBLIC, (release / "control-ui.html").read_bytes())
                self.check_dropins()
                require(self.configuration_records() == self.transaction["installed"],
                        "Operator changed configuration during public verification")
                self.assert_release(release)
                self.check_process(release)
                self.record("verified_public", configuration=self.configuration_records())
                baseline = self.root / "deployed.json"
                require(file_record(baseline) == self.transaction["baseline_before"],
                        "Operator changed deployment baseline during transaction")
                body = (self.stage / "deployment.json").read_bytes()
                self.transaction["baseline_installed"] = {
                    "body": base64.b64encode(body).decode("ascii"), "mode": 0o600,
                    "uid": os.geteuid(), "gid": self.root.stat().st_gid,
                }
                write_json(self.stage / "transaction.json", self.transaction)
                private_write(baseline, body)
                self.pending.unlink()
                sync_directory(self.root)
            except BaseException:
                if not self.pending.exists() and self.maintenance_records:
                    original, _, live, _ = self.maintenance_records
                    if file_record(self.caddy) != original or self.caddy_live() != live:
                        write_json(self.pending, {"receipt": str(self.stage), "phase": "caddy_recovery"})
                if self.transaction and self.pending.exists() and not self.transaction.get("recovery_attempted"):
                    self.recover()
                raise


def ssh(host, arguments):
    subprocess.run(["ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=15",
                    "-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=3",
                    host, shlex.join(arguments)], check=True)


def build_source(repository, directory):
    def git(*arguments):
        return subprocess.check_output(["git", "-C", str(repository), *arguments], text=True).strip()

    require(not git("status", "--porcelain", "--untracked-files=no"),
            "Tracked source is dirty; commit reviewed source before deployment (untracked artifacts are ignored)")
    revision = git("rev-parse", "HEAD")
    require(re.fullmatch("[0-9a-f]{40}", revision), "Invalid committed revision")
    require(platform.system() == "Linux" and platform.machine() == "x86_64", "Native Linux amd64 required")
    source = directory / "source"
    source.mkdir()
    archive = directory / "source.tar"
    subprocess.run(["git", "-C", str(repository), "archive", "-o", str(archive), revision], check=True)
    extract(archive, source)
    validate_source(source)
    for name in ("go.mod", "go.sum"):
        require((source / name).read_bytes() == subprocess.check_output(
            ["git", "-C", str(repository), "show", VENDOR["source_revision"] + ":" + name]),
            "Dependency versions differ from approved vendor source: " + name)
    dependencies = {name: checksum(source / name) for name in ("go.mod", "go.sum")}
    environment = dict(os.environ, GOTOOLCHAIN=TOOLCHAIN, GOOS="linux", GOARCH="amd64",
                       GOAMD64="v1", GOEXPERIMENT="", GOENV="off", GOFLAGS="", GOWORK="off", CGO_ENABLED="0")
    require(subprocess.check_output(["go", "version"], env=environment, text=True).strip()
            == "go version go1.25.0 linux/amd64", "Use pinned native Go1.25.0")
    subprocess.run(["go", "test", "-mod=readonly", "-count=1", "-timeout=5m", "./..."],
                   cwd=source, env=environment, check=True)
    subprocess.run(["go", "test", "-mod=readonly", "-race", "-count=1", "-timeout=5m", "./..."],
                   cwd=source, env=dict(environment, CGO_ENABLED="1"), check=True)
    subprocess.run(["go", "vet", "-mod=readonly", "./..."], cwd=source, env=environment, check=True)
    subprocess.run([sys.executable, "-B", "-m", "unittest", "discover", "-s", "scripts",
                    "-p", "test_deploy_vps.py"], cwd=source, check=True)
    payload = directory / "payload"
    payload.mkdir()
    subprocess.run(["go", "build", "-mod=readonly", "-trimpath", "-buildvcs=false",
                    "-ldflags", "-X main.version=" + VERSION + " -X main.buildRevision=" + revision,
                    "-o", str(payload / "aviary"), "."], cwd=source, env=environment, check=True)
    validate_binary(payload / "aviary")
    validate_unit((source / "deploy/aviary.service").read_text())
    require({name: checksum(source / name) for name in dependencies} == dependencies,
            "Validation/build changed pinned dependency manifests")
    require(git("rev-parse", "HEAD") == revision and not git("status", "--porcelain", "--untracked-files=no"),
            "Tracked source changed during validation")
    for target, name in (("aviary.service", "deploy/aviary.service"), ("vendor.json", "deploy/vendor.json"),
                         ("control-ui.html", "internal/aviary/web/index.html")):
        shutil.copyfile(source / name, payload / target)
    private_write(payload / "revision.txt", revision + "\n")
    write_json(payload / "build.json", {"toolchain": TOOLCHAIN, "cgo": 0, "version": VERSION,
                                       "revision": revision, "source_sha256": checksum(archive),
                                       "go_mod_sha256": dependencies["go.mod"], "go_sum_sha256": dependencies["go.sum"]})
    write_json(payload / "release.json", {"revision": revision, "architecture": "linux/amd64",
                                         "version": VERSION, "files": release_files(payload)})
    release_archive = directory / "release.tar"
    with tarfile.open(release_archive, "w") as output:
        for path in sorted(payload.iterdir()):
            output.add(path, arcname=path.name, recursive=False)
    return revision


def deploy(args):
    require(re.fullmatch("[A-Za-z0-9_][A-Za-z0-9_.@-]*", args.host), "Invalid SSH alias")
    require(args.yes, "Use --yes after reviewing the guarded deployment")
    if args.migrate_tmux:
        require(type(args.legacy_pid) is int and args.legacy_pid > 1
                and type(args.parent_pid) is int and args.parent_pid > 1
                and args.parent_pid != args.legacy_pid and args.legacy_sha256 == VENDOR["binary_sha256"],
                "Initial migration requires exact --legacy-pid, --parent-pid and official --legacy-sha256")
        require(args.rehearse or args.legacy_writer_proof,
                "Initial activation requires --legacy-writer-proof; quiet sockets alone cannot prove stripped legacy drain")
    else:
        require(not any((args.legacy_pid, args.parent_pid, args.legacy_sha256,
                         args.legacy_writer_proof, args.vendor_archive)),
                "Legacy arguments are only valid with --migrate-tmux")
    repository = Path(__file__).resolve().parents[1]
    with tempfile.TemporaryDirectory(prefix="aviary-deploy-") as temporary:
        directory = Path(temporary)
        revision = build_source(repository, directory)
        uploads = [directory / "release.tar", directory / "source.tar",
                   directory / "source/scripts/deploy_vps.py"]
        proof_hash = None
        if args.migrate_tmux:
            obtain_vendor(directory / "official.zip", args.vendor_archive)
            uploads.append(directory / "official.zip")
            if args.legacy_writer_proof:
                safe_path(args.legacy_writer_proof, "file")
                shutil.copyfile(args.legacy_writer_proof, directory / "writer-proof.json")
                proof_hash = checksum(directory / "writer-proof.json")
                uploads.append(directory / "writer-proof.json")
        write_json(directory / "request.json", {
            "revision": revision, "archive_sha256": checksum(directory / "release.tar"),
            "source_sha256": checksum(directory / "source.tar"), "migrate_tmux": args.migrate_tmux,
            "rehearse": args.rehearse, "legacy_pid": args.legacy_pid, "parent_pid": args.parent_pid,
            "legacy_sha256": args.legacy_sha256, "writer_proof_sha256": proof_hash,
        })
        uploads.append(directory / "request.json")
        identifier = uuid.uuid4().hex
        stage = "/root/aviary-deploy-backups/update-" + identifier
        ssh(args.host, ["install", "-d", "-m", "0700", stage])
        print("Private durable receipt: " + stage, flush=True)
        subprocess.run(["scp", "-q", "-o", "BatchMode=yes", *map(str, uploads),
                        args.host + ":" + stage + "/"], check=True)
        ssh(args.host, ["systemd-run", "--quiet", "--wait", "--unit=aviary-deploy-" + identifier,
                        "-p", "Type=oneshot", "-p", "TimeoutStartSec=infinity", "-p", "UMask=0077",
                        "-p", "StandardOutput=append:" + stage + "/install.private.log",
                        "-p", "StandardError=append:" + stage + "/install.private.log",
                        "/usr/bin/python3", "-B", stage + "/deploy_vps.py", "--install", stage])
        print("Aviary operation completed; inspect " + stage + "/deployment.json", flush=True)


def parser():
    result = argparse.ArgumentParser(description=__doc__)
    result.add_argument("host", nargs="?", default="meadow-ubuntu-8gb-hel1-1")
    result.add_argument("--yes", action="store_true")
    result.add_argument("--rehearse", action="store_true")
    result.add_argument("--migrate-tmux", action="store_true")
    result.add_argument("--legacy-pid", type=int)
    result.add_argument("--parent-pid", type=int)
    result.add_argument("--legacy-sha256")
    result.add_argument("--vendor-archive", type=Path)
    result.add_argument("--legacy-writer-proof", type=Path)
    return result


def main():
    if sys.argv[1:2] == ["--probe"]:
        require(len(sys.argv) == 6 and re.fullmatch(r"http://127\.0\.0\.1:[1-9][0-9]{0,4}", sys.argv[2])
                and int(sys.argv[2].rsplit(":", 1)[1]) <= 65535
                and re.fullmatch("[0-9a-f]{40}", sys.argv[3]) and re.fullmatch(r"0|[1-9][0-9]*", sys.argv[4])
                and re.fullmatch(r"/opt/aviary/releases/[0-9a-f]{40}-[0-9a-f]{16}/control-ui\.html", sys.argv[5]),
                "Invalid namespace-local copied probe arguments")
        health(sys.argv[2], sys.argv[3], False, int(sys.argv[4]))
        check_http(sys.argv[2], safe_path(Path(sys.argv[5]), "file").read_bytes())
    elif sys.argv[1:2] == ["--install"]:
        require(len(sys.argv) == 3 and re.fullmatch("/root/aviary-deploy-backups/update-[0-9a-f]{32}", sys.argv[2]),
                "Invalid root-private receipt path")
        installer = Installer(Path(sys.argv[2]))
        try:
            installer.install()
        except BaseException:
            path = installer.stage / "deployment.json"
            previous = json.loads(path.read_text()) if path.exists() else {}
            if previous.get("status") != "rolled_back":
                installer.record("needs_recovery" if installer.pending.exists() else "failed",
                                 last_phase=previous.get("status"))
            raise
    else:
        deploy(parser().parse_args())


if __name__ == "__main__":
    try:
        main()
    except Exception as error:
        print("Deployment failed: " + str(error) + "; inspect private receipt before retrying", file=sys.stderr)
        raise SystemExit(1)

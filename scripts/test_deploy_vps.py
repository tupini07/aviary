from contextlib import closing
from http.server import BaseHTTPRequestHandler, HTTPServer
import copy
import hashlib
import io
import json
import os
from pathlib import Path
import shutil
import signal
import sqlite3
import subprocess
import tarfile
import tempfile
import threading
from types import SimpleNamespace
import unittest
from unittest.mock import patch
import zipfile

import deploy_vps as d


REPOSITORY = Path(__file__).resolve().parents[1]
REVISION = "a" * 40


def create_state(path):
    path.mkdir(parents=True)
    (path / "projects").mkdir()
    with closing(sqlite3.connect(path / "control.db")) as db:
        db.executescript("""
            CREATE TABLE projects(id TEXT PRIMARY KEY, name TEXT);
            INSERT INTO projects VALUES('lgspkb', 'retained');
            INSERT INTO projects VALUES('backuper-info', 'retained');
            CREATE TABLE kv(key TEXT PRIMARY KEY, value TEXT);
            INSERT INTO kv VALUES('session_key', '""" + "00" * 32 + """');
            CREATE TABLE cron_jobs(id TEXT PRIMARY KEY);
            CREATE TABLE legacy(id INTEGER PRIMARY KEY AUTOINCREMENT, value BLOB);
            INSERT INTO legacy(value) VALUES(x'00ff');
            INSERT INTO legacy(value) VALUES(CAST(x'80ff' AS TEXT));
            DELETE FROM legacy WHERE id=1;
            PRAGMA user_version=12;
            PRAGMA application_id=42;
        """)
        db.commit()
    for name in ("lgspkb", "backuper-info"):
        project = path / "projects" / name
        project.mkdir()
        for directory in ("uploads/a", "backups", "pb_public", "pb_hooks", "empty"):
            (project / directory).mkdir(parents=True)
        (project / "uploads/a/binary").write_bytes(b"\0\xff\x80")
        (project / "pb_public/index.html").write_bytes(b"<html>retained</html>")
        (project / "backups/pb_backup_old.zip").write_bytes(b"retained archive")
        for database in ("data.db", "auxiliary.db"):
            with closing(sqlite3.connect(project / database)) as db:
                db.executescript("""
                    CREATE TABLE "legacy ' collision"(id INTEGER PRIMARY KEY, value BLOB);
                    INSERT INTO "legacy ' collision" VALUES(5,x'80ff00');
                    CREATE TABLE "other" (value TEXT, extra BLOB);
                    INSERT INTO other VALUES(CAST(x'fffe' AS TEXT), x'ff00');
                    CREATE TABLE "without" (id TEXT PRIMARY KEY, value TEXT) WITHOUT ROWID;
                    INSERT INTO "without" VALUES('id','old');
                    CREATE VIEW v AS SELECT * FROM other;
                    CREATE INDEX idx ON other(extra);
                    CREATE TRIGGER retain AFTER INSERT ON other BEGIN SELECT 1; END;
                """)
                db.commit()


def backup_inputs():
    parts = ['[settings]\nsecret = "unchanged secret bytes"\n']
    for section, label, relative in (
        ("databases", "aviary-control", "control.db"),
        ("databases", "aviary-projects", "projects/*/data.db"),
        ("databases", "aviary-auxiliary", "projects/*/auxiliary.db"),
        ("files", "aviary-project-files", "projects/*"),
        ("files", "aviary-exports", "projects/*/backups/pb_backup*.zip"),
        ("files", "aviary-current-binary", None),
    ):
        path = str(d.LEGACY / "data" / relative) if relative else str(d.LEGACY / "aviary")
        parts.append(f'[[{section}]]\nlabel = "{label}"\npath = "{path}"\n')
        if label == "aviary-project-files":
            parts.append('exclude = ["*.db", "*-wal", "*-shm", "*-journal", "backups", ".notify"]\n')
        if label == "aviary-exports":
            parts.append("required = false\n")
    parts.append('[[files]]\nlabel = "unrelated"\npath = "/other"\n')
    unit = '[Service]\nUser=root\nReadWritePaths="/other" "' + str(d.LEGACY / "data") + '" "' + str(
        d.LEGACY / "data/projects/backuper-info") + '" "' + str(d.LEGACY / "data/projects/lgspkb") + '"\n'
    return "".join(parts), unit


def create_release(path, revision=REVISION):
    path.mkdir(parents=True)
    (path / "aviary").write_bytes(b"\x7fELF\x02\x01\x01" + b"\0" * 11 + b"\x3e\x00" + b"binary")
    (path / "aviary").chmod(0o700)
    (path / "aviary.service").write_bytes((REPOSITORY / "deploy/aviary.service").read_bytes())
    (path / "vendor.json").write_text(json.dumps(d.VENDOR))
    (path / "revision.txt").write_text(revision + "\n")
    (path / "control-ui.html").write_bytes((REPOSITORY / "internal/aviary/web/index.html").read_bytes())
    (path / "build.json").write_text("{}")
    d.write_json(path / "release.json", {
        "revision": revision, "architecture": "linux/amd64", "version": d.VERSION,
        "files": d.release_files(path),
    })


class TemporaryTest(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)


class StateTests(TemporaryTest):
    def test_private_reader_keeps_captured_snapshot_without_freezing_later_source_writes(self):
        source = self.root / "state"
        create_state(source)
        path = source / "control.db"
        with d.private_database(path) as captured:
            captured.text_factory = bytes
            before = captured.execute("SELECT * FROM legacy").fetchall()
            with closing(sqlite3.connect(path)) as writer:
                writer.execute("UPDATE legacy SET value='later source write'")
                writer.commit()
            self.assertEqual(captured.execute("SELECT * FROM legacy").fetchall(), before)
        with closing(sqlite3.connect(path)) as reader:
            self.assertTrue(any(row[0] == "later source write" for row in reader.execute("SELECT value FROM legacy")))

    def test_full_snapshot_includes_invalid_utf8_schema_sequences_blobs_and_all_files(self):
        source = self.root / "state"
        create_state(source)
        manifest = d.state_manifest(source)
        self.assertEqual(d.copy_state(source, self.root / "copy"), manifest)
        self.assertEqual(d.state_manifest(self.root / "copy"), manifest)
        self.assertIn("projects/lgspkb/empty", manifest)
        self.assertIn("projects/backuper-info/auxiliary.db", manifest)
        self.assertIn("projects/lgspkb/backups/pb_backup_old.zip", manifest)
        with closing(sqlite3.connect(self.root / "copy/control.db")) as db:
            db.text_factory = bytes
            self.assertEqual(db.execute("SELECT value FROM legacy").fetchone(), (b"\x80\xff",))
            self.assertEqual(db.execute("SELECT seq FROM sqlite_sequence").fetchone(), (2,))
            self.assertEqual(db.execute("PRAGMA user_version").fetchone(), (12,))
            self.assertEqual(db.execute("PRAGMA application_id").fetchone(), (42,))

    def test_wal_backup_includes_uncheckpointed_commits_and_journals_only_with_database(self):
        source = self.root / "state"
        create_state(source)
        with closing(sqlite3.connect(source / "control.db")) as writer:
            writer.execute("PRAGMA journal_mode=WAL")
            writer.execute("INSERT INTO legacy(value) VALUES('wal')")
            writer.commit()
            (source / "orphan.db-wal").write_bytes(b"must retain")
            before = d.state_manifest(source)
            self.assertNotIn("control.db-wal", before)
            self.assertIn("orphan.db-wal", before)
            self.assertEqual(d.copy_state(source, self.root / "copy"), before)
            with closing(sqlite3.connect(self.root / "copy/control.db")) as db:
                self.assertEqual(db.execute("SELECT count(*) FROM legacy").fetchone(), (2,))

    def test_closed_wal_databases_do_not_create_or_mutate_source_journals(self):
        source = self.root / "state"
        create_state(source)
        for path in source.rglob("*.db"):
            with closing(sqlite3.connect(path)) as db:
                db.execute("PRAGMA journal_mode=WAL")
        before = d.state_manifest(source, physical=True)
        self.assertFalse(any(name.endswith(("-wal", "-shm")) for name in before))
        d.copy_state(source, self.root / "copy")
        self.assertEqual(d.state_manifest(source, physical=True), before)
        self.assertEqual(d.state_manifest(source), d.state_manifest(self.root / "copy"))

    def test_schema_blob_implicit_rowid_sequence_and_new_project_changes_detected(self):
        source = self.root / "state"
        create_state(source)
        for statement in (
            "UPDATE legacy SET value=x'80ff00'",
            "ALTER TABLE legacy ADD COLUMN extra BLOB",
            "UPDATE sqlite_sequence SET seq=98",
        ):
            before = d.state_manifest(source)
            with closing(sqlite3.connect(source / "control.db")) as db:
                db.execute(statement)
                db.commit()
            self.assertNotEqual(d.state_manifest(source), before)
        before = d.state_manifest(source)
        with closing(sqlite3.connect(source / "projects/lgspkb/data.db")) as db:
            db.execute("UPDATE other SET rowid=100")
            db.commit()
        self.assertNotEqual(d.state_manifest(source), before)
        before = d.state_manifest(source)
        (source / "projects/new-project").mkdir()
        with closing(sqlite3.connect(source / "control.db")) as db:
            db.execute("INSERT INTO projects VALUES('new-project','provisioning')")
            db.commit()
        self.assertNotEqual(d.state_manifest(source), before)
        self.assertNotIn("projects/new-project/data.db", d.state_manifest(source))

    def test_missing_registry_directory_and_invalid_registry_names_fail_closed(self):
        source = self.root / "state"
        create_state(source)
        shutil.rmtree(source / "projects/lgspkb")
        with self.assertRaises(d.DeploymentError):
            d.state_manifest(source)
        (source / "projects/lgspkb").mkdir()
        with closing(sqlite3.connect(source / "control.db")) as db:
            db.execute("INSERT INTO projects VALUES('../escape','bad')")
            db.commit()
        with self.assertRaises(d.DeploymentError):
            d.state_manifest(source)

    def test_required_state_redirected_ancestors_symlinks_specials_and_hardlinks_refused(self):
        source = self.root / "state"
        create_state(source)
        redirect = self.root / "redirect"
        redirect.symlink_to(source, target_is_directory=True)
        with self.assertRaises(d.DeploymentError):
            d.state_manifest(redirect)
        bad = source / "projects/lgspkb/uploads/bad"
        for create in (lambda: bad.symlink_to(source / "control.db"),
                       lambda: os.mkfifo(bad), lambda: os.link(source / "control.db", bad)):
            create()
            with self.assertRaises(d.DeploymentError):
                d.state_manifest(source)
            bad.unlink()
        (source / "control.db").unlink()
        with self.assertRaises(d.DeploymentError):
            d.state_manifest(source)

    def test_live_copy_rejects_changes_even_when_logical_database_returns_to_old_value(self):
        source = self.root / "state"
        create_state(source)
        original = d.snapshot

        def changed_snapshot(incoming, target):
            original(incoming, target)
            if incoming.name == "control.db":
                with closing(sqlite3.connect(incoming)) as db:
                    db.execute("UPDATE kv SET value='temporary'")
                    db.commit()
                    db.execute("UPDATE kv SET value=?", ("00" * 32,))
                    db.commit()

        with patch.object(d, "snapshot", side_effect=changed_snapshot):
            with self.assertRaisesRegex(d.DeploymentError, "Source changed"):
                d.copy_state(source, self.root / "copy")

    def test_live_multi_database_copy_rejects_new_files_during_copy(self):
        source = self.root / "state"
        create_state(source)
        original = d.snapshot

        def changed_snapshot(incoming, target):
            original(incoming, target)
            (source / "projects/lgspkb/uploads/new").write_bytes(b"accepted")

        with patch.object(d, "snapshot", side_effect=changed_snapshot):
            with self.assertRaisesRegex(d.DeploymentError, "Source changed"):
                d.copy_state(source, self.root / "copy")

    def test_row_boundaries_names_and_duplicate_rows_do_not_collide(self):
        a, b = self.root / "a.db", self.root / "b.db"
        for path, rows in ((a, [("ab", "c"), ("ab", "c")]), (b, [("a", "bc")])):
            with closing(sqlite3.connect(path)) as db:
                db.execute('CREATE TABLE "quote""table"("rowid" TEXT, other TEXT)')
                db.executemany('INSERT INTO "quote""table" VALUES(?,?)', rows)
                db.commit()
        self.assertNotEqual(d.read_digest(a), d.read_digest(b))

    def test_invalid_utf8_text_and_equal_blob_are_distinct_and_schema_version_is_guarded(self):
        path = self.root / "types.db"
        with closing(sqlite3.connect(path)) as db:
            db.execute("CREATE TABLE data(value)")
            db.execute("INSERT INTO data VALUES(CAST(x'80ff' AS TEXT))")
            db.commit()
        before = d.read_digest(path)
        with closing(sqlite3.connect(path)) as db:
            db.execute("UPDATE data SET value=x'80ff'")
            db.commit()
        self.assertNotEqual(d.read_digest(path), before)
        before = d.read_digest(path)
        with closing(sqlite3.connect(path)) as db:
            db.execute("PRAGMA schema_version=99")
        self.assertNotEqual(d.read_digest(path), before)

    def test_arbitrary_upload_db_extension_is_preserved_and_known_database_directory_is_rejected(self):
        source = self.root / "state"
        create_state(source)
        (source / "projects/lgspkb/uploads/not-sqlite.db").write_bytes(b"binary upload")
        before = d.state_manifest(source)
        self.assertEqual(d.copy_state(source, self.root / "copy"), before)
        path = source / "projects/lgspkb/data.db"
        path.unlink()
        path.mkdir()
        with self.assertRaises(d.DeploymentError):
            d.state_manifest(source)

    def test_sqlite_database_cannot_be_silently_discarded_as_another_database_journal(self):
        source = self.root / "state"
        create_state(source)
        shutil.copyfile(source / "projects/lgspkb/data.db", source / "projects/lgspkb/data.db-wal")
        with self.assertRaises(d.DeploymentError):
            d.state_manifest(source)

    def test_uploaded_sqlite_and_its_sidecars_are_preserved_as_exact_file_bytes(self):
        source = self.root / "state"
        create_state(source)
        upload = source / "projects/lgspkb/uploads/upload.db"
        with closing(sqlite3.connect(upload)) as db:
            db.execute("CREATE TABLE uploaded(value)")
            db.execute("INSERT INTO uploaded VALUES ('preserve original database bytes')")
            db.commit()
            db.execute("PRAGMA schema_version=73")
        for suffix in ("-wal", "-shm", "-journal"):
            Path(str(upload) + suffix).write_bytes(b"uploaded sidecar " + suffix.encode())
        before = {path.name: path.read_bytes() for path in upload.parent.glob("upload.db*")}
        manifest = d.copy_state(source, self.root / "copy")
        copied = self.root / "copy/projects/lgspkb/uploads"
        self.assertEqual({path.name: path.read_bytes() for path in copied.glob("upload.db*")}, before)
        for name in before:
            self.assertEqual(manifest["projects/lgspkb/uploads/" + name]["kind"], "file")


class ArchiveAndConfigurationTests(TemporaryTest):
    def test_unsafe_archive_paths_links_duplicates_and_aliases_refused(self):
        for names in (("../bad",), ("/bad",), ("a/./b",), ("a//b",), ("a\\b",),
                      ("file", "file"), ("a", "a/b")):
            archive = self.root / "source.tar"
            with tarfile.open(archive, "w") as output:
                for name in names:
                    member = tarfile.TarInfo(name)
                    member.size = 1
                    output.addfile(member, io.BytesIO(b"x"))
            destination = self.root / ("out-" + str(len(list(self.root.iterdir()))))
            destination.mkdir()
            with self.assertRaises((d.DeploymentError, FileExistsError)):
                d.extract(archive, destination)
        for kind in (tarfile.SYMTYPE, tarfile.LNKTYPE, tarfile.FIFOTYPE):
            with tarfile.open(archive, "w") as output:
                member = tarfile.TarInfo("unsafe")
                member.type, member.linkname = kind, "/etc/passwd"
                output.addfile(member)
            with self.assertRaises(d.DeploymentError):
                d.extract(archive, self.root)

    def test_archive_cannot_write_through_preexisting_redirect(self):
        destination = self.root / "out"
        destination.mkdir()
        (destination / "redirect").symlink_to(self.root, target_is_directory=True)
        archive = self.root / "archive.tar"
        with tarfile.open(archive, "w") as output:
            member = tarfile.TarInfo("redirect/file")
            member.size = 1
            output.addfile(member, io.BytesIO(b"x"))
        with self.assertRaises(d.DeploymentError):
            d.extract(archive, destination)

    def test_backup_retarget_preserves_secret_bytes_options_other_routes_and_paths(self):
        text, unit = backup_inputs()
        config, permissions = d.backup_configuration(text, unit)
        self.assertEqual(config.count("aviary-configuration"), 1)
        metadata = [entry for entry in d.tomllib.loads(config)["files"]
                    if entry["label"] == "aviary-export-metadata"]
        self.assertEqual(metadata, [{"label": "aviary-export-metadata",
                                    "path": str(d.DATA / "projects/*/backups/pb_backup*.zip.attrs"),
                                    "required": False}])
        self.assertIn('secret = "unchanged secret bytes"', config)
        self.assertIn('exclude = ["*.db", "*-wal", "*-shm", "*-journal", "backups", ".notify"]', config)
        self.assertIn("required = false", config)
        self.assertIn('path = "/other"', config)
        self.assertIn('ReadWritePaths="/other"', permissions)
        self.assertNotIn(str(d.LEGACY), permissions)
        self.assertIn(str(d.DATA / "projects/lgspkb"), permissions)
        self.assertIn(str(d.ROOT / "current/aviary"), config)
        for broken in (text.replace("aviary-auxiliary", "other"),
                       text + '[[files]]\nlabel="aviary-configuration"\npath="/unexpected"\n',
                       text + '[[files]]\nlabel="aviary-export-metadata"\npath="/unexpected"\n',
                       text.replace('required = false', 'required = false\npath = "/duplicate"')):
            with self.assertRaises((d.DeploymentError, d.tomllib.TOMLDecodeError)):
                d.backup_configuration(broken, unit)
        with self.assertRaises(d.DeploymentError):
            d.backup_configuration(text, unit + 'ReadWritePaths="/other"\n')

    def test_maintenance_changes_only_exact_line_75(self):
        original = "unrelated configuration\n" * 74 + "\t reverse_proxy :8090\nother { reverse_proxy :9000 }\n"
        candidate = d.maintenance_configuration(original)
        self.assertEqual(original.splitlines()[:74], candidate.splitlines()[:74])
        self.assertEqual(original.splitlines()[75:], candidate.splitlines()[75:])
        self.assertIn("503", candidate.splitlines()[74])
        for broken in (original.replace(":8090", ":8091"),
                       "\n" + original, original + "extra { reverse_proxy :8090 }\n"):
            with self.assertRaises(d.DeploymentError):
                d.maintenance_configuration(broken)

    def test_caddy_candidate_normalizes_only_generated_file_server_config_hides(self):
        source, target = Path("/private/Caddyfile.maintenance"), Path("/etc/caddy/Caddyfile")
        original = {"routes": [
            {"handler": "file_server", "hide": [str(source), ".env", str(source) + ".other"]},
            {"handler": "subroute", "routes": [
                {"handler": "file_server", "hide": [str(source)]},
                {"handler": "static_response", "body": str(source), "hide": [str(source)]},
            ]},
        ]}
        expected = copy.deepcopy(original)
        expected["routes"][0]["hide"][0] = str(target)
        expected["routes"][1]["routes"][0]["hide"][0] = str(target)
        self.assertEqual(d.canonical_caddy_adaptation(original, source, target), expected)
        self.assertEqual(original["routes"][0]["hide"][0], str(source))

    def test_backup_lock_is_opened_readonly_before_deploy_lock_without_inode_or_mode_changes(self):
        lock = self.root / "job.lock"
        lock.write_bytes(b"retained lock")
        lock.chmod(0o640)
        before = lock.stat()
        root = self.root / "application"
        calls = []
        original = d.fcntl.flock

        def record_flock(file, mode):
            calls.append(Path(os.readlink("/proc/self/fd/" + str(file.fileno()))))
            if len(calls) == 1:
                self.assertFalse(root.exists())
            return original(file, mode)

        with patch.object(d.fcntl, "flock", side_effect=record_flock):
            with d.deployment_locks(root, lock):
                self.assertEqual(calls, [lock, root / ".deploy.lock"])
        self.assertEqual(lock.stat().st_ino, before.st_ino)
        self.assertEqual(lock.stat().st_mode, before.st_mode)
        self.assertEqual(lock.read_bytes(), b"retained lock")

    def test_busy_backup_lock_refuses_before_creating_application_directory(self):
        lock = self.root / "job.lock"
        lock.touch()
        with lock.open("rb") as holder:
            d.fcntl.flock(holder, d.fcntl.LOCK_EX | d.fcntl.LOCK_NB)
            with self.assertRaisesRegex(d.DeploymentError, "Backuper is busy"):
                with d.deployment_locks(self.root / "application", lock):
                    self.fail("Lock contention was ignored")
        self.assertFalse((self.root / "application").exists())

    def test_official_archive_requires_pinned_hash_and_rejects_unsafe_redirects(self):
        archive = self.root / "official.zip"
        with zipfile.ZipFile(archive, "w") as output:
            output.writestr("aviary", b"official")
        with self.assertRaises(d.DeploymentError):
            d.vendor_binary(archive)
        with patch.dict(d.VENDOR, archive_sha256=d.checksum(archive),
                        binary_sha256=d.hashlib.sha256(b"official").hexdigest()):
            self.assertEqual(d.vendor_binary(archive), b"official")
        redirect = d.VendorRedirect()
        for url in ("https://evil.test/artifact", "http://release-assets.githubusercontent.com/file",
                    "https://user:password@release-assets.githubusercontent.com/file"):
            with self.assertRaises(d.DeploymentError):
                redirect.redirect_request(d.Request(d.VENDOR["archive_url"]), None, 302, "", {}, url)

    def test_unit_and_binary_contracts_fail_closed(self):
        unit = (REPOSITORY / "deploy/aviary.service").read_text()
        d.validate_unit(unit)
        for text in (unit.replace("SendSIGKILL=no", "SendSIGKILL=yes"),
                     unit.replace("TimeoutStopSec=660", "TimeoutStopSec=30"),
                     unit.replace("ReadWritePaths=/mnt", "ReadWritePaths=/other /mnt"),
                     unit.replace("ExecStart=", "Environment=AVIARY_REQUIRE_EXISTING=false\nExecStart="),
                     unit.replace("[Install]", "ExecStop=/bin/kill -9 $MAINPID\n[Install]")):
            with self.assertRaises(d.DeploymentError):
                d.validate_unit(text)
        create_release(self.root / "release")
        binary = self.root / "release/aviary"
        d.validate_binary(binary)
        binary.chmod(0o600)
        with self.assertRaises(d.DeploymentError):
            d.validate_binary(binary)

    def test_committed_archive_contains_embedded_ui_not_generated_bundles(self):
        archive = self.root / "source.tar"
        if (REPOSITORY / ".git").exists():
            subprocess.run(["git", "-C", str(REPOSITORY), "archive", "-o", str(archive), "HEAD"], check=True)
        else:
            shutil.copyfile(REPOSITORY.parent / "source.tar", archive)
        source = self.root / "source"
        source.mkdir()
        d.extract(archive, source)
        self.assertIn("//go:embed web/index.html", (source / "internal/aviary/ui.go").read_text())
        self.assertIn(b"<script", (source / "internal/aviary/web/index.html").read_bytes())
        for name in ("runtime.go", "deploy/aviary.service", "deploy/vendor.json",
                     "scripts/deploy_vps.py", "scripts/test_deploy_vps.py"):
            path = source / name
            if not path.exists():
                path.parent.mkdir(parents=True, exist_ok=True)
                shutil.copyfile(REPOSITORY / name, path)
        d.validate_source(source)
        (source / "internal/aviary/web/index.html").unlink()
        with self.assertRaises(d.DeploymentError):
            d.validate_source(source)


class HTTPTests(TemporaryTest):
    def test_public_ui_keeps_every_original_byte_and_accepts_only_pinned_edge_insertion(self):
        ui = b"<html><body><script>owned application</script></body></html>"
        insertion = (b"<script>window.__CF$cv$params={r:'0123456789abcdef',t:'MTIz'};</script>"
                     b"<script data-beacon='{\"token\":\"0123456789abcdef0123456789abcdef\"}'></script>")
        normalized = insertion.replace(b"0123456789abcdef',t:'MTIz", b"RAY',t:'TIME").replace(
            b"0123456789abcdef0123456789abcdef", b"SITE")
        observed = ui.replace(b"</body>", insertion + b"</body>")
        with patch.object(d, "CF_UI_INSERTION_SHA256", hashlib.sha256(normalized).hexdigest()):
            self.assertTrue(d.public_ui_matches(observed, ui))
            self.assertTrue(d.public_ui_matches(ui, ui))
            for bad in (observed.replace(b"owned application", b"changed application"),
                        observed.replace(b"</body>", b"<script>unreviewed()</script></body>"),
                        observed.replace(b"MTIz", b"';unreviewed()"),
                        observed + b"<script>unreviewed()</script>",
                        ui.replace(b"</body>", b"x" * 9000 + b"</body>")):
                self.assertFalse(d.public_ui_matches(bad, ui))
            with patch.object(d, "request", return_value=(200, observed)):
                with self.assertRaises(d.DeploymentError):
                    d.check_http("http://backend", ui)

    def test_required_health_and_exact_embedded_ui_no_redirects_wrong_mime_body_or_modes(self):
        state = {"status": 200, "mime": "application/json", "redirect": False,
                 "health": {"status": "ok", "revision": REVISION, "version": d.VERSION,
                            "projects": 2, "cronEnabled": False}, "ui": b"<html><script>ok</script></html>",
                 "session": {"authenticated": False, "configured": True, "email": "", "role": "",
                             "hasPasskeys": True, "passwordLoginDisabled": True}}

        class Handler(BaseHTTPRequestHandler):
            def do_GET(self):
                code = 401 if self.path == "/api/projects" else state["status"]
                self.send_response(302 if state["redirect"] else code)
                self.send_header("Content-Type", state["mime"] if self.path != "/" else "text/html")
                if state["redirect"]:
                    self.send_header("Location", "/")
                self.end_headers()
                body = {"/_aviary/health": state["health"], "/api/auth/session": state["session"],
                        "/api/projects": {"code": 401, "error": "authentication required"}}
                self.wfile.write(json.dumps(body[self.path]).encode() if self.path != "/" else state["ui"])

            def log_message(self, *args):
                pass

        with HTTPServer(("127.0.0.1", 0), Handler) as server:
            thread = threading.Thread(target=server.serve_forever)
            thread.start()
            try:
                origin = "http://127.0.0.1:" + str(server.server_port)
                d.health(origin, REVISION, False, 2)
                d.check_http(origin, state["ui"])
                for key, value in (("revision", "b" * 40), ("version", "0.4.0"),
                                   ("projects", True), ("projects", -1), ("cronEnabled", 0)):
                    previous = state["health"][key]
                    state["health"][key] = value
                    with self.assertRaises(d.DeploymentError):
                        d.health(origin, REVISION, False, 2)
                    state["health"][key] = previous
                for key, value in (("status", 404), ("mime", "text/html"), ("redirect", True)):
                    previous = state[key]
                    state[key] = value
                    with self.assertRaises(d.DeploymentError):
                        d.health(origin, REVISION, False, 2)
                    state[key] = previous
                with self.assertRaises(d.DeploymentError):
                    d.check_http(origin, b"<html><script>wrong assets</script></html>")
                state["session"]["configured"] = False
                with self.assertRaises(d.DeploymentError):
                    d.check_http(origin, state["ui"])
            finally:
                server.shutdown()
                thread.join()


class LegacyGuardTests(TemporaryTest):
    def test_init_is_never_signalled_even_if_an_identity_is_supplied(self):
        with patch.object(d.os, "pidfd_open") as open_pid:
            with self.assertRaisesRegex(d.DeploymentError, "Never signal init"):
                d.stop_exact({"pid": 1}, {"pid": 0})
            open_pid.assert_not_called()

    def test_recovery_retry_requires_matching_nonrestarting_reviewed_service_and_absolute_argv(self):
        stage = self.root / "receipt"
        stage.mkdir()
        installer = d.Installer(stage, self.root / "system")
        name = "aviary-legacy-recovery-" + "a" * 32 + ".service"
        metadata = {"legacy_pid": 123, "parent_pid": 1, "legacy_sha256": d.VENDOR["binary_sha256"],
                    "legacy_recovery_unit": name}
        app = {"pid": 123, "parent": 1, "exe": str(installer.legacy / "aviary"),
               "cwd": str(installer.legacy), "sha256": d.VENDOR["binary_sha256"],
               "argv": [d.base64.b64encode(str(installer.legacy / "aviary").encode()).decode()]}
        parent = {"pid": 1}
        values = {"MainPID": "123", "Restart": "no", "SendSIGKILL": "no", "DropInPaths": ""}

        def run(arguments):
            return "active" if arguments[1] == "is-active" else values[arguments[-2]]

        with patch.object(d, "process_identity", side_effect=lambda pid: app if pid == 123 else parent), \
                patch.object(Path, "read_bytes", return_value=b""), \
                patch.object(d, "checksum", return_value=d.VENDOR["binary_sha256"]), \
                patch.object(installer, "run", side_effect=run):
            self.assertEqual(installer.legacy_identity(metadata), {"app": app, "parent": parent})
            for key, value in (("MainPID", "124"), ("Restart", "always"), ("SendSIGKILL", "yes"),
                               ("DropInPaths", "/operator.conf")):
                old = values[key]
                values[key] = value
                with self.assertRaises(d.DeploymentError):
                    installer.legacy_identity(metadata)
                values[key] = old
            for invalid in (dict(metadata, legacy_recovery_unit="unrelated.service"),
                            dict(metadata, legacy_recovery_unit=None)):
                with self.assertRaises(d.DeploymentError):
                    installer.legacy_identity(invalid)
            app["argv"] = [d.base64.b64encode(b"./aviary").decode()]
            with self.assertRaises(d.DeploymentError):
                installer.legacy_identity(metadata)

    def test_changed_process_or_parent_never_signalled(self):
        original = {"pid": 123, "start": "original"}
        with patch.object(d.os, "pidfd_open", return_value=987), \
                patch.object(d.os, "close"), \
                patch.object(d, "process_identity", return_value={"pid": 123, "start": "reused"}), \
                patch.object(d.signal, "pidfd_send_signal") as send:
            with self.assertRaises(d.DeploymentError):
                d.stop_exact(original, {"pid": 456})
            send.assert_not_called()

    def test_all_owned_tcp_and_udp_not_just_8090_are_busy(self):
        process = self.root / "123"
        (process / "fd").mkdir(parents=True)
        (process / "net").mkdir()
        (process / "fd/3").symlink_to("socket:[111]")
        (process / "fd/4").symlink_to("socket:[222]")
        header = "header\n"
        row = lambda inode, state: f"0: 00000000:0019 00000000:001A {state} 0 0 0 0 0 {inode}\n"
        for protocol in ("tcp", "tcp6", "udp", "udp6"):
            (process / "net" / protocol).write_text(header)
        (process / "net/tcp").write_text(header + row("111", "0A") + row("222", "01") + row("999", "01"))
        (process / "net/udp6").write_text(header + row("111", "07"))
        self.assertEqual([x[0] for x in d.owned_connections(123, self.root)], ["tcp", "udp6"])

    def test_schedule_review_requires_risk_acknowledgement_identity_state_and_bounded_window(self):
        proof = {"format": 1, "legacy": {"pid": 123}, "state_manifest": {"complete": "digest"},
                 "not_before": 100, "not_after": 2000, "legacy_shutdown_risk_accepted": True,
                 "scheduled_writers_excluded": True, "reviewed_schedules": ["backup", "optimization", "log flush"]}
        d.validate_schedule_review(proof, proof["legacy"], proof["state_manifest"], 200)
        for changes in ({"state_manifest": {}}, {"legacy": {}}, {"not_after": 201},
                        {"not_after": 5000}, {"legacy_shutdown_risk_accepted": False},
                        {"scheduled_writers_excluded": False}, {"reviewed_schedules": []}):
            with self.assertRaises(d.DeploymentError):
                d.validate_schedule_review(dict(proof, **changes), proof["legacy"], proof["state_manifest"], 200)
        with self.assertRaises(d.DeploymentError):
            d.validate_schedule_review(None, proof["legacy"], proof["state_manifest"], 200)

    def quiet_fixture(self):
        stage = self.root / "receipt"
        stage.mkdir()
        installer = d.Installer(stage, self.root / "system")
        create_state(installer.legacy / "data")
        legacy = {"app": {"pid": 123}, "parent": {"pid": 456}}
        manifest = d.state_manifest(installer.legacy / "data")
        proof = {"format": 1, "legacy": legacy, "state_manifest": manifest,
                 "not_before": 1000, "not_after": 2600, "legacy_shutdown_risk_accepted": True,
                 "scheduled_writers_excluded": True, "reviewed_schedules": ["backup", "optimization", "log"]}
        return installer, legacy, manifest, proof

    def test_legacy_guard_requires_sixty_continuous_quiet_seconds_after_all_socket_activity(self):
        i, legacy, manifest, proof = self.quiet_fixture()
        clock = [0]

        def sleep(seconds):
            clock[0] += seconds

        with patch.object(d, "state_manifest", return_value=manifest), \
                patch.object(d, "process_identity", side_effect=lambda pid: {"pid": pid}), \
                patch.object(d, "owned_connections", side_effect=lambda pid: ["SMTP"] if clock[0] < 5 else []), \
                patch.object(d.time, "monotonic", side_effect=lambda: clock[0]), \
                patch.object(d.time, "time", side_effect=lambda: 1000 + clock[0]), \
                patch.object(d.time, "sleep", side_effect=sleep):
            self.assertEqual(i.legacy_quiet(legacy, proof), manifest)
        self.assertEqual(clock[0], 5 + d.QUIET_SECONDS)

    def test_legacy_guard_rechecks_schedule_window_after_the_quiet_wait(self):
        i, legacy, manifest, proof = self.quiet_fixture()
        proof["not_after"] = 1930
        clock = [0]

        def sleep(seconds):
            clock[0] += seconds

        with patch.object(d, "state_manifest", return_value=manifest), \
                patch.object(d, "process_identity", side_effect=lambda pid: {"pid": pid}), \
                patch.object(d, "owned_connections", return_value=[]), \
                patch.object(d.time, "monotonic", side_effect=lambda: clock[0]), \
                patch.object(d.time, "time", side_effect=lambda: 1000 + clock[0]), \
                patch.object(d.time, "sleep", side_effect=sleep):
            with self.assertRaisesRegex(d.DeploymentError, "900 seconds"):
                i.legacy_quiet(legacy, proof)
        self.assertEqual(clock[0], d.QUIET_SECONDS)

    def test_legacy_cron_jobs_and_hook_files_require_separate_review(self):
        i, legacy, manifest, proof = self.quiet_fixture()
        control = i.legacy / "data/control.db"
        with closing(sqlite3.connect(control)) as db:
            db.execute("INSERT INTO cron_jobs VALUES('unreviewed')")
            db.commit()
        proof["state_manifest"] = d.state_manifest(i.legacy / "data")
        with patch.object(d.time, "time", return_value=1000), \
                self.assertRaisesRegex(d.DeploymentError, "zero control cron"):
            i.legacy_quiet(legacy, proof)
        with closing(sqlite3.connect(control)) as db:
            db.execute("DELETE FROM cron_jobs")
            db.commit()
        (i.legacy / "data/projects/lgspkb/pb_hooks/active.pb.js").write_bytes(b"hook")
        proof["state_manifest"] = d.state_manifest(i.legacy / "data")
        with patch.object(d.time, "time", return_value=1000), \
                self.assertRaisesRegex(d.DeploymentError, "Project JS hooks"):
            i.legacy_quiet(legacy, proof)


class ClientBuildTests(TemporaryTest):
    def test_dirty_tracked_source_is_rejected_before_build_or_upload(self):
        with patch.object(d.subprocess, "check_output", return_value=" M main.go\n"), \
                patch.object(d.subprocess, "run") as run:
            with self.assertRaisesRegex(d.DeploymentError, "Tracked source is dirty"):
                d.build_source(REPOSITORY, self.root)
        run.assert_not_called()

    def test_clean_archive_pipeline_runs_nonrace_race_vet_python_and_pinned_cgo_zero_build(self):
        commands = []
        required = ("go.mod", "go.sum", "main.go", "runtime.go", "deploy/aviary.service",
                    "deploy/vendor.json", "scripts/deploy_vps.py", "scripts/test_deploy_vps.py",
                    "internal/aviary/ui.go", "internal/aviary/web/index.html")

        def output(arguments, **kwargs):
            if arguments[0] == "go":
                return "go version go1.25.0 linux/amd64\n"
            if arguments[3] == "status":
                self.assertIn("--untracked-files=no", arguments)
                return ""
            if arguments[3] == "rev-parse":
                return REVISION + "\n"
            return (REPOSITORY / arguments[4].split(":", 1)[1]).read_bytes()

        def run(arguments, **kwargs):
            commands.append((arguments, kwargs))
            if arguments[0] == "git":
                with tarfile.open(arguments[5], "w") as archive:
                    for name in required:
                        archive.add(REPOSITORY / name, arcname=name, recursive=False)
            if arguments[:2] == ["go", "build"]:
                target = Path(arguments[arguments.index("-o") + 1])
                target.write_bytes(b"\x7fELF\x02\x01\x01" + b"\0" * 11 + b"\x3e\x00")
                target.chmod(0o700)
            return SimpleNamespace(returncode=0)

        with patch.object(d.subprocess, "check_output", side_effect=output), \
                patch.object(d.subprocess, "run", side_effect=run):
            self.assertEqual(d.build_source(REPOSITORY, self.root), REVISION)
        go = [(args, values["env"]) for args, values in commands if args[0] == "go"]
        self.assertEqual([args[1] for args, _ in go], ["test", "test", "vet", "build"])
        self.assertEqual([env["CGO_ENABLED"] for _, env in go], ["0", "1", "0", "0"])
        self.assertNotIn("-race", go[0][0])
        self.assertIn("-race", go[1][0])
        for args, env in go:
            self.assertIn("-mod=readonly", args)
            self.assertEqual(env["GOTOOLCHAIN"], "go1.25.0")
            self.assertEqual(env["GOAMD64"], "v1")
            self.assertEqual(env["GOEXPERIMENT"], "")
        build = go[-1][0]
        self.assertIn("-trimpath", build)
        self.assertIn("-buildvcs=false", build)
        self.assertEqual(build[build.index("-ldflags") + 1],
                         "-X main.version=0.4.0-managed.1 -X main.buildRevision=" + REVISION)
        self.assertTrue(any("test_deploy_vps.py" in args for args, _ in commands))
        with tarfile.open(self.root / "release.tar") as archive:
            self.assertEqual(set(archive.getnames()), d.PAYLOAD_FILES | {"release.json"})
        self.assertTrue((self.root / "source.tar").exists())

    def test_cli_rejects_unapproved_legacy_combinations_without_build_or_ssh(self):
        for arguments in ([], ["--yes", "--legacy-pid", "123"],
                          ["--yes", "--migrate-tmux", "--legacy-pid", "123", "--parent-pid", "456",
                           "--legacy-sha256", d.VENDOR["binary_sha256"]]):
            with patch.object(d, "build_source") as build, patch.object(d, "ssh") as ssh:
                with self.assertRaises(d.DeploymentError):
                    d.deploy(d.parser().parse_args(arguments))
            build.assert_not_called()
            ssh.assert_not_called()

    def test_initial_cutover_requires_both_schedule_review_and_risk_acknowledgement(self):
        base = ["--yes", "--migrate-tmux", "--legacy-pid", "123", "--parent-pid", "456",
                "--legacy-sha256", d.VENDOR["binary_sha256"]]
        for extra in (["--accept-legacy-shutdown-risk"],
                      ["--legacy-schedule-review", "/review.json"]):
            with patch.object(d, "build_source") as build, patch.object(d, "ssh") as ssh:
                with self.assertRaisesRegex(d.DeploymentError, "requires --accept-legacy"):
                    d.deploy(d.parser().parse_args(base + extra))
                build.assert_not_called()
                ssh.assert_not_called()
        with patch.object(d, "build_source") as build, patch.object(d, "ssh") as ssh:
            with self.assertRaises(d.DeploymentError):
                d.deploy(d.parser().parse_args(["--yes", "--accept-legacy-shutdown-risk"]))
            build.assert_not_called()
            ssh.assert_not_called()


class FakeInstaller(d.Installer):
    def __init__(self, stage, system_root):
        super().__init__(stage, system_root)
        self.user = SimpleNamespace(pw_uid=os.getuid(), pw_gid=os.getgid(),
                                    pw_dir="/var/lib/aviary", pw_shell="/usr/sbin/nologin")
        self.starts = 0
        self.calls = []
        self.failure = None
        self.stuck_stop = False
        self.stop_attempts = 0
        self.preflights = 0
        self.live = b""
        self.stopped_units = set()

    def caddy_live(self):
        return self.live

    def adaptation(self, path):
        return json.dumps({"aviary": 503 if b"503" in path.read_bytes() else 8090, "other": 9000}).encode()

    def check_platform(self):
        pass

    def check_dropins(self):
        pass

    def assert_release(self, release):
        d.require(d.release_files(release) == json.loads((release / "release.json").read_text())["files"],
                  "Release changed")

    def run(self, arguments, timeout=180):
        self.calls.append(arguments)
        if arguments[:2] == ["caddy", "adapt"]:
            return self.adaptation(Path(arguments[3])).decode()
        if arguments[:2] == ["caddy", "reload"]:
            self.live = self.adaptation(Path(arguments[3]))
        if arguments[:2] == ["systemctl", "stop"]:
            self.stop_attempts += 1
            if self.stuck_stop:
                raise d.DeploymentError("stop stuck")
            self.stopped_units.add(arguments[2])
        if arguments[0] == "systemd-run":
            unit = next(value.split("=", 1)[1] for value in arguments if value.startswith("--unit="))
            self.stopped_units.discard(unit)
        if arguments[:3] == ["systemctl", "start", "aviary.service"]:
            self.starts += 1
            if self.starts == 1 and self.failure:
                self.failure()
                raise d.DeploymentError("startup failed")
        if arguments[:2] == ["systemctl", "show"]:
            if arguments[4] == "MainPID":
                if arguments[2] in self.stopped_units:
                    return "0"
                return "123" if arguments[2].startswith(("aviary-legacy", "aviary-check")) else "0"
            if arguments[4] in ("SendSIGKILL", "KillMode", "KillSignal"):
                return {"SendSIGKILL": "no", "KillMode": "mixed", "KillSignal": "15"}[arguments[4]]
            return ""
        if arguments[:2] == ["systemctl", "is-enabled"]:
            return "enabled"
        if arguments[:2] == ["systemctl", "is-active"]:
            return "active"
        return ""

    def wait_health(self, *args):
        pass

    def check_process(self, release):
        return 123

    def legacy_quiet(self, legacy, proof):
        return d.state_manifest(self.legacy / "data")

    def prepare_release(self, metadata):
        return self.release, REVISION

    def preflight(self, *args):
        self.preflights += 1


class TransactionTests(TemporaryTest):
    def setUp(self):
        super().setUp()
        stage = self.root / "receipt"
        stage.mkdir(mode=0o700)
        self.installer = FakeInstaller(stage, self.root / "system")
        i = self.installer
        for path in (i.root, i.env.parent, i.unit.parent, i.backup_config.parent):
            path.mkdir(parents=True, exist_ok=True)
        i.root.chmod(0o755)
        i.caddy.parent.mkdir(parents=True)
        i.caddy.write_bytes(b"# unrelated Caddy routes\n" * 74 + b"\treverse_proxy :8090\n# other :9000\n")
        i.live = i.adaptation(i.caddy)
        text, unit = backup_inputs()
        i.backup_config.write_text(text)
        i.backup_unit.write_text(unit)
        self.release = i.root / "releases" / REVISION
        self.previous = i.root / "releases" / ("b" * 40)
        create_release(self.release)
        create_release(self.previous, "b" * 40)
        i.release = self.release
        i.env.write_bytes(d.CONFIGURATION)
        i.unit.write_bytes((self.previous / "aviary.service").read_bytes())
        i.current.symlink_to(self.previous)
        create_state(i.data)
        self.addCleanup(patch.stopall)
        patch.object(d.os, "fchown").start()
        patch.object(d, "check_http").start()
        patch.object(d, "health").start()

    def activate(self):
        self.installer.activate(self.release, REVISION, None, self.installer.configuration_records())

    def test_successful_activation_preserves_exact_stopped_state_before_public_checks(self):
        before = d.state_manifest(self.installer.data)
        self.activate()
        self.assertEqual(self.installer.current.resolve(), self.release)
        self.assertEqual(d.state_manifest(self.installer.data), before)
        self.assertTrue(self.installer.pending.exists())
        self.assertEqual(json.loads((self.installer.stage / "deployment.json").read_text())["status"], "healthy")
        calls = self.installer.calls
        self.assertLess(calls.index(["systemctl", "stop", "aviary.service"]),
                        calls.index(["systemctl", "start", "aviary.service"]))

    def test_unchanged_state_recovers_previous_binary_without_copying_stale_databases(self):
        before = d.state_manifest(self.installer.data)
        self.installer.failure = lambda: None
        with patch.object(d, "copy_state", wraps=d.copy_state) as copies:
            with self.assertRaisesRegex(d.DeploymentError, "startup failed"):
                self.activate()
        self.assertEqual(copies.call_count, 1)
        self.assertEqual(self.installer.current.resolve(), self.previous)
        self.assertEqual(d.state_manifest(self.installer.data), before)
        self.assertFalse(self.installer.pending.exists())
        self.assertEqual(self.installer.starts, 2)

    def assert_change_blocks_rollback(self, change):
        self.installer.failure = change
        with self.assertRaisesRegex(d.DeploymentError, "forbid rollback"):
            self.activate()
        self.assertTrue(self.installer.pending.exists())
        self.assertEqual(self.installer.current.resolve(), self.release)
        self.assertEqual(self.installer.starts, 1)
        self.assertIn(["systemctl", "disable", "aviary.service"], self.installer.calls)

    def test_accepted_database_changes_forbid_rollback(self):
        def change():
            with closing(sqlite3.connect(self.installer.data / "control.db")) as db:
                db.execute("INSERT INTO legacy(value) VALUES('accepted')")
                db.commit()
        self.assert_change_blocks_rollback(change)

    def test_accepted_schema_changes_forbid_rollback(self):
        def change():
            with closing(sqlite3.connect(self.installer.data / "control.db")) as db:
                db.execute("ALTER TABLE legacy ADD COLUMN accepted TEXT")
                db.commit()
        self.assert_change_blocks_rollback(change)

    def test_accepted_file_changes_forbid_rollback(self):
        def change():
            (self.installer.data / "projects/lgspkb/uploads/new").write_bytes(b"accepted")
        self.assert_change_blocks_rollback(change)

    def test_accepted_new_project_changes_forbid_rollback(self):
        def change():
            (self.installer.data / "projects/new").mkdir()
            with closing(sqlite3.connect(self.installer.data / "control.db")) as db:
                db.execute("INSERT INTO projects VALUES('new','accepted')")
                db.commit()
        self.assert_change_blocks_rollback(change)

    def test_operator_configuration_bytes_and_permissions_forbid_rollback(self):
        def change_config():
            self.installer.env.write_bytes(b"operator secret configuration")

        self.installer.failure = change_config
        with self.assertRaisesRegex(d.DeploymentError, "Operator configuration"):
            self.activate()
        self.assertEqual(self.installer.env.read_bytes(), b"operator secret configuration")
        self.assertTrue(self.installer.pending.exists())
        self.assertEqual(self.installer.starts, 1)

    def test_operator_state_permissions_forbid_rollback(self):
        self.installer.failure = lambda: (self.installer.data / "control.db").chmod(0o644)
        with self.assertRaisesRegex(d.DeploymentError, "permissions/ownership"):
            self.activate()
        self.assertTrue(self.installer.pending.exists())

    def test_stuck_stop_preserves_pending_and_never_snapshots_or_switches(self):
        self.installer.stuck_stop = True
        with patch.object(d, "copy_state") as copies:
            with self.assertRaisesRegex(d.DeploymentError, "Stop unconfirmed"):
                self.activate()
        copies.assert_not_called()
        self.assertEqual(self.installer.current.resolve(), self.previous)
        self.assertTrue(self.installer.pending.exists())
        self.assertEqual(self.installer.stop_attempts, 1)

    def test_stuck_stop_during_recovery_never_reverts_pointer_or_config(self):
        def become_stuck():
            self.installer.stuck_stop = True

        self.installer.failure = become_stuck
        with self.assertRaisesRegex(d.DeploymentError, "stop stuck"):
            self.activate()
        self.assertEqual(self.installer.current.resolve(), self.release)
        self.assertTrue(self.installer.pending.exists())

    def test_initial_failure_recovers_intact_original_without_touching_parent_or_restoring_data(self):
        i = self.installer
        i.current.unlink()
        i.env.unlink()
        i.unit.unlink()
        shutil.rmtree(i.data)
        create_state(i.legacy / "data")
        (i.legacy / "aviary").write_bytes(b"official fixture")
        original_state = d.state_manifest(i.legacy / "data")
        original_files = d.state_manifest(i.legacy / "data", physical=True)
        old_config = i.configuration_records()
        i.failure = lambda: None
        legacy = {"app": {"pid": 321}, "parent": {"pid": 654}}
        with patch.dict(d.VENDOR, binary_sha256=d.checksum(i.legacy / "aviary")), \
                patch.object(d, "stop_exact") as stop, \
                patch.object(d, "process_identity", return_value={
                    "sha256": d.checksum(i.legacy / "aviary"), "exe": str(i.legacy / "aviary"),
                    "cwd": str(i.legacy)}):
            with self.assertRaisesRegex(d.DeploymentError, "startup failed"):
                i.activate(self.release, REVISION, legacy, old_config, {})
        stop.assert_called_once_with(legacy["app"], legacy["parent"])
        self.assertEqual(d.state_manifest(i.legacy / "data"), original_state)
        self.assertEqual(d.state_manifest(i.legacy / "data", physical=True), original_files)
        self.assertEqual(i.configuration_records(), old_config)
        self.assertTrue(i.data.exists())
        self.assertFalse(i.pending.exists())
        self.assertFalse(i.current.exists())
        recovery = [call for call in i.calls if call[0] == "systemd-run"]
        self.assertEqual(len(recovery), 1)
        self.assertEqual(recovery[0][-1], str(i.legacy / "aviary"))
        self.assertNotIn("tmux", " ".join(recovery[0]))

    def test_successful_initial_activation_retargets_backups_and_retains_original(self):
        i = self.installer
        i.current.unlink()
        i.env.unlink()
        i.unit.unlink()
        shutil.rmtree(i.data)
        create_state(i.legacy / "data")
        before = d.state_manifest(i.legacy / "data")
        legacy = {"app": {"pid": 321}, "parent": {"pid": 654}}
        with patch.object(d, "stop_exact") as stop:
            i.activate(self.release, REVISION, legacy, i.configuration_records(), {})
        stop.assert_called_once_with(legacy["app"], legacy["parent"])
        self.assertEqual(d.state_manifest(i.data), before)
        self.assertEqual(d.state_manifest(i.legacy / "data"), before)
        self.assertEqual(i.env.read_bytes(), d.CONFIGURATION)
        self.assertIn(str(d.DATA), i.backup_config.read_text())
        self.assertIn("aviary-configuration", i.backup_config.read_text())
        self.assertNotIn(str(d.LEGACY / "data"), i.backup_unit.read_text())
        self.assertEqual(i.current.resolve(), self.release)

    def test_maintenance_restores_disk_attributes_and_live_bytes_on_body_failure(self):
        i = self.installer
        original, live = d.file_record(i.caddy), i.live
        with self.assertRaisesRegex(d.DeploymentError, "cutover failure"):
            with i.maintenance():
                self.assertIn(b"503", i.caddy.read_bytes())
                self.assertEqual(json.loads(i.live)["aviary"], 503)
                raise d.DeploymentError("cutover failure")
        self.assertEqual(d.file_record(i.caddy), original)
        self.assertEqual(i.live, live)
        validate = next(n for n, args in enumerate(i.calls) if args[:2] == ["caddy", "validate"])
        reload = next(n for n, args in enumerate(i.calls) if args[:2] == ["caddy", "reload"])
        self.assertLess(validate, reload)

    def test_maintenance_never_overwrites_operator_disk_or_live_changes(self):
        i = self.installer
        with self.assertRaisesRegex(d.DeploymentError, "Operator changed Caddy"):
            with i.maintenance():
                i.caddy.write_bytes(b"operator Caddy configuration")
                i.live = b'{"operator": true}'
        self.assertEqual(i.caddy.read_bytes(), b"operator Caddy configuration")
        self.assertEqual(i.live, b'{"operator": true}')

    def install_request(self):
        i = self.installer
        d.write_json(i.root / "deployed.json", {"configuration": i.configuration_records(),
                                               "release": str(i.current.resolve())})
        d.write_json(i.stage / "request.json", {"migrate_tmux": False, "rehearse": False})
        lock = i.system_root / "var/lib/backuper/job.lock"
        lock.parent.mkdir(parents=True)
        lock.touch()
        return patch.object(d.pwd, "getpwnam", return_value=i.user), patch.object(d.os, "chown")

    def test_complete_install_maintenance_precedes_stop_and_finalizes_only_after_public_checks(self):
        i = self.installer
        original, live = d.file_record(i.caddy), i.live
        account, owner = self.install_request()
        with account, owner:
            i.install()
        self.assertEqual(d.file_record(i.caddy), original)
        self.assertEqual(i.live, live)
        self.assertFalse(i.pending.exists())
        self.assertEqual(json.loads((i.root / "deployed.json").read_text())["status"], "verified_public")
        maintenance_reload = next(n for n, args in enumerate(i.calls) if args[:2] == ["caddy", "reload"])
        stop = i.calls.index(["systemctl", "stop", "aviary.service"])
        self.assertLess(maintenance_reload, stop)

    def test_public_failure_with_accepted_writes_stops_disables_and_keeps_pending(self):
        i = self.installer
        account, owner = self.install_request()

        def failed_public(*args):
            with closing(sqlite3.connect(i.data / "control.db")) as db:
                db.execute("INSERT INTO legacy(value) VALUES('public accepted')")
                db.commit()
            raise d.DeploymentError("public health failed")

        with account, owner, patch.object(d, "health", side_effect=failed_public):
            with self.assertRaisesRegex(d.DeploymentError, "forbid rollback"):
                i.install()
        self.assertEqual(i.current.resolve(), self.release)
        self.assertTrue(i.pending.exists())
        self.assertIn(["systemctl", "disable", "aviary.service"], i.calls)
        self.assertEqual(i.stop_attempts, 2)

    def test_failure_after_verified_public_receipt_recovers_instead_of_leaving_untracked_service(self):
        i = self.installer
        account, owner = self.install_request()
        baseline = d.file_record(i.root / "deployed.json")
        original = d.private_write

        def fail_baseline(path, *args, **kwargs):
            if path == i.root / "deployed.json" and "baseline_installed" in i.transaction:
                raise d.DeploymentError("baseline write failed")
            return original(path, *args, **kwargs)

        with account, owner, patch.object(d, "private_write", side_effect=fail_baseline):
            with self.assertRaisesRegex(d.DeploymentError, "baseline write failed"):
                i.install()
        self.assertFalse(i.pending.exists())
        self.assertIn(["systemctl", "disable", "aviary.service"], i.calls)
        self.assertEqual(d.file_record(i.root / "deployed.json"), baseline)
        self.assertEqual(i.current.resolve(), self.previous)

    def test_copied_preflight_is_private_network_cron_disabled_and_all_state_unchanged(self):
        i = self.installer
        before = d.state_manifest(i.data)
        d.Installer.preflight(i, self.release, REVISION, i.data)
        self.assertEqual(d.state_manifest(i.data), before)
        command = next(args for args in i.calls if args[0] == "systemd-run")
        self.assertIn("PrivateNetwork=true", command)
        self.assertIn("IPAddressDeny=any", command)
        self.assertIn("IPAddressAllow=localhost", command)
        self.assertIn("--disable-cron", command)
        self.assertIn("--require-existing", command)
        probe = next(args for args in i.calls if args[0] == "nsenter")
        self.assertIn("--net", probe)
        self.assertIn("--probe", probe)
        self.assertFalse(any((i.data.parent / ".aviary-deploy-checks").iterdir()))

    def test_stuck_copied_stop_preserves_copy_and_blocks_followup_transaction(self):
        i = self.installer
        i.stuck_stop = True
        with self.assertRaisesRegex(d.DeploymentError, "Copied stop unconfirmed"):
            d.Installer.preflight(i, self.release, REVISION, i.data)
        self.assertTrue(i.pending.exists())
        copies = list((i.data.parent / ".aviary-deploy-checks").iterdir())
        self.assertEqual(len(copies), 1)
        self.assertEqual(d.state_manifest(copies[0] / "state"), d.state_manifest(i.data))

    def test_changed_loaded_stop_policy_never_invokes_potential_forced_stop(self):
        i = self.installer
        original = i.run

        def altered(arguments, **kwargs):
            if arguments[:2] == ["systemctl", "show"] and arguments[4] == "SendSIGKILL":
                return "yes"
            return original(arguments, **kwargs)

        with patch.object(i, "run", side_effect=altered):
            with self.assertRaisesRegex(d.DeploymentError, "potentially forced"):
                i.stop()
        self.assertEqual(i.stop_attempts, 0)

    def test_healthy_identical_release_noop_preserves_pid_without_stop_snapshot_or_rehearsal(self):
        i = self.installer
        i.current.unlink()
        i.current.symlink_to(self.release)
        d.write_json(i.root / "deployed.json", {"configuration": i.configuration_records(), "release": str(self.release)})
        d.write_json(i.stage / "request.json", {"migrate_tmux": False, "rehearse": False})
        lock = i.system_root / "var/lib/backuper/job.lock"
        lock.parent.mkdir(parents=True)
        lock.touch()
        with patch.object(d.pwd, "getpwnam", return_value=i.user), patch.object(d.os, "chown"):
            i.install()
        self.assertEqual(i.starts, 0)
        self.assertEqual(i.stop_attempts, 0)
        self.assertEqual(i.preflights, 0)
        self.assertEqual(json.loads((i.stage / "deployment.json").read_text())["pid"], 123)

    def test_pending_blocks_noop_and_operator_configuration_drift_not_adopted(self):
        i = self.installer
        i.current.unlink()
        i.current.symlink_to(self.release)
        d.write_json(i.root / "deployed.json", {"configuration": i.configuration_records(), "release": str(self.release)})
        d.write_json(i.stage / "request.json", {"migrate_tmux": False, "rehearse": False})
        lock = i.system_root / "var/lib/backuper/job.lock"
        lock.parent.mkdir(parents=True)
        lock.touch()
        i.pending.write_text("{}")
        with self.assertRaisesRegex(d.DeploymentError, "Pending"):
            i.install()
        i.pending.unlink()
        i.backup_config.write_text(i.backup_config.read_text() + "# operator edit\n")
        with self.assertRaisesRegex(d.DeploymentError, "Operator configuration"):
            i.install()
        self.assertEqual(i.stop_attempts, 0)


if __name__ == "__main__":
    unittest.main()

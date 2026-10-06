#!/usr/bin/env -S uv run --script
# /// script
# requires-python = ">=3.11"
# dependencies = []
# ///
"""Tests for scripts/migrate-ids.py. Run: uv run scripts/test_migrate_ids.py"""

from __future__ import annotations

import contextlib
import fcntl
import importlib.util
import io
import json
import os
import sqlite3
import sys
import tempfile
import unittest
from pathlib import Path

HERE = Path(__file__).resolve().parent
_spec = importlib.util.spec_from_file_location("migrate_ids", HERE / "migrate-ids.py")
mig = importlib.util.module_from_spec(_spec)
sys.modules["migrate_ids"] = mig
_spec.loader.exec_module(mig)

# The v40 tables and columns that the script reads.
SCHEMA_V40 = """
CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL);
CREATE TABLE workspaces (id TEXT PRIMARY KEY, name TEXT NOT NULL, path TEXT NOT NULL);
CREATE TABLE job_index (
  workspace_id TEXT NOT NULL, name TEXT NOT NULL, path TEXT NOT NULL,
  kind TEXT NOT NULL DEFAULT 'job', active INTEGER NOT NULL DEFAULT 1,
  harness TEXT, hash TEXT, parse_error TEXT, updated_at TEXT NOT NULL,
  owner TEXT NOT NULL DEFAULT '', PRIMARY KEY (workspace_id, name));
CREATE TABLE runs (id TEXT PRIMARY KEY, workspace_id TEXT NOT NULL, job TEXT NOT NULL);
CREATE TABLE event_queue (id TEXT PRIMARY KEY, workspace_id TEXT NOT NULL, job TEXT NOT NULL, kind TEXT NOT NULL DEFAULT 'job');
CREATE TABLE event_outcomes (event_id TEXT NOT NULL, workspace_id TEXT NOT NULL, job TEXT NOT NULL);
CREATE TABLE schedules (workspace_id TEXT NOT NULL, job TEXT NOT NULL, idx INTEGER NOT NULL DEFAULT 0);
CREATE TABLE inbox_items (id TEXT PRIMARY KEY, workspace_id TEXT, job TEXT);
CREATE TABLE requests (id TEXT PRIMARY KEY, workspace_id TEXT NOT NULL, job TEXT NOT NULL);
CREATE TABLE agent_proposals (id TEXT PRIMARY KEY, workspace_id TEXT NOT NULL, agent TEXT NOT NULL);
CREATE TABLE conversations (id TEXT PRIMARY KEY, workspace_id TEXT NOT NULL, operator TEXT NOT NULL DEFAULT '');
CREATE TABLE email_messages (id TEXT PRIMARY KEY, workspace_id TEXT NOT NULL DEFAULT '', agent TEXT NOT NULL DEFAULT '');
CREATE TABLE event_log (seq INTEGER PRIMARY KEY AUTOINCREMENT, entity TEXT, entity_id TEXT, attrs_json TEXT);
CREATE TABLE run_events (id INTEGER PRIMARY KEY AUTOINCREMENT, run_id TEXT NOT NULL, detail_json TEXT);
INSERT INTO schema_migrations VALUES (40, '2026-10-01T00:00:00Z');
"""

WS = "ws_11111111"
T = "2026-10-01T00:00:00Z"


class Fixture:
    """A small trebi home with one workspace, one agent, and plain jobs."""

    def __init__(self, root: Path):
        self.home = root / "home"
        self.ws = root / "ws"
        self.home.mkdir()
        self.db_path = self.home / "trebi.db"
        files = {
            "plain": ("plain/JOB.md", b"# no frontmatter\n"),
            "dup-a": ("dup-a/JOB.md", b"---\nid: jb_aaaaaaaaaa\n---\nA\n"),
            "dup-b": ("dup-b/JOB.md", b"---\nharness: x\nid: jb_aaaaaaaaaa\n---\nB\n"),
            "valid": ("valid/JOB.md", b"---\nid: jb_0123456789\n---\n"),
            "crlf": ("crlf/JOB.md", b"---\r\nharness: x\r\nid: agt_0123456789\r\nmeta:\r\n  id: inner\r\n---\r\nbody\r\n"),
            "agents/ops": ("agents/ops/AGENT.md", b"---\nid: jb_bbbbbbbbbb\nname: ops\n---\n"),
            "agents/ops/child": ("agents/ops/child/JOB.md", b"---\nname: child\n---\n"),
        }
        c = sqlite3.connect(self.db_path)
        c.executescript(SCHEMA_V40)
        c.execute("INSERT INTO workspaces VALUES (?, 'w', ?)", (WS, str(self.ws)))
        for name, (rel, data) in files.items():
            p = self.ws / rel
            p.parent.mkdir(parents=True, exist_ok=True)
            p.write_bytes(data)
            kind = "agent" if rel.endswith("AGENT.md") else "job"
            owner = "agents/ops" if name.startswith("agents/ops/") else ""
            c.execute("INSERT INTO job_index (workspace_id, name, path, kind, updated_at, owner) VALUES (?,?,?,?,?,?)", (WS, name, str(p), kind, T, owner))
        c.execute("INSERT INTO job_index (workspace_id, name, path, updated_at) VALUES (?, 'gone', ?, ?)", (WS, str(self.ws / "gone/JOB.md"), T))
        c.executemany("INSERT INTO runs VALUES (?,?,?)", [("r1", WS, "plain"), ("r2", WS, "deleted"), ("r3", WS, "agents/ops"), ("r4", WS, "agents/ops/child")])
        c.execute("INSERT INTO event_queue (id, workspace_id, job) VALUES ('q1', ?, 'deleted')", (WS,))
        c.execute("INSERT INTO event_outcomes VALUES ('e1', ?, 'plain')", (WS,))
        c.execute("INSERT INTO schedules VALUES (?, 'valid', 0)", (WS,))
        c.executemany("INSERT INTO inbox_items VALUES (?,?,?)", [("i1", WS, "plain"), ("i2", WS, ""), ("i3", None, None)])
        c.execute("INSERT INTO requests VALUES ('rq1', ?, 'agents/ops/child')", (WS,))
        c.execute("INSERT INTO agent_proposals VALUES ('p1', ?, 'ops')", (WS,))
        c.executemany("INSERT INTO conversations VALUES (?,?,?)", [("c1", WS, ""), ("c2", WS, "agents/ops")])
        c.executemany("INSERT INTO email_messages VALUES (?,?,?)", [("m1", WS, "ops"), ("m2", WS, ""), ("m3", WS, "old")])
        logs = [
            ("job", "plain", {"job": "plain", "workspace": WS}),
            ("agent", "agents/ops", {"name": "agents/ops", "workspace": WS}),
            ("run", "r4", {"job": "agents/ops/child", "owner": "agents/ops", "workspace": WS, "note": "a<b"}),
            ("request", "rq1", {"job": "agents/ops/child", "workspace_id": WS}),
            ("job", "nobody", {"job": "nobody"}),
            ("workspace", WS, {"name": "W"}),
        ]
        c.executemany("INSERT INTO event_log (entity, entity_id, attrs_json) VALUES (?,?,?)", [(e, i, json.dumps(a)) for e, i, a in logs])
        c.executemany(
            "INSERT INTO run_events (run_id, detail_json) VALUES (?,?)",
            [
                ("r4", json.dumps({"job": "jobs/x/child", "parent_agent": "ops", "reason": "manual"})),
                ("r4", json.dumps({"delta": "true", "text": "agent"})),
                ("r2", json.dumps({"operator": ""})),
            ],
        )
        c.commit()
        c.close()
        self.stub = root / "stub.sh"
        self.stub.write_text('#!/bin/sh\necho "$@" >> "$(dirname "$0")/stub.log"\n[ "$2" = status ] && echo \'{"os": "running"}\'\nexit 0\n')
        self.stub.chmod(0o755)

    def run(self, *args: str) -> tuple[int, str]:
        out = io.StringIO()
        os.environ["TREBI_SERVICE_CMD"] = str(self.stub)
        with contextlib.redirect_stdout(out), contextlib.redirect_stderr(out):
            code = mig.main(["--home", str(self.home), *args])
        return code, out.getvalue()

    def q(self, sql: str, *args):
        c = sqlite3.connect(self.db_path)
        try:
            return c.execute(sql, args).fetchall()
        finally:
            c.close()

    def id_of(self, name: str) -> str:
        return self.q("SELECT id FROM job_index WHERE workspace_id = ? AND name = ?", WS, name)[0][0]


class FrontmatterTest(unittest.TestCase):
    def test_edits(self):
        cases = [
            (b"body\n", b"---\nid: jb_0000000000\n---\nbody\n"),
            (b"body\r\nx\r\n", b"---\r\nid: jb_0000000000\r\n---\r\nbody\r\nx\r\n"),
            (b"---\na: 1\n---\nb\n", b"---\nid: jb_0000000000\na: 1\n---\nb\n"),
            (b"---\na: 1\nid: x\n  id: y\n---\nid: z\n", b"---\nid: jb_0000000000\na: 1\n  id: y\n---\nid: z\n"),
            (b"---\r\nid: 'old' # c\r\n---\r\n", b"---\r\nid: jb_0000000000\r\n---\r\n"),
        ]
        for src, want in cases:
            fm = mig.Frontmatter.parse(src)
            self.assertEqual(fm.with_id("jb_0000000000"), want)

    def test_read_and_malformed(self):
        self.assertEqual(mig.Frontmatter.parse(b'---\nid: "jb_0123456789"\n---\n').id(), "jb_0123456789")
        self.assertIsNone(mig.Frontmatter.parse(b"---\nid: x\nno close\n"))


class MigrationTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.fx = Fixture(Path(self.tmp.name))

    def tearDown(self):
        self.tmp.cleanup()

    def test_dry_run_writes_nothing(self):
        before = {p: p.read_bytes() for p in self.fx.ws.rglob("*.md")}
        code, out = self.fx.run()
        self.assertEqual(code, 0, out)
        self.assertIn("checks: ok", out)
        self.assertIn("files: 5 ids written (4 jobs, 1 agents), 1 duplicates re-minted, 1 skipped", out)
        self.assertEqual(before, {p: p.read_bytes() for p in self.fx.ws.rglob("*.md")})
        self.assertEqual(self.fx.q("SELECT MAX(version) FROM schema_migrations"), [(40,)])
        self.assertFalse((self.fx.home / "backups").exists())

    def test_apply(self):
        code, out = self.fx.run("--apply")
        self.assertEqual(code, 0, out)
        fx = self.fx
        self.assertEqual(fx.q("SELECT MAX(version) FROM schema_migrations"), [(41,)])
        self.assertIn("service stop", (fx.stub.parent / "stub.log").read_text())

        # Files.
        plain = (fx.ws / "plain/JOB.md").read_bytes()
        self.assertRegex(plain, rb"^---\nid: jb_[0-9a-f]{10}\n---\n# no frontmatter\n$")
        self.assertEqual((fx.ws / "dup-a/JOB.md").read_bytes(), b"---\nid: jb_aaaaaaaaaa\n---\nA\n")
        self.assertNotIn(b"jb_aaaaaaaaaa", (fx.ws / "dup-b/JOB.md").read_bytes())
        self.assertEqual(fx.id_of("valid"), "jb_0123456789")
        crlf = (fx.ws / "crlf/JOB.md").read_bytes()
        self.assertTrue(crlf.startswith(f"---\r\nid: {fx.id_of('crlf')}\r\nharness: x\r\nmeta:\r\n  id: inner\r\n".encode()))
        self.assertTrue(fx.id_of("crlf").startswith("jb_"))
        ops = fx.id_of("agents/ops")
        self.assertTrue(ops.startswith("agt_"))
        self.assertIn(f"id: {ops}".encode(), (fx.ws / "agents/ops/AGENT.md").read_bytes())
        self.assertTrue(fx.id_of("gone").startswith("jb_"))

        # Backup.
        (backup,) = (fx.home / "backups").iterdir()
        self.assertTrue((backup / "trebi.db").is_file())
        self.assertEqual((backup / "files" / WS / "plain/JOB.md").read_bytes(), b"# no frontmatter\n")
        self.assertFalse((backup / "files" / WS / "valid/JOB.md").exists())

        # Retired rows.
        retired = dict(fx.q("SELECT name, kind || '|' || active || '|' || path || '|' || owner FROM job_index WHERE retired_at IS NOT NULL"))
        self.assertEqual(retired, {"deleted": "job|0||", "agents/old": "agent|0||"})

        # Fill.
        self.assertEqual(fx.q("SELECT owner_id FROM job_index WHERE name = 'agents/ops/child'"), [(ops,)])
        self.assertEqual(fx.q("SELECT job_id FROM runs WHERE id = 'r3'"), [(ops,)])
        self.assertEqual(fx.q("SELECT job_id FROM runs WHERE id = 'r2'"), [(fx.id_of("deleted"),)])
        self.assertEqual(fx.q("SELECT id, job_id FROM inbox_items ORDER BY id"), [("i1", fx.id_of("plain")), ("i2", None), ("i3", None)])
        self.assertEqual(fx.q("SELECT agent_id FROM agent_proposals"), [(ops,)])
        self.assertEqual(fx.q("SELECT id, operator_id FROM conversations ORDER BY id"), [("c1", ""), ("c2", ops)])
        self.assertEqual(fx.q("SELECT id, agent_id FROM email_messages ORDER BY id"), [("m1", ops), ("m2", None), ("m3", fx.id_of("agents/old"))])

        # JSON.
        log = fx.q("SELECT entity, entity_id, attrs_json FROM event_log ORDER BY seq")
        child = fx.id_of("agents/ops/child")
        self.assertEqual(log[0], ("job", fx.id_of("plain"), f'{{"job_id":"{fx.id_of("plain")}","job_name":"plain","workspace":"{WS}"}}'))
        self.assertEqual(log[1], ("agent", ops, f'{{"agent_id":"{ops}","agent_name":"ops","workspace":"{WS}"}}'))
        self.assertEqual(log[2][2], f'{{"job_id":"{child}","job_name":"agents/ops/child","note":"a\\u003cb","owner_id":"{ops}","workspace":"{WS}"}}')
        self.assertEqual(json.loads(log[3][2])["job_id"], child)
        self.assertEqual(log[4], ("job", "nobody", '{"job_name":"nobody"}'))
        self.assertEqual(log[5][2], '{"name": "W"}')
        ev = fx.q("SELECT detail_json FROM run_events ORDER BY id")
        self.assertEqual(json.loads(ev[0][0]), {"job_id": child, "job_name": "agents/ops/child", "parent_agent_id": ops, "parent_agent_name": "ops", "reason": "manual"})
        self.assertEqual(ev[1][0], '{"delta": "true", "text": "agent"}')
        self.assertEqual(ev[2][0], '{"operator_id":"","operator_name":""}')

    def test_assistant_rows_use_the_workspace_id(self):
        c = sqlite3.connect(self.fx.db_path)
        c.executemany("INSERT INTO event_queue VALUES (?,?,?,?)", [("qa1", WS, "assistant", "assistant"), ("qa2", WS, "", "assistant"), ("qa3", WS, "assistant", "job")])
        c.execute("INSERT INTO event_outcomes VALUES ('ea', ?, 'assistant')", (WS,))
        c.execute("INSERT INTO schedules VALUES (?, 'assistant', 0)", (WS,))
        c.executemany(
            "INSERT INTO event_log (entity, entity_id, attrs_json) VALUES (?,?,?)",
            [("job", "assistant", json.dumps({"job": "assistant", "workspace": WS})), ("queue", "qa1", json.dumps({"job": "assistant", "workspace": WS}))],
        )
        c.commit()
        c.close()
        code, out = self.fx.run("--apply")
        self.assertEqual(code, 0, out)
        self.assertIn("retired: 2 names", out)
        self.assertIn("unresolved: 2 ", out)  # the two names of the base fixture only
        self.assertEqual(self.fx.q("SELECT id, job_id FROM event_queue WHERE id LIKE 'qa%' ORDER BY id"), [("qa1", WS), ("qa2", WS), ("qa3", WS)])
        self.assertEqual(self.fx.q("SELECT job_id FROM event_outcomes WHERE event_id = 'ea'"), [(WS,)])
        self.assertEqual(self.fx.q("SELECT job_id FROM schedules WHERE job = 'assistant'"), [(WS,)])
        self.assertEqual(self.fx.q("SELECT COUNT(*) FROM job_index WHERE name = 'assistant'"), [(0,)])
        log = self.fx.q("SELECT entity_id, attrs_json FROM event_log WHERE entity IN ('job', 'queue') AND attrs_json LIKE '%assistant%' ORDER BY seq")
        want = f'{{"job_id":"{WS}","job_name":"assistant","workspace":"{WS}"}}'
        self.assertEqual(log, [(WS, want), ("qa1", want)])

    def test_second_apply_changes_nothing(self):
        self.assertEqual(self.fx.run("--apply")[0], 0)
        files = {p: p.read_bytes() for p in self.fx.ws.rglob("*.md")}
        ids = self.fx.q("SELECT workspace_id, name, id, owner_id FROM job_index ORDER BY 1, 2")
        log = self.fx.q("SELECT * FROM event_log")
        code, out = self.fx.run("--apply")
        self.assertEqual(code, 0, out)
        self.assertIn("files: 0 ids written", out)
        self.assertIn("retired: 0 names", out)
        self.assertIn("rewritten: event_log 0 rows, run_events 0 rows", out)
        self.assertEqual(files, {p: p.read_bytes() for p in self.fx.ws.rglob("*.md")})
        self.assertEqual(ids, self.fx.q("SELECT workspace_id, name, id, owner_id FROM job_index ORDER BY 1, 2"))
        self.assertEqual(log, self.fx.q("SELECT * FROM event_log"))

    def test_wrong_version(self):
        c = sqlite3.connect(self.fx.db_path)
        c.execute("INSERT INTO schema_migrations VALUES (43, ?)", (T,))
        c.commit()
        c.close()
        code, out = self.fx.run()
        self.assertEqual(code, 2)
        self.assertIn("schema is v43", out)

    def test_check_failure_rolls_back(self):
        m = mig.Migration(sqlite3.connect(self.fx.db_path, isolation_level=None), apply=True)
        m.version()
        m.add_columns()
        m.plan_files()
        m.db.execute("INSERT INTO conversations (id, workspace_id, operator) VALUES ('c9', ?, 'agents/x')", (WS,))
        # Hide one name from the retired step, so that a check fails.
        orig = m._retire
        m._retire = lambda names: orig({k for k in names if k[1] != "agents/x"})
        with self.assertRaises(mig.CheckFailed):
            m.migrate_rows()
        self.assertEqual(self.fx.q("SELECT COUNT(*) FROM runs WHERE job_id IS NOT NULL"), [(0,)])
        self.assertTrue(any("conversations.operator_id is empty" in f for f in m.report.failures))

    def test_lock_timeout(self):
        lock = open(self.fx.home / "daemon.lock", "a+b")  # noqa: SIM115 - closed in finally
        fcntl.flock(lock.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
        old = mig.SERVICE_TIMEOUT_S
        mig.SERVICE_TIMEOUT_S = 0.5
        try:
            code, out = self.fx.run("--apply")
        finally:
            mig.SERVICE_TIMEOUT_S = old
            lock.close()
        self.assertEqual(code, 2)
        self.assertIn("daemon.lock is held", out)
        self.assertEqual(self.fx.q("SELECT MAX(version) FROM schema_migrations"), [(40,)])


if __name__ == "__main__":
    unittest.main()

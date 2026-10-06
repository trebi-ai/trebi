#!/usr/bin/env -S uv run --script
# /// script
# requires-python = ">=3.11"
# dependencies = []
# ///
"""One-time migration from job and agent names to static IDs (plan Part F3).

The default is a dry run: the script copies trebi.db to a temporary file, does
the migration on the copy, prints the report, and deletes the copy. It does
not write job files. Pass --apply to migrate.

  uv run scripts/migrate-ids.py                 # dry run: report only
  uv run scripts/migrate-ids.py --apply         # stop the service, back up, migrate
  uv run scripts/migrate-ids.py --home <dir>    # default: $TREBI_HOME or ~/.trebi

--apply does these steps:

  1. Stop the daemon with `$TREBI_SERVICE_CMD service stop` (default `trebi`).
     Wait until <home>/daemon.lock is free (30 s). Hold the lock until the end.
  2. Read schema_migrations. The version must be v40 or v41.
  3. Back up trebi.db and each JOB.md/AGENT.md that changes to
     <home>/backups/pre-ids-<UTC time>/.
  4. At v40: add the v41 columns and record v41.
  5. Write `id:` as the first frontmatter line of each job file.
  6. Add a retired job_index row for each name that has no row.
  7. Fill the ID columns.
  8. Rewrite the JSON of event_log and run_events.
  9. Check the result. On a failure, roll back the database.

Exit codes: 0 = the checks pass. 1 = the checks fail (the database does not
change). 2 = the script cannot start (no database, wrong schema version,
daemon does not stop, file error).

A second --apply on a migrated database mints no new IDs and rewrites nothing.
"""

from __future__ import annotations

import argparse
import fcntl
import json
import os
import re
import secrets
import shlex
import shutil
import sqlite3
import subprocess
import sys
import tempfile
import time
from dataclasses import dataclass, field
from datetime import datetime, timezone
from pathlib import Path

V_PREV = 40  # vN-1
V_IDS = 41  # vN: ID columns (Go auto step, copied here)
V_REBUILD = 42  # vN+1: Go table rebuild at the next daemon start

# Copy of the Go v41 step. Keep it byte-identical to internal/store/schema.go.
V41_DDL = """
ALTER TABLE job_index ADD COLUMN id TEXT;
ALTER TABLE job_index ADD COLUMN owner_id TEXT NOT NULL DEFAULT '';
ALTER TABLE job_index ADD COLUMN retired_at TEXT;
CREATE UNIQUE INDEX job_index_id ON job_index(id) WHERE id IS NOT NULL;
ALTER TABLE runs ADD COLUMN job_id TEXT;
ALTER TABLE event_queue ADD COLUMN job_id TEXT;
ALTER TABLE event_outcomes ADD COLUMN job_id TEXT;
ALTER TABLE schedules ADD COLUMN job_id TEXT;
ALTER TABLE inbox_items ADD COLUMN job_id TEXT;
ALTER TABLE requests ADD COLUMN job_id TEXT;
ALTER TABLE agent_proposals ADD COLUMN agent_id TEXT;
ALTER TABLE conversations ADD COLUMN operator_id TEXT NOT NULL DEFAULT '';
ALTER TABLE email_messages ADD COLUMN agent_id TEXT;
"""

V41_COLUMNS = {
    "job_index": ("id", "owner_id", "retired_at"),
    "runs": ("job_id",),
    "event_queue": ("job_id",),
    "event_outcomes": ("job_id",),
    "schedules": ("job_id",),
    "inbox_items": ("job_id",),
    "requests": ("job_id",),
    "agent_proposals": ("agent_id",),
    "conversations": ("operator_id",),
    "email_messages": ("agent_id",),
}

# Tables with a job_index name in `job`. inbox_items.job is nullable.
JOB_TABLES = ("runs", "event_queue", "event_outcomes", "schedules", "inbox_items", "requests")
# Tables with a short agent name in `agent`. email_messages.agent '' stays NULL.
AGENT_TABLES = ("agent_proposals", "email_messages")

ID_RE = re.compile(r"^(jb|agt)_[0-9a-f]{10}$")
ID_LINE_RE = re.compile(rb"^id:(?:[ \t]|$)")
PREFIX = {"job": "jb_", "agent": "agt_"}
# event_log entities whose row has a workspace_id.
ENTITY_TABLES = {"request": "requests", "conversation": "conversations", "queue": "event_queue", "inbox": "inbox_items", "email": "email_messages"}
REF_KEYS = {"job", "agent", "operator", "owner", "parent_agent"}
SERVICE_TIMEOUT_S = 30
SAMPLE = 10


def now_rfc3339() -> str:
    return datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def is_id(v: object) -> bool:
    return isinstance(v, str) and bool(ID_RE.match(v))


def valid_id(kind: str, v: str | None) -> bool:
    return v is not None and is_id(v) and v.startswith(PREFIX[kind])


def agent_index_name(v: str) -> str:
    """Return the job_index name of an agent: `x` or `agents/x` -> `agents/x`."""
    return v if v.startswith("agents/") else "agents/" + v


def agent_short(v: str) -> str:
    return v.removeprefix("agents/")


def kind_of_name(name: str) -> str:
    parts = name.split("/")
    return "agent" if len(parts) == 2 and parts[0] == "agents" else "job"


def owner_of_name(name: str) -> str:
    parts = name.split("/")
    return "/".join(parts[:2]) if len(parts) > 2 and parts[0] == "agents" else ""


def go_json(d: dict) -> str:
    """Encode like Go json.Marshal of a map: sorted keys, compact, HTML escaped."""
    s = json.dumps(d, sort_keys=True, separators=(",", ":"), ensure_ascii=False)
    for a, b in (("<", "\\u003c"), (">", "\\u003e"), ("&", "\\u0026"), (" ", "\\u2028"), (" ", "\\u2029")):
        s = s.replace(a, b)
    return s


# ── frontmatter ────────────────────────────────────────────────────────────


@dataclass
class Frontmatter:
    """Text view of a job file. It keeps all bytes."""

    data: bytes
    has_block: bool = False
    body_start: int = 0  # first byte after the opening line
    body_end: int = 0  # first byte of the closing line
    eol: bytes = b"\n"

    @classmethod
    def parse(cls, data: bytes) -> Frontmatter | None:
        """Return None when the file opens a block that does not close."""
        fm = cls(data)
        if not data.startswith(b"---"):
            first, sep, _ = data.partition(b"\n")
            fm.eol = b"\r\n" if sep and first.endswith(b"\r") else b"\n"
            return fm
        nl = data.find(b"\n")
        if nl < 0 or data[:nl].rstrip(b" \t\r") != b"---":
            return None
        fm.eol = b"\r\n" if data[:nl].endswith(b"\r") else b"\n"
        start = idx = nl + 1
        while idx <= len(data):
            end = data.find(b"\n", idx)
            line = data[idx:] if end < 0 else data[idx:end]
            if line.rstrip(b" \t\r") == b"---":
                fm.has_block, fm.body_start, fm.body_end = True, start, idx
                return fm
            if end < 0:
                break
            idx = end + 1
        return None

    def lines(self) -> list[bytes]:
        return self.data[self.body_start : self.body_end].splitlines(keepends=True)

    def id(self) -> str | None:
        for line in self.lines():
            if ID_LINE_RE.match(line):
                v = line[3:].split(b" #", 1)[0].strip().strip(b"\"'")
                return v.decode("utf-8", "replace")
        return None

    def with_id(self, new_id: str) -> bytes:
        line = b"id: " + new_id.encode() + self.eol
        if not self.has_block:
            return b"---" + self.eol + line + b"---" + self.eol + self.data
        kept = [ln for ln in self.lines() if not ID_LINE_RE.match(ln)]
        return self.data[: self.body_start] + line + b"".join(kept) + self.data[self.body_end :]


def write_atomic(path: Path, data: bytes) -> None:
    mode = path.stat().st_mode & 0o7777
    fd, tmp = tempfile.mkstemp(dir=path.parent, prefix="." + path.name + ".", suffix=".tmp")
    try:
        with os.fdopen(fd, "wb") as f:
            f.write(data)
            f.flush()
            os.fsync(f.fileno())
        os.chmod(tmp, mode)
        os.replace(tmp, path)
    except BaseException:
        Path(tmp).unlink(missing_ok=True)
        raise


# ── daemon control ─────────────────────────────────────────────────────────


class Daemon:
    """Stops the service and holds daemon.lock while the script runs."""

    def __init__(self, home: Path):
        self.home = home
        self.lock_path = home / "daemon.lock"
        self.cmd = shlex.split(os.environ.get("TREBI_SERVICE_CMD", "trebi"))
        self.held = None

    def _run(self, *args: str) -> subprocess.CompletedProcess:
        env = dict(os.environ, TREBI_HOME=str(self.home))
        return subprocess.run([*self.cmd, *args], capture_output=True, text=True, env=env, timeout=60, check=False)

    def _try_lock(self):
        try:
            f = open(self.lock_path, "r+b")  # noqa: SIM115 - the lock stays open
        except FileNotFoundError:
            f = open(self.lock_path, "a+b")  # noqa: SIM115 - the lock stays open
        try:
            fcntl.flock(f.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            f.close()
            return None
        return f

    def running(self) -> bool:
        try:
            p = self._run("service", "status", "--json")
            out = json.loads(p.stdout or "{}")
            if "api" in out or "running" in str(out.get("os", "")).lower():
                return True
        except (OSError, subprocess.SubprocessError, ValueError):
            pass
        f = self._try_lock()
        if f is None:
            return True
        f.close()
        return False

    def stop_and_lock(self) -> None:
        """Raise RuntimeError when the lock stays held after the timeout."""
        if self.running():
            print(f"daemon: stop with `{shlex.join(self.cmd)} service stop`")
            try:
                p = self._run("service", "stop")
                if p.returncode != 0:
                    print(f"daemon: service stop exit {p.returncode}: {(p.stderr or p.stdout).strip()}")
            except (OSError, subprocess.SubprocessError) as e:
                print(f"daemon: service stop failed: {e}")
        deadline = time.monotonic() + SERVICE_TIMEOUT_S
        while True:
            f = self._try_lock()
            if f is not None:
                self.held = f
                return
            if time.monotonic() > deadline:
                pid = self.lock_path.read_text(errors="replace").strip() or "unknown"
                raise RuntimeError(f"{self.lock_path} is held after {SERVICE_TIMEOUT_S} s (pid {pid}). Stop that process, then run the script again.")
            time.sleep(0.25)

    def release(self) -> None:
        if self.held is not None:
            fcntl.flock(self.held.fileno(), fcntl.LOCK_UN)
            self.held.close()
            self.held = None


# ── migration ──────────────────────────────────────────────────────────────


class CheckFailed(Exception):
    pass


class SetupError(Exception):
    pass


@dataclass
class FileChange:
    workspace_id: str
    name: str
    path: Path
    kind: str
    new_id: str
    data: bytes
    duplicate: bool


@dataclass
class Report:
    from_version: int = 0
    files_jobs: int = 0
    files_agents: int = 0
    duplicates: int = 0
    skipped: int = 0
    retired: list[tuple[str, str, str]] = field(default_factory=list)  # (id, ws, name)
    filled: dict[str, int] = field(default_factory=dict)
    rewritten: dict[str, int] = field(default_factory=dict)
    unresolved: int = 0
    failures: list[str] = field(default_factory=list)
    backup: Path | None = None

    def print(self, apply: bool) -> None:
        if self.from_version == V_PREV:
            schema = f"v{V_PREV} -> v{V_IDS} (vN)"
        else:
            schema = f"v{V_IDS} (vN)"
        print(f"mode: {'apply' if apply else 'dry run (no writes)'}")
        print(f"schema: {schema}, v{V_REBUILD} (vN+1) runs at next daemon start")
        n = self.files_jobs + self.files_agents
        print(f"files: {n} ids written ({self.files_jobs} jobs, {self.files_agents} agents), {self.duplicates} duplicates re-minted, {self.skipped} skipped")
        sample = ", ".join(f"{i}: {nm}" for i, _, nm in self.retired[:5])
        more = ", …" if len(self.retired) > 5 else ""
        print(f"retired: {len(self.retired)} names" + (f" ({sample}{more})" if self.retired else ""))
        print("filled: " + ", ".join(f"{t} {c}" for t, c in self.filled.items()))
        print("rewritten: " + ", ".join(f"{t} {c} rows" for t, c in self.rewritten.items()))
        print(f"unresolved: {self.unresolved} JSON names have no workspace (kept as *_name only)")
        print("checks: " + ("ok" if not self.failures else "FAILED"))
        for f in self.failures:
            print("  " + f)
        print(f"backup: {self.backup}" if self.backup else "backup: none (dry run)")


class Migration:
    """Owns one database connection and the name -> ID map."""

    def __init__(self, conn: sqlite3.Connection, apply: bool, verbose: bool = False):
        self.db = conn
        self.apply = apply
        self.verbose = verbose
        self.report = Report()
        self.ids: dict[tuple[str, str], str] = {}  # (workspace_id, job_index name) -> ID
        self.names: dict[str, str] = {}  # ID -> job_index name
        self.taken: set[str] = set()
        self.changes: list[FileChange] = []
        self.index_ids: dict[tuple[str, str], str] = {}  # live rows: final ID
        self.now = now_rfc3339()

    # step 2
    def version(self) -> int:
        try:
            v = self.db.execute("SELECT MAX(version) FROM schema_migrations").fetchone()[0]
        except sqlite3.Error as e:
            raise SetupError(f"cannot read schema_migrations: {e}") from e
        if v not in (V_PREV, V_IDS):
            hint = "Start the current daemon once to migrate it." if (v or 0) < V_PREV else "The ID migration is done or needs a newer script."
            raise SetupError(f"schema is v{v}; this script needs v{V_PREV} or v{V_IDS}. {hint}")
        self.report.from_version = v
        return v

    # step 4
    def add_columns(self) -> None:
        if self.report.from_version == V_PREV:
            self.db.execute("BEGIN IMMEDIATE")
            try:
                for stmt in filter(None, (s.strip() for s in V41_DDL.split(";"))):
                    self.db.execute(stmt)
                self.db.execute("INSERT INTO schema_migrations(version, applied_at) VALUES (?, ?)", (V_IDS, self.now))
                self.db.execute("COMMIT")
            except sqlite3.Error:
                self.db.execute("ROLLBACK")
                raise
        for table, cols in V41_COLUMNS.items():
            have = {r[1] for r in self.db.execute(f"PRAGMA table_info({table})")}
            missing = [c for c in cols if c not in have]
            if missing:
                raise SetupError(f"schema v{V_IDS}: {table} has no column {', '.join(missing)}")

    # step 5 (plan only; write_files writes)
    def plan_files(self) -> None:
        rows = self.db.execute("SELECT workspace_id, name, path, kind, parse_error, id, retired_at FROM job_index").fetchall()
        live = [r for r in rows if r[6] is None]
        for ws, name, _, _, _, rid, ret in rows:
            if ret is not None and rid:
                self._bind(ws, name, rid)
        found: list[tuple[str, str, Path, str, Frontmatter, str | None]] = []
        index_only = []
        for ws, name, path, kind, perr, rid, _ in live:
            p = Path(path) if path else None
            fm = None
            if p is not None and p.is_file() and not perr:
                try:
                    fm = Frontmatter.parse(p.read_bytes())
                except OSError:
                    fm = None
            if fm is None:
                index_only.append((ws, name, p, kind, rid))
                continue
            fkind = "agent" if p.name == "AGENT.md" else "job"
            found.append((ws, name, p, fkind, fm, fm.id()))
        found.sort(key=lambda r: str(r[2]))
        for ws, name, p, kind, fm, fid in found:
            if valid_id(kind, fid) and fid not in self.taken:
                self._bind(ws, name, fid)
                continue
            dup = valid_id(kind, fid)
            new = self._mint(kind)
            self._bind(ws, name, new)
            self.changes.append(FileChange(ws, name, p, kind, new, fm.with_id(new), dup))
            if dup:
                self.report.duplicates += 1
            if kind == "agent":
                self.report.files_agents += 1
            else:
                self.report.files_jobs += 1
        for ws, name, p, kind, rid in sorted(index_only, key=lambda r: (r[0], r[1])):
            if p is not None:
                kind = "agent" if p.name == "AGENT.md" else "job"
            self.report.skipped += 1
            self._bind(ws, name, rid if valid_id(kind, rid) and rid not in self.taken else self._mint(kind))
        for ws, name, *_ in live:
            self.index_ids[(ws, name)] = self.ids[(ws, name)]
        if self.verbose:
            for c in self.changes:
                print(f"file: {c.path}: id {c.new_id}" + (" (duplicate)" if c.duplicate else ""))

    def _mint(self, kind: str) -> str:
        while True:
            v = PREFIX[kind] + secrets.token_hex(5)
            if v not in self.taken:
                self.taken.add(v)
                return v

    def _bind(self, ws: str, name: str, v: str) -> None:
        self.ids[(ws, name)] = v
        self.names[v] = name
        self.taken.add(v)

    def workspace_roots(self) -> dict[str, Path]:
        return {ws: Path(p) for ws, p in self.db.execute("SELECT id, path FROM workspaces")}

    # step 3 (files part)
    def backup_files(self, dest: Path) -> None:
        roots = self.workspace_roots()
        for c in self.changes:
            root = roots.get(c.workspace_id)
            try:
                rel = c.path.relative_to(root) if root else Path("_abs") / c.path.relative_to("/")
            except ValueError:
                rel = Path("_abs") / c.path.relative_to("/")
            target = dest / "files" / c.workspace_id / rel
            target.parent.mkdir(parents=True, exist_ok=True)
            shutil.copy2(c.path, target)

    def write_files(self) -> None:
        for c in self.changes:
            write_atomic(c.path, c.data)

    # steps 6-9, one transaction
    def migrate_rows(self) -> None:
        self.db.execute("BEGIN IMMEDIATE")
        try:
            self._store_index_ids()
            self._retire(self._table_names())
            self.runs = {r[0]: (r[1], r[2]) for r in self.db.execute("SELECT id, workspace_id, job FROM runs")}
            self._retire(self._json_names())
            self._fill()
            self._rewrite_json()
            self._check()
        except BaseException:
            self.db.execute("ROLLBACK")
            raise
        self.db.execute("COMMIT")

    def _store_index_ids(self) -> None:
        cur = {(ws, n): i for ws, n, i in self.db.execute("SELECT workspace_id, name, id FROM job_index WHERE retired_at IS NULL")}
        moved = [k for k, v in self.index_ids.items() if cur.get(k) != v]
        # Clear first, so a swap of two IDs does not hit the unique index.
        self.db.executemany("UPDATE job_index SET id = NULL WHERE workspace_id = ? AND name = ?", moved)
        self.db.executemany("UPDATE job_index SET id = ? WHERE workspace_id = ? AND name = ?", [(self.index_ids[k], *k) for k in moved])
        self.report.filled["job_index"] = len(moved)

    def _table_names(self) -> set[tuple[str, str]]:
        out: set[tuple[str, str]] = set()
        for t in JOB_TABLES:
            out.update(self.db.execute(f"SELECT DISTINCT workspace_id, job FROM {t} WHERE job_id IS NULL AND job IS NOT NULL AND job <> '' AND workspace_id IS NOT NULL"))
        for t in AGENT_TABLES:
            for ws, a in self.db.execute(f"SELECT DISTINCT workspace_id, agent FROM {t} WHERE agent_id IS NULL AND agent <> ''"):
                out.add((ws, agent_index_name(a)))
        for ws, op in self.db.execute("SELECT DISTINCT workspace_id, operator FROM conversations WHERE operator <> '' AND operator_id = ''"):
            out.add((ws, agent_index_name(op)))
        for ws, ow in self.db.execute("SELECT DISTINCT workspace_id, owner FROM job_index WHERE owner <> '' AND owner_id = ''"):
            out.add((ws, ow))
        return out

    def _retire(self, names: set[tuple[str, str]]) -> None:
        missing = sorted(k for k in names if k not in self.ids)
        # Agents first, so that their child jobs find the owner.
        missing.sort(key=lambda k: kind_of_name(k[1]) != "agent")
        for ws, name in missing:
            kind = kind_of_name(name)
            new = self._mint(kind)
            self._bind(ws, name, new)
            owner = owner_of_name(name)
            owner_id = self.ids.get((ws, owner), "") if owner else ""
            if not owner_id:
                owner = ""
            self.db.execute(
                "INSERT INTO job_index (workspace_id, name, path, kind, active, updated_at, owner, id, owner_id, retired_at) VALUES (?, ?, '', ?, 0, ?, ?, ?, ?, ?)",
                (ws, name, kind, self.now, owner, new, owner_id, self.now),
            )
            self.report.retired.append((new, ws, name))
            if self.verbose:
                print(f"retired: {ws} {name} -> {new}")

    def _fill(self) -> None:
        f = self.report.filled
        cur = self.db.execute(
            "UPDATE job_index SET owner_id = (SELECT a.id FROM job_index a WHERE a.workspace_id = job_index.workspace_id AND a.name = job_index.owner) "
            "WHERE owner <> '' AND owner_id = '' AND EXISTS (SELECT 1 FROM job_index a WHERE a.workspace_id = job_index.workspace_id AND a.name = job_index.owner AND a.id IS NOT NULL)"
        )
        f["job_index"] = f.get("job_index", 0) + cur.rowcount
        sub = "(SELECT j.id FROM job_index j WHERE j.workspace_id = {t}.workspace_id AND j.name = {n})"
        for t in JOB_TABLES:
            q = sub.format(t=t, n=f"{t}.job")
            cur = self.db.execute(f"UPDATE {t} SET job_id = {q} WHERE job_id IS NULL AND job IS NOT NULL AND job <> '' AND {q} IS NOT NULL")
            f[t] = cur.rowcount
        agent_expr = "CASE WHEN {t}.{c} LIKE 'agents/%' THEN {t}.{c} ELSE 'agents/' || {t}.{c} END"
        for t in AGENT_TABLES:
            q = sub.format(t=t, n=agent_expr.format(t=t, c="agent"))
            cur = self.db.execute(f"UPDATE {t} SET agent_id = {q} WHERE agent_id IS NULL AND agent <> '' AND {q} IS NOT NULL")
            f[t] = cur.rowcount
        q = sub.format(t="conversations", n=agent_expr.format(t="conversations", c="operator"))
        cur = self.db.execute(f"UPDATE conversations SET operator_id = {q} WHERE operator <> '' AND operator_id = '' AND {q} IS NOT NULL")
        f["conversations"] = cur.rowcount
        order = ("job_index", *JOB_TABLES[:5], "requests", "agent_proposals", "conversations", "email_messages")
        self.report.filled = {k: f.get(k, 0) for k in dict.fromkeys(order)}

    # JSON
    def _context(self, table: str, row: tuple, d: dict) -> tuple[str | None, str | None]:
        """Return (workspace_id, job of the run) for one JSON row."""
        ws = None
        for k in ("workspace", "workspace_id"):
            v = d.get(k)
            if isinstance(v, str) and v.startswith("ws_"):
                ws = v
                break
        run_id = row[1] if table == "run_events" else (row[2] if row[1] == "run" else d.get("run_id"))
        run = self.runs.get(run_id) if isinstance(run_id, str) else None
        if run is not None and ws is None:
            ws = run[0]
        if ws is None and table == "event_log" and row[1] in ENTITY_TABLES:
            ws = self._entity_ws(row[1], row[2])
        run_job = run[1] if run is not None and run[0] == ws else None
        return ws, run_job

    def _entity_ws(self, entity: str, eid: str | None) -> str | None:
        if eid is None:
            return None
        r = self.db.execute(f"SELECT workspace_id FROM {ENTITY_TABLES[entity]} WHERE id = ?", (eid,)).fetchone()
        return r[0] if r and r[0] else None

    def _lookup_job(self, ws: str, name: str, run_job: str | None, want: set) -> str | None:
        v = self.ids.get((ws, name))
        if v:
            return v
        # A launch detail can carry an old path; the run row has the job.
        if run_job and name.rsplit("/", 1)[-1] == run_job.rsplit("/", 1)[-1]:
            v = self.ids.get((ws, run_job))
            if v:
                return v
            want.add((ws, run_job))
            return None
        want.add((ws, name))
        return None

    def _rewrite_dict(self, d: dict, ws: str | None, run_job: str | None, entity: str | None, want: set) -> tuple[bool, int]:
        """Rewrite the top-level keys of d in place. Return (changed, unresolved)."""
        changed, unresolved = False, 0

        def put(prefix: str, ref: str, kind: str, keep_name: bool = True) -> None:
            nonlocal unresolved
            if kind == "agent":
                if ref == "":
                    d.setdefault(prefix + "_id", "")
                    if keep_name:
                        d.setdefault(prefix + "_name", "")
                    return
                if is_id(ref):
                    i, label = ref, self.names.get(ref)
                else:
                    label = agent_index_name(ref)
                    i = None
                    if ws is not None:
                        i = self.ids.get((ws, label))
                        if i is None:
                            want.add((ws, label))
                label = agent_short(label) if label else None
            else:
                if is_id(ref):
                    i, label = ref, self.names.get(ref)
                else:
                    label = ref
                    i = self._lookup_job(ws, ref, run_job, want) if ws is not None else None
                    if i is not None:
                        label = self.names.get(i, ref)
            if i is not None:
                d.setdefault(prefix + "_id", i)
            elif ws is None:
                unresolved += 1
            if (keep_name or i is None) and label is not None:
                d.setdefault(prefix + "_name", label)

        for key, kind, keep in (("job", "job", True), ("agent", "agent", True), ("operator", "agent", True), ("owner", "agent", False), ("parent_agent", "agent", True)):
            v = d.get(key)
            if isinstance(v, str):
                del d[key]
                put(key, v, kind, keep)
                changed = True
        if entity in ("job", "agent") and isinstance(d.get("name"), str):
            v = d.pop("name")
            put(entity, v, entity if entity == "agent" else "job")
            changed = True
        return changed, unresolved

    def _json_rows(self, table: str):
        if table == "event_log":
            return self.db.execute("SELECT seq, entity, entity_id, attrs_json FROM event_log WHERE attrs_json IS NOT NULL OR entity IN ('job', 'agent')")
        return self.db.execute("SELECT id, run_id, detail_json FROM run_events WHERE detail_json IS NOT NULL")

    def _rewrite_row(self, table: str, row: tuple, want: set) -> tuple[str | None, str | None, bool, int]:
        """Return (new json, new entity_id, changed, unresolved)."""
        raw = row[-1]
        d = None
        if raw:
            try:
                d = json.loads(raw)
            except ValueError:
                d = None
        if not isinstance(d, dict):
            d = None
        entity = row[1] if table == "event_log" else None
        if entity not in ("job", "agent") and not (d and REF_KEYS & d.keys()):
            return raw, (row[2] if table == "event_log" else None), False, 0
        ws, run_job = self._context(table, row, d or {})
        changed, unresolved = (False, 0)
        if d is not None:
            changed, unresolved = self._rewrite_dict(d, ws, run_job, entity, want)
        eid = row[2] if table == "event_log" else None
        new_eid = eid
        if entity in ("job", "agent") and isinstance(eid, str) and eid and not is_id(eid):
            if ws is None:
                unresolved += 1
            elif entity == "agent":
                name = agent_index_name(eid)
                new_eid = self.ids.get((ws, name))
                if new_eid is None:
                    want.add((ws, name))
                    new_eid = eid
            else:
                new_eid = self._lookup_job(ws, eid, run_job, want) or eid
        return (go_json(d) if changed else raw), new_eid, changed or new_eid != eid, unresolved

    def _json_names(self) -> set[tuple[str, str]]:
        want: set[tuple[str, str]] = set()
        for table in ("event_log", "run_events"):
            for row in self._json_rows(table).fetchall():
                self._rewrite_row(table, row, want)
        return want

    def _rewrite_json(self) -> None:
        want: set = set()
        for table in ("event_log", "run_events"):
            n = 0
            updates = []
            for row in self._json_rows(table).fetchall():
                new_json, new_eid, changed, unresolved = self._rewrite_row(table, row, want)
                self.report.unresolved += unresolved
                if not changed:
                    continue
                n += 1
                if table == "event_log":
                    updates.append((new_json, new_eid, row[0]))
                else:
                    updates.append((new_json, row[0]))
            if table == "event_log":
                self.db.executemany("UPDATE event_log SET attrs_json = ?, entity_id = ? WHERE seq = ?", updates)
            else:
                self.db.executemany("UPDATE run_events SET detail_json = ? WHERE id = ?", updates)
            self.report.rewritten[table] = n
        if want:
            raise CheckFailed(f"{len(want)} JSON names have no ID after the retired step: {sorted(want)[:SAMPLE]}")

    def _check(self) -> None:
        checks = [
            ("job_index.id is NULL", "SELECT workspace_id, name FROM job_index WHERE id IS NULL"),
            ("job_index: two live rows have one ID", "SELECT id, COUNT(*) FROM job_index WHERE retired_at IS NULL GROUP BY id HAVING COUNT(*) > 1"),
            ("job_index.owner_id is empty", "SELECT workspace_id, name, owner FROM job_index WHERE owner <> '' AND owner_id = ''"),
            ("conversations.operator_id is empty", "SELECT id, workspace_id, operator FROM conversations WHERE operator <> '' AND operator_id = ''"),
        ]
        for t in JOB_TABLES:
            checks.append((f"{t}.job_id is NULL", f"SELECT workspace_id, job FROM {t} WHERE job_id IS NULL AND job IS NOT NULL AND job <> ''"))
        for t in AGENT_TABLES:
            checks.append((f"{t}.agent_id is NULL", f"SELECT workspace_id, agent FROM {t} WHERE agent_id IS NULL AND agent <> ''"))
        refs = [(t, "job_id") for t in JOB_TABLES] + [(t, "agent_id") for t in AGENT_TABLES] + [("conversations", "operator_id"), ("job_index", "owner_id")]
        for t, c in refs:
            checks.append((f"{t}.{c} is not in job_index", f"SELECT {c} FROM {t} WHERE {c} IS NOT NULL AND {c} <> '' AND {c} NOT IN (SELECT id FROM job_index WHERE id IS NOT NULL)"))
        for t, c in refs[len(JOB_TABLES) :]:
            checks.append((f"{t}.{c} is not an agent ID", f"SELECT {c} FROM {t} WHERE {c} IS NOT NULL AND {c} <> '' AND {c} NOT LIKE 'agt\\_%' ESCAPE '\\'"))
        for label, q in checks:
            rows = self.db.execute(q).fetchmany(SAMPLE)
            if rows:
                self.report.failures.append(f"{label}: {rows}")
        if self.report.failures:
            raise CheckFailed("checks failed")


# ── commands ───────────────────────────────────────────────────────────────


def default_home() -> Path:
    return Path(os.environ.get("TREBI_HOME") or Path.home() / ".trebi").expanduser()


def connect(path: Path) -> sqlite3.Connection:
    conn = sqlite3.connect(path, isolation_level=None, timeout=10)
    conn.execute("PRAGMA busy_timeout = 10000")
    return conn


def backup_db(src: Path, dest: Path) -> None:
    s = sqlite3.connect(f"file:{src}?mode=ro", uri=True)
    d = sqlite3.connect(dest)
    try:
        s.backup(d)
    finally:
        d.close()
        s.close()


def run_steps(m: Migration, before_rows=None) -> int:
    m.add_columns()
    m.plan_files()
    if before_rows:
        before_rows(m)
    try:
        m.migrate_rows()
    except CheckFailed as e:
        if not m.report.failures:
            m.report.failures.append(str(e))
        return 1
    return 0


def dry_run(home: Path, db_path: Path, verbose: bool) -> int:
    with tempfile.TemporaryDirectory(prefix="trebi-ids-") as tmp:
        copy = Path(tmp) / "trebi.db"
        backup_db(db_path, copy)
        conn = connect(copy)
        try:
            m = Migration(conn, apply=False, verbose=verbose)
            m.version()
            code = run_steps(m)
        finally:
            conn.close()
    m.report.print(apply=False)
    print("\nRun with --apply to migrate.")
    return code


def apply(home: Path, db_path: Path, verbose: bool) -> int:
    daemon = Daemon(home)
    daemon.stop_and_lock()
    try:
        conn = connect(db_path)
        try:
            m = Migration(conn, apply=True, verbose=verbose)
            m.version()
            stamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")
            dest = home / "backups" / f"pre-ids-{stamp}"
            n = 1
            while dest.exists():
                n += 1
                dest = home / "backups" / f"pre-ids-{stamp}-{n}"
            dest.mkdir(parents=True)
            backup_db(db_path, dest / "trebi.db")
            m.report.backup = dest

            def files(m: Migration) -> None:
                m.backup_files(dest)
                m.write_files()

            code = run_steps(m, before_rows=files)
            if code == 0:
                conn.execute("PRAGMA wal_checkpoint(TRUNCATE)")
        finally:
            conn.close()
    finally:
        daemon.release()
    m.report.print(apply=True)
    if code != 0:
        print("\nThe database did not change. The files keep their new IDs. Fix the rows above, then run the script again.")
        return code
    print(
        f"""
Next:
  1. Build and install the new binary: cd ../trebi-pro && scripts/build.sh
     The daemon starts and runs the v{V_REBUILD} table rebuild.
  2. Check: trebi doctor
  3. Undo: stop the daemon, delete {db_path}-wal and {db_path}-shm, copy {dest}/trebi.db back,
     and copy each file under {dest}/files/<workspace_id>/ back to that workspace folder."""
    )
    return 0


def main(argv: list[str] | None = None) -> int:
    p = argparse.ArgumentParser(description="One-time migration from job and agent names to IDs (dry run is the default).")
    p.add_argument("--home", type=Path, default=None, help="trebi home (default: $TREBI_HOME or ~/.trebi)")
    p.add_argument("--apply", action="store_true", help="stop the service, back up, and migrate")
    p.add_argument("-v", "--verbose", action="store_true", help="print each file change and each retired name")
    args = p.parse_args(argv)
    home = (args.home or default_home()).expanduser().resolve()
    db_path = home / "trebi.db"
    if not db_path.is_file():
        print(f"error: {db_path} does not exist", file=sys.stderr)
        return 2
    try:
        return apply(home, db_path, args.verbose) if args.apply else dry_run(home, db_path, args.verbose)
    except (SetupError, RuntimeError, OSError, sqlite3.Error) as e:
        print(f"error: {e}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    sys.exit(main())

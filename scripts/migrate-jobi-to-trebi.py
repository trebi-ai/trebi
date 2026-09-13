#!/usr/bin/env -S uv run --script
# /// script
# requires-python = ">=3.11"
# dependencies = ["ruamel.yaml>=0.18"]
# ///
"""jobi → trebi migration.

Default is dry-run: prints the plan and never writes. Pass --apply to execute.

  uv run scripts/migrate-jobi-to-trebi.py status
  uv run scripts/migrate-jobi-to-trebi.py migrate --shadow
  uv run scripts/migrate-jobi-to-trebi.py rewrite-jobs --diff
  uv run scripts/migrate-jobi-to-trebi.py rewrite-jobs --job receipts --target-workspace ellis
  uv run scripts/migrate-jobi-to-trebi.py migrate --shadow --job receipts --target-workspace ellis

--job limits file rewrite and DB copy to one job. --target-workspace is the
trebi destination (id, name, or path). Incremental --job --apply copies into
the target and leaves ~/.jobi files untouched. Full rewrite-jobs --apply
without --job still rewrites in place (cutover).

--jobi-dir, --trebi-dir, --job and --target-workspace are accepted on either
side of the subcommand. make-fixture takes only its output directory.
"""

from __future__ import annotations

import argparse
import fcntl
import hashlib
import json
import os
import re
import shlex
import shutil
import socket
import sqlite3
import sys
import urllib.error
import urllib.request
from dataclasses import dataclass, field
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

from ruamel.yaml import YAML

yaml = YAML()
yaml.preserve_quotes = True
yaml.default_flow_style = False
yaml.width = 4096

KNOWN_HARNESSES = (
    "claude",
    "grok",
    "codex",
    "opencode",
    "gemini",
    "cursor",
    "copilot",
    "pi",
    "fake",
)
SENTINEL_OLD = "JOBI_TASK_COMPLETE"
SENTINEL_NEW = "TREBI_RUN_COMPLETE"
TABLE_MAP = {
    "tasks": "runs",
    "task_segments": "run_segments",
    "task_events": "run_events",
    "event_queue": "event_queue",
    "trigger_policy": "dispatch_policy",
    "source_metrics": "connector_metrics",
    "source_cursors": "connector_cursors",
    "schedule_anchors": "schedules",
}
SECRET_KEY_RE = re.compile(r"(TOKEN|KEY|SECRET|PASSWORD|AUTH)", re.I)
EXPECTED_SCHEMA = 10


def default_jobi_dir() -> Path:
    return Path(os.environ.get("JOBI_DIR", Path.home() / ".jobi"))


def default_trebi_dir() -> Path:
    return Path(os.environ.get("TREBI_HOME", Path.home() / ".trebi"))


def now_iso() -> str:
    return datetime.now(timezone.utc).isoformat()


# ── harness ────────────────────────────────────────────────────────────────


def _token_harness(tok: str) -> str | None:
    s = tok.strip().lower()
    if not s:
        return None
    base = Path(s.split("@", 1)[0]).name
    if base in KNOWN_HARNESSES:
        return base
    if "claude-code" in s or base.startswith("claude"):
        return "claude"
    for name in KNOWN_HARNESSES:
        if name in base:
            return name
    return None


def detect_harness(command: Any, tool: Any) -> tuple[str | None, list[str]]:
    """Return (harness_name, notes). Never copies a full argv into harness."""
    notes: list[str] = []
    if tool is not None:
        h = _token_harness(str(tool))
        if h:
            if str(tool).strip() not in (h, ""):
                notes.append(f"tool {tool!r} → harness {h}")
            return h, notes
        notes.append(f"unrecognized tool: {tool!r}")

    if command is None or command == "":
        return None, notes
    if isinstance(command, list):
        cmd = shlex.join(str(x) for x in command)
    else:
        cmd = str(command).strip()
    h = _token_harness(cmd)
    if h and cmd == h:
        return h, notes
    try:
        tokens = shlex.split(cmd)
    except ValueError:
        tokens = cmd.split()
    for tok in tokens:
        h = _token_harness(tok)
        if h:
            notes.append(f"command {cmd!r} → harness {h} (argv not copied)")
            return h, notes
    low = cmd.lower()
    if "claude-code" in low or "anthropic" in low:
        notes.append(f"command {cmd!r} → harness claude (argv not copied)")
        return "claude", notes
    notes.append(f"could not map command to a harness name: {cmd!r}")
    return None, notes


# ── frontmatter ────────────────────────────────────────────────────────────


def split_frontmatter(text: str) -> tuple[str | None, str, str]:
    if not text.startswith("---"):
        return None, "", text
    lines = text.splitlines(keepends=True)
    end = None
    for i in range(1, len(lines)):
        if lines[i].strip() == "---":
            end = i
            break
    if end is None:
        return None, "", text
    fm = "".join(lines[1:end])
    rest = "".join(lines[end + 1 :])
    opening = lines[0]
    closing = lines[end]
    return fm, opening + fm + closing, rest


def load_fm_map(fm_text: str) -> dict[str, Any]:
    data = yaml.load(fm_text) if fm_text.strip() else {}
    return data if isinstance(data, dict) else {}


def _schedule_entry(item: str, top_when: Any) -> dict[str, Any]:
    entry: dict[str, Any] = {}
    s = item.strip()
    if s.startswith("@every"):
        m = re.match(r"@every\s+(\S+)(?:\s+\(([^)]+)\))?", s)
        if m:
            entry["every"] = m.group(1)
            if m.group(2):
                entry["anchor"] = m.group(2)
        else:
            entry["every"] = s.removeprefix("@every").strip()
    else:
        entry["cron"] = s
    if top_when is not None:
        entry["when"] = top_when
    return entry


def rewrite_frontmatter_inplace(data: Any, report: list[str]) -> None:
    if not isinstance(data, dict):
        return

    command = data.get("command")
    tool = data.get("tool")
    harness, notes = detect_harness(command, tool)
    report.extend(notes)
    if harness:
        data["harness"] = harness
    elif "harness" not in data:
        report.append("no harness resolved — job will use defaults.command")

    if "command" in data:
        del data["command"]
    if "tool" in data:
        del data["tool"]

    if "env" in data and isinstance(data["env"], str):
        data["envset"] = data.pop("env")

    sess = data.get("session")
    if isinstance(sess, dict) and "timeout" in sess:
        sess["idle_timeout"] = sess.pop("timeout")

    if "agent_context" in data:
        data["assistant_context"] = data.pop("agent_context")

    if "memory" in data and isinstance(data["memory"], str) and data["memory"].strip():
        data["memory"] = True

    if "triggers" not in data or not isinstance(data.get("triggers"), list):
        triggers: list[Any] = []
        top_when = data.get("when")
        sched = data.get("schedule")
        if sched is not None:
            items = sched if isinstance(sched, list) else [sched]
            for item in items:
                if not isinstance(item, str):
                    report.append(f"non-string schedule entry skipped: {item!r}")
                    continue
                triggers.append(_schedule_entry(item, top_when))
        on_list = data.get("on") or []
        if isinstance(on_list, list):
            for on in on_list:
                if not isinstance(on, dict):
                    continue
                entry: dict[str, Any] = {}
                src = on.get("source") or on.get("event")
                if src:
                    entry["event"] = src
                for k in (
                    "match",
                    "extract",
                    "debounce",
                    "rate_limit",
                    "acknowledge",
                    "typing",
                    "typing_interval",
                    "typing_max",
                    "followup_prompt",
                    "allowlist",
                    "when",
                ):
                    if k in on:
                        entry[k] = on[k]
                triggers.append(entry)
        if triggers:
            data["triggers"] = triggers
        elif top_when is not None and sched is None:
            report.append(
                "top-level when: with no schedule — manual-only gate; needs human decision (B25)"
            )

    for k in ("schedule", "on", "when", "command"):
        if k in data:
            del data[k]


def rewrite_frontmatter_map(data: dict[str, Any], report: list[str]) -> dict[str, Any]:
    from copy import deepcopy

    out = deepcopy(data)
    rewrite_frontmatter_inplace(out, report)
    return out


def dump_frontmatter(data: Any, body: str) -> str:
    from io import StringIO

    buf = StringIO()
    yaml.dump(data, buf)
    fm = buf.getvalue()
    if not fm.endswith("\n"):
        fm += "\n"
    body = body.lstrip("\n")
    return f"---\n{fm}---\n{body}"


def rewrite_job_text(text: str, report: list[str], *, rewrite_sentinel: bool = True) -> tuple[str, bool]:
    fm_raw, _full, body = split_frontmatter(text)
    if fm_raw is None:
        new_body = body
        changed = False
        if rewrite_sentinel and SENTINEL_OLD in body:
            new_body = body.replace(SENTINEL_OLD, SENTINEL_NEW)
            changed = new_body != body
        return new_body if changed else text, changed

    data = yaml.load(fm_raw) if fm_raw.strip() else {}
    if not isinstance(data, dict):
        data = {}
    rewrite_frontmatter_inplace(data, report)
    new_body = body
    if rewrite_sentinel and SENTINEL_OLD in new_body:
        new_body = new_body.replace(SENTINEL_OLD, SENTINEL_NEW)
    new_text = dump_frontmatter(data, new_body)
    return new_text, new_text != text


# ── config / connectors (current trebi shapes) ─────────────────────────────


def command_to_string(cmd: Any) -> str:
    if cmd is None:
        return ""
    if isinstance(cmd, str):
        return cmd
    if isinstance(cmd, list):
        return shlex.join(str(x) for x in cmd)
    return str(cmd)


def convert_source_entry(src: dict[str, Any]) -> dict[str, Any]:
    out: dict[str, Any] = {}
    if src.get("name"):
        out["name"] = src["name"]
    out["type"] = src.get("type") or "stream"
    out["command"] = command_to_string(src.get("command"))
    if src.get("format"):
        out["format"] = src["format"]
    if src.get("cwd"):
        out["cwd"] = src["cwd"]
    if src.get("env"):
        out["env"] = src["env"]
    if src.get("facets"):
        out["facets"] = src["facets"]
    # Preserve jobi-only fields so they are not silently dropped from disk.
    # Current trebi Definition ignores them on load.
    for k in ("cursor", "catchup", "restart"):
        if k not in src:
            continue
        val = src[k]
        if k == "catchup" and isinstance(val, dict) and "command" in val:
            val = dict(val)
            val["command"] = command_to_string(val.get("command"))
        out[k] = val
    return out


def convert_sources_yaml(raw_text: str) -> tuple[Any, list[str]]:
    notes: list[str] = []
    data = yaml.load(raw_text) or {}
    entries: list[Any] = []
    if isinstance(data, dict):
        src = data.get("sources") or data.get("connectors") or []
        if isinstance(src, list):
            entries = src
        elif isinstance(src, dict):
            entries = [{"name": k, **(v if isinstance(v, dict) else {"command": v})} for k, v in src.items()]
    elif isinstance(data, list):
        entries = data
    converted: list[dict[str, Any]] = []
    for item in entries:
        if not isinstance(item, dict):
            notes.append(f"skipped non-object source: {item!r}")
            continue
        conv = convert_source_entry(item)
        if not conv.get("command"):
            notes.append(f"source {conv.get('name', '?')}: empty command after conversion")
        if "catchup" in conv or "cursor" in conv:
            notes.append(
                f"source {conv.get('name', '?')}: cursor/catchup preserved in YAML; "
                "trebi Definition does not load them yet"
            )
        converted.append(conv)
    notes.append(f"connectors: {len(converted)} (command lists joined for sh -c)")
    return {"connectors": converted}, notes


def envsets_secret_key_names(jobi_dir: Path) -> list[str]:
    path = jobi_dir / "envsets.yaml"
    if not path.exists():
        return []
    data = yaml.load(path.read_text(encoding="utf-8", errors="replace")) or {}
    if not isinstance(data, dict):
        return []
    flagged: list[str] = []
    for set_name, kv in data.items():
        if not isinstance(kv, dict):
            continue
        for k in kv:
            if SECRET_KEY_RE.search(str(k)):
                flagged.append(f"{set_name}.{k}")
    return flagged


# ── paths / matching ───────────────────────────────────────────────────────


# ── daemon liveness ────────────────────────────────────────────────────────
#
# trebi has no unix socket: the daemon is an HTTP server on a loopback port
# (internal/api/server.go, net.Listen("tcp", ...)). Liveness is an advisory
# flock on daemon.lock (internal/home/lock_unix.go), with api.json holding the
# url + bearer token. An earlier guard here tested for a daemon.sock that this
# codebase never creates, so it never fired.


def daemon_lock_held(trebi_dir: Path) -> tuple[bool, int | None]:
    """True if another process holds the daemon.lock flock. Mirrors inspectDaemonLock."""
    lock = trebi_dir / "daemon.lock"
    if not lock.exists():
        return False, None
    try:
        fh = open(lock, "r+")
    except OSError:
        return False, None
    try:
        try:
            fcntl.flock(fh.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
        except OSError:
            # Locked by the daemon. The pid is written inside the file.
            try:
                fh.seek(0)
                return True, int(fh.read().strip() or 0) or None
            except (OSError, ValueError):
                return True, None
        fcntl.flock(fh.fileno(), fcntl.LOCK_UN)
        return False, None
    finally:
        fh.close()


def daemon_http_alive(trebi_dir: Path, timeout: float = 2.0) -> bool:
    """Probe GET /v1/system/health with the api.json token. Catches a daemon
    started against this home from elsewhere, whose flock we cannot see."""
    api = trebi_dir / "api.json"
    if not api.exists():
        return False
    try:
        cfg = json.loads(api.read_text())
    except (OSError, json.JSONDecodeError):
        return False
    url = str(cfg.get("url") or "").rstrip("/")
    token = str(cfg.get("token") or "")
    if not url:
        return False
    req = urllib.request.Request(  # noqa: S310 - loopback url from local api.json
        f"{url}/v1/system/health", headers={"Authorization": f"Bearer {token}"}
    )
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:  # noqa: S310
            return 200 <= resp.status < 300
    except urllib.error.HTTPError as e:
        # 401/403 still proves something is listening and serving /v1.
        return e.code in (401, 403)
    except (urllib.error.URLError, TimeoutError, socket.timeout, OSError):
        return False


def refuse_if_daemon_running(trebi_dir: Path, what: str) -> bool:
    """Print why and return True when the daemon must be stopped first.

    The daemon is the single writer: it owns the bus, the scheduler, and the
    queue drainer. This script writes rows with raw SQL and emits no bus
    events, so a live daemon could fire an imported schedule or claim an
    imported queue row mid-import.
    """
    held, pid = daemon_lock_held(trebi_dir)
    if held:
        where = f" (pid {pid})" if pid else ""
        print(
            f"ERROR: trebid holds {trebi_dir / 'daemon.lock'}{where} — "
            f"stop it before {what} (trebi service stop)",
            file=sys.stderr,
        )
        return True
    if daemon_http_alive(trebi_dir):
        print(
            f"ERROR: a trebid is answering /v1/system/health for {trebi_dir} — "
            f"stop it before {what} (trebi service stop)",
            file=sys.stderr,
        )
        return True
    return False


def open_jobi_ro(jobi_dir: Path) -> sqlite3.Connection | None:
    db = jobi_dir / "jobi.db"
    if not db.exists():
        return None
    conn = sqlite3.connect(f"file:{db}?mode=ro", uri=True)
    # jobi rows can contain invalid UTF-8 in prompt_preview / event payloads.
    conn.text_factory = lambda b: b.decode("utf-8", "replace") if isinstance(b, bytes) else b
    return conn


def open_trebi_ro(trebi_dir: Path) -> sqlite3.Connection | None:
    db = trebi_dir / "trebi.db"
    if not db.exists():
        return None
    return sqlite3.connect(f"file:{db}?mode=ro", uri=True)


def table_count(conn: sqlite3.Connection, table: str) -> int:
    try:
        row = conn.execute(f"SELECT COUNT(*) FROM {table}").fetchone()
        return int(row[0]) if row else 0
    except sqlite3.Error:
        return -1


def load_workspaces_yaml(jobi_dir: Path) -> list[dict[str, Any]]:
    path = jobi_dir / "workspaces.yaml"
    if not path.exists():
        return []
    data = yaml.load(path.read_text()) or {}
    raw: list[Any] = []
    if isinstance(data, list):
        raw = data
    elif isinstance(data, dict):
        ws = data.get("workspaces") or data.get("paths") or []
        if isinstance(ws, list):
            raw = ws
        elif isinstance(ws, dict):
            raw = [{"name": k, "path": v} for k, v in ws.items()]
    out: list[dict[str, Any]] = []
    for w in raw:
        if isinstance(w, str):
            p = Path(w)
            if not p.is_absolute():
                p = (jobi_dir / p).resolve()
            out.append({"path": str(p), "name": p.name})
        elif isinstance(w, dict):
            pstr = str(w.get("path") or w.get("dir") or "")
            if pstr:
                p = Path(pstr)
                if not p.is_absolute():
                    p = (jobi_dir / p).resolve()
                entry = dict(w)
                entry["path"] = str(p)
                if not entry.get("name"):
                    entry["name"] = p.name
                out.append(entry)
            else:
                out.append(w)
        else:
            out.append(w)
    return out


def nested_agent_dirs(ws_path: Path) -> list[Path]:
    agents = ws_path / "agents"
    if not agents.is_dir():
        return []
    out: list[Path] = []
    for p in sorted(agents.iterdir()):
        if p.is_dir() and ((p / "AGENT.md").exists() or (p / "TREBI.md").exists()):
            out.append(p)
    return out


def mint_ws_id(path: str) -> str:
    h = hashlib.sha256(path.encode()).hexdigest()[:8]
    return f"ws_{h}"


def workspace_paths_from_jobi(jobi_dir: Path) -> list[Path]:
    out: list[Path] = []
    for w in load_workspaces_yaml(jobi_dir):
        p = w.get("path") or w.get("dir") if isinstance(w, dict) else w
        if p:
            out.append(Path(p))
    return out


def source_workspace_for(path: Path, workspaces: list[Path]) -> Path | None:
    pl = str(path.resolve())
    best: Path | None = None
    for ws in workspaces:
        wpl = str(ws.resolve())
        if pl.startswith(wpl.rstrip("/") + "/") or pl == wpl:
            if best is None or len(wpl) > len(str(best)):
                best = ws
    return best


def job_file_name(path: Path) -> str:
    if path.name in ("JOB.md", "job.md"):
        return path.parent.name
    if path.name in ("AGENT.md", "TREBI.md", "SOUL.md"):
        return path.parent.name
    return path.stem


def job_spec_matches(path: Path, spec: str, source_ws: Path | None) -> bool:
    spec = spec.strip()
    if not spec:
        return True
    if path.as_posix() == spec or str(path) == spec:
        return True
    name = job_file_name(path)
    if name == spec:
        return True
    if spec.lower() == name.lower():
        return True
    posix = path.as_posix()
    if posix.endswith("/" + spec) or posix.endswith("/" + spec + "/JOB.md"):
        return True
    if spec in posix:
        return True
    if source_ws:
        try:
            rel = path.resolve().relative_to(source_ws.resolve()).as_posix()
            if rel == spec or rel.endswith("/" + spec) or spec in rel:
                return True
        except ValueError:
            pass
    return False


@dataclass
class TargetWorkspace:
    id: str
    name: str
    path: Path
    source: str


def load_trebi_workspaces(trebi_dir: Path) -> list[TargetWorkspace]:
    out: list[TargetWorkspace] = []
    conn = open_trebi_ro(trebi_dir)
    if conn:
        try:
            for rid, name, path in conn.execute("SELECT id, name, path FROM workspaces"):
                out.append(TargetWorkspace(id=rid, name=name, path=Path(path), source="db"))
        except sqlite3.Error:
            pass
        finally:
            conn.close()
    return out


def resolve_target_workspace(trebi_dir: Path, spec: str | None) -> TargetWorkspace | None:
    if not spec:
        return None
    spec = spec.strip()
    registered = load_trebi_workspaces(trebi_dir)
    for ws in registered:
        if spec in (ws.id, ws.name) or Path(spec).resolve() == ws.path.resolve():
            return ws
    p = Path(spec).expanduser()
    if p.exists() and p.is_dir():
        ty = p / "trebi.yaml"
        wid, name = mint_ws_id(str(p.resolve())), p.name
        if ty.exists():
            data = yaml.load(ty.read_text()) or {}
            if isinstance(data, dict):
                wid = str(data.get("id") or wid)
                name = str(data.get("name") or name)
        return TargetWorkspace(id=wid, name=name, path=p.resolve(), source="path")
    # dry-run may point at a path that does not exist yet
    return TargetWorkspace(id=mint_ws_id(spec), name=Path(spec).name, path=p, source="pending-path")


# ── plan ───────────────────────────────────────────────────────────────────


@dataclass
class PhasePlan:
    name: str
    action: str
    counts: dict[str, int] = field(default_factory=dict)
    notes: list[str] = field(default_factory=list)


@dataclass
class MigrationPlan:
    jobi_dir: Path
    trebi_dir: Path
    mode: str
    job: str | None = None
    target: TargetWorkspace | None = None
    phases: list[PhasePlan] = field(default_factory=list)
    warnings: list[str] = field(default_factory=list)
    workspace_map: dict[str, str] = field(default_factory=dict)

    def render(self) -> str:
        lines = [
            f"# Migration plan ({self.mode}) — DRY RUN",
            f"jobi:  {self.jobi_dir}",
            f"trebi: {self.trebi_dir}",
            f"generated: {now_iso()}",
        ]
        if self.job:
            lines.append(f"job filter: {self.job}")
        if self.target:
            lines.append(
                f"target workspace: {self.target.name} ({self.target.id}) "
                f"path={self.target.path} source={self.target.source}"
            )
        lines += ["", "## Phases"]
        for p in self.phases:
            lines.append(f"### {p.name}")
            lines.append(f"- action: {p.action}")
            for k, v in p.counts.items():
                lines.append(f"- {k}: {v}")
            for n in p.notes:
                lines.append(f"- note: {n}")
            lines.append("")
        if self.workspace_map:
            lines.append("## Workspace id map")
            for path, wid in self.workspace_map.items():
                lines.append(f"- {wid} ← {path}")
            lines.append("")
        if self.warnings:
            lines.append("## Warnings")
            for w in self.warnings:
                lines.append(f"- {w}")
        lines.append("")
        lines.append("No files written (dry-run default). Pass --apply to execute.")
        return "\n".join(lines)


def is_junk_task(d: dict[str, Any]) -> bool:
    return not d.get("started_at") and not d.get("workspace_path")


def project_task_derivations(
    conn: sqlite3.Connection, job_filter: str | None = None
) -> dict[str, int]:
    counts = {
        "reason_set": 0,
        "reason_null": 0,
        "end_reason_set": 0,
        "end_reason_null": 0,
        "conversation_key_set": 0,
        "dropped_junk": 0,
        "will_insert": 0,
        "filtered_out": 0,
    }
    try:
        cols = [r[1] for r in conn.execute("PRAGMA table_info(tasks)")]
    except sqlite3.Error:
        return counts
    for row in conn.execute("SELECT * FROM tasks"):
        d = dict(zip(cols, row))
        if job_filter and not _task_matches_job(d, job_filter):
            counts["filtered_out"] += 1
            continue
        if is_junk_task(d):
            counts["dropped_junk"] += 1
            continue
        counts["will_insert"] += 1
        r = derive_reason(conn, d["id"], d.get("trigger_source"))
        counts["reason_set" if r else "reason_null"] += 1
        er = derive_end_reason(conn, d["id"])
        counts["end_reason_set" if er else "end_reason_null"] += 1
        if derive_conversation_key(d.get("trigger_vars_json")):
            counts["conversation_key_set"] += 1
    return counts


def _task_matches_job(d: dict[str, Any], spec: str) -> bool:
    spec = spec.strip()
    name = str(d.get("job_name") or "")
    if name == spec or name.lower() == spec.lower():
        return True
    jf = str(d.get("job_file") or "")
    if spec in jf or jf.endswith("/" + spec + "/JOB.md"):
        return True
    return Path(jf).parent.name == spec if jf else False


def plan_migrate(
    jobi_dir: Path,
    trebi_dir: Path,
    mode: str,
    *,
    job: str | None = None,
    target: TargetWorkspace | None = None,
) -> MigrationPlan:
    plan = MigrationPlan(
        jobi_dir=jobi_dir, trebi_dir=trebi_dir, mode=mode, job=job, target=target
    )

    pf = PhasePlan("preflight", "check jobi present, schema, latch")
    pf.counts["jobi_db_exists"] = int((jobi_dir / "jobi.db").exists())
    pf.counts["trebi_dir_exists"] = int(trebi_dir.exists())
    pf.counts["trebi_db_exists"] = int((trebi_dir / "trebi.db").exists())
    tconn = open_trebi_ro(trebi_dir)
    if tconn:
        try:
            ver = tconn.execute("SELECT COALESCE(MAX(version),0) FROM schema_migrations").fetchone()[0]
            pf.counts["schema_version"] = int(ver)
            if int(ver) < EXPECTED_SCHEMA:
                plan.warnings.append(
                    f"trebi schema {ver} < {EXPECTED_SCHEMA}; run `trebi service migrate-db`"
                )
        except sqlite3.Error:
            plan.warnings.append("could not read trebi schema_migrations")
        tconn.close()
    if job:
        pf.notes.append(f"job filter: {job} — skip harness/bin/logs copy")
    if mode == "final":
        pf.notes.append("final mode requires jobi daemon stopped (not checked in dry-run)")
    if target:
        pf.notes.append(f"rows remap to workspace {target.id} ({target.name})")
    plan.phases.append(pf)

    ws_phase = PhasePlan("workspaces", "mint ids + registry rows")
    workspaces = load_workspaces_yaml(jobi_dir)
    for w in workspaces:
        path = str(w.get("path") or "")
        name = str(w.get("name") or Path(path).name)
        if not path:
            continue
        plan.workspace_map[path] = mint_ws_id(path)
        ws_phase.notes.append(f"{name}: {path} → {plan.workspace_map[path]}")
        for nested in nested_agent_dirs(Path(path)):
            nid = mint_ws_id(str(nested))
            plan.workspace_map[str(nested)] = nid
            ws_phase.notes.append(f"  nested {nested.name}: {nested} → {nid}")
    if target:
        plan.workspace_map[str(target.path)] = target.id
        ws_phase.notes.append(f"target override: {target.name} → {target.id}")
    ws_phase.counts["workspaces"] = len(plan.workspace_map)
    plan.phases.append(ws_phase)

    cfg = PhasePlan("config", "copy config.yaml + envsets.yaml (no key renames)")
    cfg.counts["config.yaml"] = int((jobi_dir / "config.yaml").exists())
    cfg.counts["envsets.yaml"] = int((jobi_dir / "envsets.yaml").exists())
    cfg.counts["sources.yaml"] = int((jobi_dir / "sources.yaml").exists())
    cfg.notes.append("defaults.command and session.timeout stay (current trebi config)")
    cfg.notes.append("envsets.yaml copied as envsets.yaml (not nested under config)")
    flagged = envsets_secret_key_names(jobi_dir)
    if flagged:
        cfg.notes.append(f"secret-like envset keys (names only): {len(flagged)}")
        plan.warnings.append(f"plaintext envsets keys: {', '.join(flagged)}")
    if (jobi_dir / "sources.yaml").exists():
        _, cnotes = convert_sources_yaml((jobi_dir / "sources.yaml").read_text())
        cfg.notes.extend(cnotes)
    if job:
        cfg.notes.append("--job: config/connectors not copied on apply")
    plan.phases.append(cfg)

    conn = open_jobi_ro(jobi_dir)
    if conn is None:
        plan.warnings.append("jobi.db missing — DB phases empty")
    else:
        try:
            for src, dst in TABLE_MAP.items():
                n = table_count(conn, src)
                ph = PhasePlan(f"db-{dst}", f"copy {src} → {dst}")
                ph.counts["source_rows"] = n
                if src == "tasks":
                    proj = project_task_derivations(conn, job)
                    ph.counts.update(proj)
                    ph.notes.append("workspace_path → workspace_id; command detected → harness name")
                    ph.notes.append("seconds → _ms (×1000); reason NULL if no launch fact (M13)")
                    try:
                        q = "SELECT status, COUNT(*) FROM tasks"
                        params: tuple[Any, ...] = ()
                        if job:
                            q += " WHERE job_name = ? OR job_file LIKE ?"
                            params = (job, f"%/{job}/%")
                        q += " GROUP BY status"
                        for st, c in conn.execute(q, params):
                            ph.counts[f"status_{st}"] = c
                    except sqlite3.Error:
                        pass
                if src == "event_queue":
                    ph.notes.append("source→connector; all statuses migrate")
                if src == "schedule_anchors":
                    ph.notes.append("@every: seed schedules.last_fired_at from anchor_time")
                if job:
                    ph.notes.append("filtered by --job")
                plan.phases.append(ph)
        finally:
            conn.close()

    if job:
        plan.phases.append(
            PhasePlan("files", "skipped for --job (harness/bin/logs are global)")
        )
    else:
        for name, src_rel, dst_rel in (
            ("harness-homes", "harness", "harness"),
            ("bin-tree", "bin", "bin"),
            ("logs", "logs", "logs"),
        ):
            src = jobi_dir / src_rel
            ph = PhasePlan(name, f"copy {src} → {trebi_dir / dst_rel}")
            if name == "logs":
                ph.notes.append("logs/tasks → logs/runs; logs/sources → logs/connectors")
                ph.notes.append("logs/jobi.log* → logs/imported-jobi/")
            if src.exists():
                files = sum(1 for _ in src.rglob("*") if _.is_file())
                ph.counts["files"] = files
                ph.counts["bytes"] = sum(f.stat().st_size for f in src.rglob("*") if f.is_file())
            else:
                ph.counts["files"] = 0
                ph.notes.append("source missing")
            plan.phases.append(ph)

    plan.phases.append(PhasePlan("verify", "run invariants (after apply)"))
    return plan


# ── DB apply ───────────────────────────────────────────────────────────────


def workspace_id_for(path: str, wmap: dict[str, str], target_id: str | None = None) -> str:
    if target_id:
        return target_id
    if not path:
        return "imported-orphans"
    best = ""
    best_id = "imported-orphans"
    pl = path.lower()
    for wp, wid in wmap.items():
        wpl = wp.lower().rstrip("/")
        if pl.startswith(wpl) and len(wpl) > len(best):
            best = wpl
            best_id = wid
    return best_id


def derive_reason(jconn: sqlite3.Connection, task_id: str, trigger_source: str | None) -> str | None:
    try:
        row = jconn.execute(
            "SELECT detail_json FROM task_events WHERE task_id=? AND kind='launch' ORDER BY at LIMIT 1",
            (task_id,),
        ).fetchone()
        if row and row[0]:
            try:
                detail = json.loads(row[0])
                if isinstance(detail, dict) and detail.get("reason"):
                    return str(detail["reason"])
            except json.JSONDecodeError:
                pass
        row = jconn.execute(
            "SELECT reason FROM task_segments WHERE task_id=? AND layer='task' "
            "AND reason IS NOT NULL AND reason!='' ORDER BY started_at LIMIT 1",
            (task_id,),
        ).fetchone()
        if row and row[0] in ("manual", "schedule", "trigger", "when", "resume", "replay"):
            return row[0]
    except sqlite3.Error:
        pass
    if trigger_source:
        return "trigger"
    return None


def derive_end_reason(jconn: sqlite3.Connection, task_id: str) -> str | None:
    try:
        row = jconn.execute(
            """SELECT reason FROM task_segments
               WHERE task_id=? AND layer='task' AND ended_at IS NOT NULL
                 AND state IN ('complete','failed','exited','skipped','timed_out','resumed')
               ORDER BY started_at DESC LIMIT 1""",
            (task_id,),
        ).fetchone()
        if row and row[0]:
            return row[0]
        row = jconn.execute(
            """SELECT reason FROM task_segments
               WHERE task_id=? AND layer='task'
                 AND state IN ('complete','failed','exited','skipped','timed_out','resumed')
               ORDER BY started_at DESC LIMIT 1""",
            (task_id,),
        ).fetchone()
        if row and row[0]:
            return row[0]
    except sqlite3.Error:
        pass
    return None


def derive_conversation_key(trigger_vars_json: str | None) -> str | None:
    if not trigger_vars_json:
        return None
    try:
        v = json.loads(trigger_vars_json)
        if isinstance(v, dict) and v.get("conversation_key"):
            return str(v["conversation_key"])
    except json.JSONDecodeError:
        pass
    return None


def apply_tasks(
    jconn: sqlite3.Connection,
    tconn: sqlite3.Connection,
    wmap: dict[str, str],
    *,
    job_filter: str | None = None,
    target_id: str | None = None,
) -> tuple[int, set[str]]:
    cols = [r[1] for r in jconn.execute("PRAGMA table_info(tasks)")]
    ids: set[str] = set()
    n = 0
    for row in jconn.execute("SELECT * FROM tasks"):
        d = dict(zip(cols, row))
        if job_filter and not _task_matches_job(d, job_filter):
            continue
        if is_junk_task(d):
            continue
        path_for_ws = d.get("job_file") or d.get("workspace_path") or ""
        wid = workspace_id_for(path_for_ws, wmap, target_id)
        kind = d.get("kind") or "job"
        if kind == "agent":
            kind = "assistant"
        harness, _notes = detect_harness(d.get("command"), None)
        if not harness:
            harness = d.get("command") or "claude"
            if harness and " " in str(harness):
                harness = "claude"

        def ms(sec_key: str, default: int = 0) -> int:
            v = d.get(sec_key)
            if v is None:
                return default
            return int(v) * 1000 if int(v) > 0 else int(v)

        nudge = d.get("nudge_seconds")
        if nudge is None:
            nudge_ms = -1
        else:
            nudge_ms = int(nudge) if int(nudge) < 0 else int(nudge) * 1000
        idle = d.get("timeout_seconds") or 0
        idle_ms = int(idle) * 1000 if idle else 0
        trace = hashlib.sha256((d["id"] + "trace").encode()).hexdigest()[:32]
        reason = derive_reason(jconn, d["id"], d.get("trigger_source"))
        end_reason = derive_end_reason(jconn, d["id"])
        conv_key = derive_conversation_key(d.get("trigger_vars_json"))
        queue_id = None
        try:
            qrow = jconn.execute(
                "SELECT id FROM event_queue WHERE task_id=? AND status='done' LIMIT 1",
                (d["id"],),
            ).fetchone()
            if qrow:
                queue_id = qrow[0]
        except sqlite3.Error:
            pass
        tconn.execute(
            """INSERT OR REPLACE INTO runs (
              id, workspace_id, job, kind, reason, status, end_reason, prompt_preview, session_id, runtime, harness,
              tmux_session, tokens_input, tokens_output, tokens_cache_read, tokens_cache_creation, tokens_model,
              started_at, ended_at, max_duration_ms, idle_timeout_ms, keep_alive_ms, nudge_ms,
              trigger_connector, trigger_capture_json, trigger_vars_json, conversation_key, queue_id,
              trace_id, revision, updated_at, working_dir
            ) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,1,?,?)""",
            (
                d["id"],
                wid,
                d.get("job_name") or "",
                kind,
                reason,
                d.get("status") or "exited",
                end_reason,
                d.get("prompt_preview"),
                d.get("session_id"),
                "tui",
                harness,
                d.get("tmux_session"),
                d.get("tokens_input"),
                d.get("tokens_output"),
                d.get("tokens_cache_read"),
                d.get("tokens_cache_creation"),
                d.get("tokens_model"),
                d.get("started_at"),
                d.get("ended_at"),
                ms("max_duration_seconds"),
                idle_ms,
                ms("keep_alive_seconds"),
                nudge_ms,
                d.get("trigger_source"),
                d.get("trigger_capture_json"),
                d.get("trigger_vars_json"),
                conv_key,
                queue_id,
                trace,
                d.get("ended_at") or d.get("started_at"),
                d.get("working_dir"),
            ),
        )
        ids.add(d["id"])
        n += 1
    return n, ids


def apply_timeline(
    jconn: sqlite3.Connection, tconn: sqlite3.Connection, task_ids: set[str] | None
) -> None:
    try:
        for row in jconn.execute(
            "SELECT task_id, layer, state, started_at, ended_at, duration_ms, reason, source, detail_json FROM task_segments"
        ):
            if task_ids is not None and row[0] not in task_ids:
                continue
            tconn.execute(
                "INSERT INTO run_segments (run_id, layer, state, started_at, ended_at, duration_ms, reason, source, detail_json) VALUES (?,?,?,?,?,?,?,?,?)",
                row,
            )
        for row in jconn.execute(
            "SELECT task_id, at, kind, source, detail_json FROM task_events"
        ):
            if task_ids is not None and row[0] not in task_ids:
                continue
            tconn.execute(
                "INSERT INTO run_events (run_id, at, kind, source, detail_json) VALUES (?,?,?,?,?)",
                row,
            )
    except sqlite3.Error as e:
        print(f"timeline skip: {e}")


def apply_queue(
    jconn: sqlite3.Connection,
    tconn: sqlite3.Connection,
    wmap: dict[str, str],
    *,
    job_filter: str | None = None,
    target_id: str | None = None,
) -> int:
    try:
        cols = [r[1] for r in jconn.execute("PRAGMA table_info(event_queue)")]
    except sqlite3.Error:
        return 0
    n = 0
    for row in jconn.execute("SELECT * FROM event_queue"):
        d = dict(zip(cols, row))
        if job_filter:
            jn = str(d.get("job_name") or "")
            if jn != job_filter and job_filter.lower() != jn.lower():
                continue
        wid = workspace_id_for(d.get("workspace_path") or "", wmap, target_id)
        kind = "assistant" if d.get("job_name") and d.get("job_name") == d.get("parent_agent") else "job"
        tconn.execute(
            """INSERT OR REPLACE INTO event_queue (
              id, connector, workspace_id, job, kind, on_index, conversation_key,
              event_raw, capture_json, vars_json, prompt, status, block_reason, result,
              attempts, next_attempt_at, claimed_at, run_id, error_message, created_at, updated_at, done_at
            ) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)""",
            (
                d["id"],
                d.get("source"),
                wid,
                d.get("job_name"),
                kind,
                d.get("on_index") or 0,
                d.get("conversation_key") or "",
                d.get("event_raw"),
                d.get("capture_json"),
                d.get("vars_json"),
                d.get("prompt") or "",
                d.get("status"),
                d.get("block_reason"),
                d.get("result"),
                d.get("attempts") or 0,
                d.get("next_attempt_at"),
                d.get("claimed_at"),
                d.get("task_id"),
                d.get("error_message"),
                d.get("created_at"),
                d.get("updated_at"),
                d.get("done_at"),
            ),
        )
        n += 1
    return n


def apply_policy_cursors(jconn: sqlite3.Connection, tconn: sqlite3.Connection) -> None:
    try:
        for key, last, win in jconn.execute(
            "SELECT key, last_fired_at, window_json FROM trigger_policy"
        ):
            tconn.execute(
                "INSERT OR REPLACE INTO dispatch_policy (key, last_fired_at, window_json) VALUES (?,?,?)",
                (key, last, win),
            )
    except sqlite3.Error:
        pass
    try:
        for name, eid, upd in jconn.execute(
            "SELECT source_name, last_event_id, updated_at FROM source_cursors"
        ):
            tconn.execute(
                "INSERT OR REPLACE INTO connector_cursors (name, last_event_id, updated_at) VALUES (?,?,?)",
                (name, eid, upd),
            )
    except sqlite3.Error:
        pass
    try:
        for row in jconn.execute(
            "SELECT name, events_total, parse_errors, match_errors, fires, drops, last_event_at, updated_at FROM source_metrics"
        ):
            tconn.execute(
                "INSERT OR REPLACE INTO connector_metrics "
                "(name, events_total, parse_errors, match_errors, fires, drops, last_event_at, updated_at) "
                "VALUES (?,?,?,?,?,?,?,?)",
                row,
            )
    except sqlite3.Error:
        pass


def apply_schedules(
    jconn: sqlite3.Connection,
    tconn: sqlite3.Connection,
    wmap: dict[str, str],
    *,
    job_filter: str | None = None,
    target_id: str | None = None,
) -> int:
    try:
        rows = list(jconn.execute("SELECT key, anchor_time, interval FROM schedule_anchors"))
    except sqlite3.Error:
        return 0
    job_ws: dict[str, str] = {}
    try:
        for jn, wp, jf in jconn.execute(
            "SELECT job_name, workspace_path, job_file FROM tasks WHERE job_name IS NOT NULL"
        ):
            if jn and jn not in job_ws:
                job_ws[jn] = workspace_id_for(jf or wp or "", wmap, target_id)
    except sqlite3.Error:
        pass
    n = 0
    now = now_iso()
    for key, anchor_time, interval in rows:
        job, idx = key, 0
        if "#" in key:
            job, _, rest = key.partition("#")
            try:
                idx = int(rest)
            except ValueError:
                idx = 0
        if job_filter and job != job_filter and job.lower() != job_filter.lower():
            continue
        wid = target_id or job_ws.get(job) or "imported-orphans"
        tconn.execute(
            """INSERT OR REPLACE INTO schedules (
              workspace_id, job, idx, spec, timezone, catch_up, next_due_at, last_fired_at, anchor, updated_at
            ) VALUES (?,?,?,?,?,?,?,?,?,?)""",
            (wid, job, idx, interval or "", "UTC", 0, None, anchor_time, anchor_time, now),
        )
        n += 1
    return n


def copy_logs(jobi: Path, trebi: Path) -> None:
    src = jobi / "logs"
    if not src.exists():
        return
    tasks = src / "tasks"
    if tasks.exists():
        dst = trebi / "logs" / "runs"
        dst.mkdir(parents=True, exist_ok=True)
        for f in tasks.iterdir():
            if f.is_file():
                shutil.copy2(f, dst / f.name)
    sources = src / "sources"
    if sources.exists():
        dst = trebi / "logs" / "connectors"
        dst.mkdir(parents=True, exist_ok=True)
        for f in sources.rglob("*"):
            if f.is_file():
                shutil.copy2(f, dst / f.name)
    imported = trebi / "logs" / "imported-jobi"
    imported.mkdir(parents=True, exist_ok=True)
    for f in src.glob("jobi.log*"):
        shutil.copy2(f, imported / f.name)
    tray = src / "tray.log"
    if tray.exists():
        shutil.copy2(tray, imported / "tray.log")


def apply_migrate(jobi: Path, trebi: Path, mode: str, plan: MigrationPlan) -> int:
    trebi.mkdir(parents=True, exist_ok=True)
    if refuse_if_daemon_running(trebi, "migrate --apply"):
        return 2
    job_filter = plan.job
    target_id = plan.target.id if plan.target else None

    if not job_filter:
        src_cfg = jobi / "config.yaml"
        if src_cfg.exists():
            # Current trebi still uses defaults.command and session.timeout — copy as-is.
            (trebi / "config.yaml").write_text(src_cfg.read_text(encoding="utf-8", errors="replace"))
            print("wrote config.yaml (keys unchanged)")
        src_env = jobi / "envsets.yaml"
        if src_env.exists():
            (trebi / "envsets.yaml").write_text(src_env.read_text(encoding="utf-8", errors="replace"))
            print("wrote envsets.yaml (values not printed)")
        src_src = jobi / "sources.yaml"
        if src_src.exists():
            converted, notes = convert_sources_yaml(src_src.read_text(encoding="utf-8", errors="replace"))
            from io import StringIO

            buf = StringIO()
            yaml.dump(converted, buf)
            (trebi / "connectors.yaml").write_text(buf.getvalue())
            for n in notes:
                print(f"  {n}")
            print("wrote connectors.yaml")
        for name in ("harness", "bin"):
            src = jobi / name
            dst = trebi / name
            if src.exists():
                if dst.exists():
                    shutil.rmtree(dst)
                shutil.copytree(src, dst, symlinks=True)
                print(f"copied {name}/")
        copy_logs(jobi, trebi)
        print("copied logs (tasks→runs, sources→connectors)")

    tdb = trebi / "trebi.db"
    jdb = jobi / "jobi.db"
    if not jdb.exists():
        print("no jobi.db — skip db phases")
        return 0
    if not tdb.exists():
        print("WARNING: trebi.db missing — run `trebi service migrate-db` first; DB rows not inserted")
        return 0
    jconn = open_jobi_ro(jobi)
    if jconn is None:
        print("no jobi.db — skip db phases")
        return 0
    tconn = sqlite3.connect(tdb)
    try:
        n, ids = apply_tasks(
            jconn, tconn, plan.workspace_map, job_filter=job_filter, target_id=target_id
        )
        print(f"inserted runs: {n}")
        apply_timeline(jconn, tconn, ids if job_filter else None)
        print("timeline segments+events copied")
        n = apply_queue(
            jconn, tconn, plan.workspace_map, job_filter=job_filter, target_id=target_id
        )
        print(f"queue rows: {n}")
        if not job_filter:
            apply_policy_cursors(jconn, tconn)
            print("policy + cursors + metrics copied")
        n = apply_schedules(
            jconn, tconn, plan.workspace_map, job_filter=job_filter, target_id=target_id
        )
        print(f"schedule seeds: {n}")
        now = now_iso()
        if plan.target:
            tconn.execute(
                "INSERT OR REPLACE INTO workspaces (id, name, path, git_enabled, orphaned, created_at, updated_at) "
                "VALUES (?,?,?,0,0,?,?)",
                (plan.target.id, plan.target.name, str(plan.target.path), now, now),
            )
        else:
            for path, wid in plan.workspace_map.items():
                name = Path(path).name
                dest = path if mode == "final" else str(trebi / "shadow-workspaces" / name)
                tconn.execute(
                    "INSERT OR REPLACE INTO workspaces (id, name, path, git_enabled, orphaned, created_at, updated_at) "
                    "VALUES (?,?,?,0,0,?,?)",
                    (wid, name, dest, now, now),
                )
        tconn.commit()
        manifest = {
            "mode": mode,
            "workspace_map": plan.workspace_map,
            "job": job_filter,
            "target": None
            if not plan.target
            else {"id": plan.target.id, "name": plan.target.name, "path": str(plan.target.path)},
            "cutover_started_at": now if mode == "final" and not job_filter else None,
        }
        (trebi / "migration-manifest.json").write_text(json.dumps(manifest, indent=2))
        print("wrote migration-manifest.json")
    finally:
        jconn.close()
        tconn.close()
    return 0


# ── rewrite-jobs ───────────────────────────────────────────────────────────


def find_job_files(workspace: Path) -> list[Path]:
    out: list[Path] = []
    for root, dirs, files in os.walk(workspace):
        dirs[:] = [d for d in dirs if d not in {".git", "node_modules", ".trebi"}]
        for f in files:
            if f in ("JOB.md", "AGENT.md", "SOUL.md", "TREBI.md"):
                out.append(Path(root) / f)
    return out


def dest_for_job(
    path: Path,
    source_ws: Path | None,
    target: TargetWorkspace | None,
    incremental: bool,
) -> Path:
    if incremental and target:
        if source_ws:
            try:
                rel = path.resolve().relative_to(source_ws.resolve())
                # If the target is a nested agent dir of the source, strip that prefix.
                try:
                    nested = target.path.resolve().relative_to(source_ws.resolve())
                    rel_s = rel.as_posix()
                    prefix = nested.as_posix().rstrip("/") + "/"
                    if rel_s.startswith(prefix):
                        rel = Path(rel_s[len(prefix) :])
                except ValueError:
                    pass
                return target.path / rel
            except ValueError:
                pass
        return target.path / job_file_name(path) / path.name
    return path


def plan_rewrite(
    workspaces: list[Path],
    *,
    job: str | None,
    target: TargetWorkspace | None,
    incremental: bool,
    rewrite_sentinel: bool,
) -> tuple[list[str], list[dict[str, Any]]]:
    lines: list[str] = []
    details: list[dict[str, Any]] = []
    for ws in workspaces:
        if not ws.exists():
            lines.append(f"SKIP missing workspace {ws}")
            continue
        files = find_job_files(ws)
        matched = 0
        header_written = False
        for path in files:
            if job and not job_spec_matches(path, job, ws):
                continue
            if path.name == "SOUL.md" and job and job_file_name(path) != job:
                continue
            matched += 1
            if not header_written:
                lines.append(f"workspace {ws}:")
                header_written = True
            report: list[str] = []
            text = path.read_text(encoding="utf-8", errors="replace")
            dest = dest_for_job(path, ws, target, incremental)
            if path.name == "SOUL.md":
                details.append(
                    {
                        "path": str(path),
                        "dest": str(dest.with_name("TREBI.md")),
                        "action": "merge-into-TREBI.md",
                        "changed": True,
                        "source_ws": str(ws),
                    }
                )
                lines.append(f"  MERGE {path.relative_to(ws)} → {dest.with_name('TREBI.md')}")
                continue
            if path.name == "AGENT.md":
                details.append(
                    {
                        "path": str(path),
                        "dest": str(dest.with_name("TREBI.md")),
                        "action": "merge-into-TREBI.md",
                        "changed": True,
                        "source_ws": str(ws),
                    }
                )
                lines.append(f"  MERGE {path.relative_to(ws)} → {dest.with_name('TREBI.md')}")
                new_text, _changed = rewrite_job_text(text, report, rewrite_sentinel=rewrite_sentinel)
                for r in report:
                    lines.append(f"    ! {r}")
                continue
            new_text, changed = rewrite_job_text(text, report, rewrite_sentinel=rewrite_sentinel)
            action = "rewrite" if changed else "unchanged"
            if incremental and target:
                action = f"copy→{dest}"
            details.append(
                {
                    "path": str(path),
                    "dest": str(dest),
                    "action": action,
                    "changed": changed,
                    "source_ws": str(ws),
                    "new_text": new_text if changed else None,
                }
            )
            mark = "*" if changed else " "
            try:
                rel = path.relative_to(ws)
            except ValueError:
                rel = path
            lines.append(f"  {mark} {rel} ({action})")
            for r in report:
                lines.append(f"    ! {r}")
        if job and matched == 0:
            lines.append(f"workspace {ws}: no files matched --job {job}")
        elif not job:
            pass
    return lines, details


def backup_file(backup_root: Path, original: Path) -> None:
    key = hashlib.sha256(str(original).encode()).hexdigest()[:12]
    bdir = backup_root / key
    bdir.mkdir(parents=True, exist_ok=True)
    shutil.copy2(original, bdir / original.name)
    (bdir / "meta.json").write_text(
        json.dumps({"original": str(original), "name": original.name}, indent=2)
    )


def cmd_rewrite_jobs(args: argparse.Namespace) -> int:
    jobi = Path(args.jobi_dir)
    trebi = Path(args.trebi_dir)
    workspaces = workspace_paths_from_jobi(jobi)
    if args.workspace:
        workspaces = [Path(args.workspace)]
    job = getattr(args, "job", None)
    target_spec = getattr(args, "target_workspace", None)
    if not target_spec and args.mirror:
        target_spec = str(trebi / "shadow-workspaces")
    target = resolve_target_workspace(trebi, target_spec)
    incremental = bool(job or args.mirror)
    if incremental and args.apply and not target:
        print("ERROR: --job --apply requires --target-workspace", file=sys.stderr)
        return 2
    if getattr(args, "target_workspace", None) and target is None:
        print(f"ERROR: could not resolve --target-workspace {args.target_workspace!r}", file=sys.stderr)
        return 2
    if incremental and target:
        print(f"TARGET {target.name} ({target.id}) {target.path} [{target.source}]")
    lines, details = plan_rewrite(
        workspaces,
        job=job,
        target=target,
        incremental=incremental,
        rewrite_sentinel=not args.no_sentinels,
    )
    for line in lines:
        print(line)
    changed = sum(1 for d in details if d.get("changed"))
    print(f"\n{len(details)} files considered, {changed} would change")
    if not details and job:
        print(f"ERROR: no job matched --job {job}", file=sys.stderr)
        return 2
    if args.diff and not args.apply:
        shown = 0
        for d in details:
            if not d.get("changed"):
                continue
            path = Path(d["path"])
            if path.name not in ("JOB.md", "TREBI.md"):
                continue
            text = path.read_text(encoding="utf-8", errors="replace")
            report: list[str] = []
            new_text, _ = rewrite_job_text(text, report, rewrite_sentinel=not args.no_sentinels)
            print(f"\n--- {path}")
            if d.get("dest") and d["dest"] != d["path"]:
                print(f"+++ {d['dest']}")
            print("OLD frontmatter:")
            fm, _, _ = split_frontmatter(text)
            print((fm or text).rstrip()[:2000])
            print("NEW frontmatter:")
            fm2, _, _ = split_frontmatter(new_text)
            print((fm2 or new_text).rstrip()[:2000])
            shown += 1
            if shown >= 5:
                break
    if args.revert:
        if not args.apply:
            print("\nRevert is a no-op without --apply.")
            return 0
        backup = trebi / "rewrite-backup"
        if not backup.exists():
            print("no rewrite-backup to revert", file=sys.stderr)
            return 2
        restored = 0
        for meta in backup.glob("*/meta.json"):
            info = json.loads(meta.read_text())
            original = Path(info["original"])
            src = meta.parent / info["name"]
            if not src.exists():
                continue
            original.parent.mkdir(parents=True, exist_ok=True)
            shutil.copy2(src, original)
            print(f"restored {original}")
            restored += 1
        print(f"restored {restored} files")
        return 0
    if not args.apply:
        print("\nNo files written (dry-run).")
        if incremental and not target:
            print("Hint: pass --target-workspace <id|name|path> to set the destination.")
        return 0

    # The daemon watches these files; a live watcher would index half-written jobs.
    if refuse_if_daemon_running(trebi, "rewrite-jobs --apply"):
        return 2

    backup = trebi / "rewrite-backup"
    backup.mkdir(parents=True, exist_ok=True)
    for d in details:
        path = Path(d["path"])
        dest = Path(d.get("dest") or path)
        if path.name == "SOUL.md":
            continue
        if path.name == "AGENT.md":
            ws_root = path.parent
            soul = path.parent / "SOUL.md"
            agent_text = path.read_text(encoding="utf-8", errors="replace")
            report: list[str] = []
            new_agent, _ = rewrite_job_text(agent_text, report, rewrite_sentinel=not args.no_sentinels)
            soul_body = ""
            if soul.exists():
                soul_body = soul.read_text(encoding="utf-8", errors="replace")
                _, _, soul_body = split_frontmatter(soul_body)
            fm, _, body = split_frontmatter(new_agent)
            data = load_fm_map(fm or "")
            data = rewrite_frontmatter_map(data, report)
            data.setdefault("memory", True)
            merged_body = f"## Soul\n{soul_body.strip()}\n\n## Instructions\n{body.strip()}\n"
            out = dump_frontmatter(data, merged_body)
            dest = dest.with_name("TREBI.md") if dest.suffix else dest / "TREBI.md"
            if not incremental:
                backup_file(backup, path)
                if soul.exists():
                    backup_file(backup, soul)
            dest.parent.mkdir(parents=True, exist_ok=True)
            dest.write_text(out)
            print(f"wrote {dest}")
            if not incremental:
                path.unlink()
                if soul.exists():
                    soul.unlink()
                print(f"removed {path}")
            continue
        if path.name not in ("JOB.md", "TREBI.md"):
            continue
        text = path.read_text(encoding="utf-8", errors="replace")
        report: list[str] = []
        new_text, changed = rewrite_job_text(text, report, rewrite_sentinel=not args.no_sentinels)
        if not changed and dest == path:
            continue
        if not incremental:
            backup_file(backup, path)
        dest.parent.mkdir(parents=True, exist_ok=True)
        dest.write_text(new_text if changed else text)
        print(f"{'copied' if incremental else 'rewrote'} {dest}")

    if not incremental:
        for ws in workspaces:
            if not ws.exists():
                continue
            ty = ws / "trebi.yaml"
            if not ty.exists():
                ty.write_text(f"id: {mint_ws_id(str(ws))}\nname: {ws.name}\ngit_enabled: false\n")
                print(f"minted {ty}")
    return 0


# ── other commands ─────────────────────────────────────────────────────────


def cmd_status(args: argparse.Namespace) -> int:
    jobi = Path(args.jobi_dir)
    trebi = Path(args.trebi_dir)
    print(f"jobi_dir:  {jobi} exists={jobi.exists()}")
    print(f"trebi_dir: {trebi} exists={trebi.exists()}")
    ws = load_workspaces_yaml(jobi)
    print(f"jobi workspaces.yaml entries: {len(ws)}")
    for w in ws:
        print(f"  - {w.get('name', '?')}: {w.get('path')}")
        for nested in nested_agent_dirs(Path(w.get("path") or "")):
            print(f"      nested agent: {nested.name} ({nested})")
    registered = load_trebi_workspaces(trebi)
    print(f"trebi workspaces: {len(registered)}")
    for w in registered:
        print(f"  - {w.name} ({w.id}): {w.path}")
    conn = open_jobi_ro(jobi)
    if conn:
        print("jobi.db tables:")
        for src, dst in TABLE_MAP.items():
            n = table_count(conn, src)
            print(f"  {src:20} {n:>8}  → {dst}")
        conn.close()
    else:
        print("jobi.db: missing")
    for name in ("config.yaml", "sources.yaml", "envsets.yaml", "harness", "bin", "logs"):
        p = jobi / name
        print(f"  artifact {name}: exists={p.exists()}")
    flagged = envsets_secret_key_names(jobi)
    if flagged:
        print(f"  envset secret-like keys: {len(flagged)} (values not shown)")
    job = getattr(args, "job", None)
    if job:
        print(f"\n--job {job} matches:")
        n = 0
        for wpath in workspace_paths_from_jobi(jobi):
            for path in find_job_files(wpath):
                if job_spec_matches(path, job, wpath) and path.name == "JOB.md":
                    print(f"  {path}")
                    n += 1
        if n == 0:
            print("  (none)")
    return 0


def cmd_migrate(args: argparse.Namespace) -> int:
    mode = "shadow" if args.shadow else "final" if args.final else ""
    if not mode:
        print("ERROR: migrate requires exactly one of --shadow / --final", file=sys.stderr)
        return 2
    jobi = Path(args.jobi_dir)
    trebi = Path(args.trebi_dir)
    job = getattr(args, "job", None)
    target = resolve_target_workspace(trebi, getattr(args, "target_workspace", None))
    if job and args.apply and not target:
        print("ERROR: --job --apply requires --target-workspace", file=sys.stderr)
        return 2
    plan = plan_migrate(jobi, trebi, mode, job=job, target=target)
    text = plan.render()
    print(text)
    if args.report:
        Path(args.report).write_text(text)
        print(f"\n(report written to {args.report})")
    if not args.apply:
        print("\nDry-run only. Re-run with --apply to execute.")
        return 0
    return apply_migrate(jobi, trebi, mode, plan)


def cmd_verify(args: argparse.Namespace) -> int:
    trebi = Path(args.trebi_dir)
    jobi = Path(args.jobi_dir)
    job = getattr(args, "job", None)
    db = trebi / "trebi.db"
    print(f"verify against {db}")
    if not db.exists():
        print("trebi.db missing — nothing to verify (ok for pre-apply)")
        return 0
    tconn = sqlite3.connect(f"file:{db}?mode=ro", uri=True)
    jconn = open_jobi_ro(jobi)
    try:
        if job:
            runs = tconn.execute("SELECT COUNT(*) FROM runs WHERE job=?", (job,)).fetchone()[0]
        else:
            runs = tconn.execute("SELECT COUNT(*) FROM runs").fetchone()[0]
        if jconn:
            proj = project_task_derivations(jconn, job)
            want = proj["will_insert"]
            ok = runs == want
            print(f"run conservation: trebi.runs={runs} jobi.will_insert={want} junk={proj['dropped_junk']} ok={ok}")
        else:
            print(f"trebi.runs={runs} (no jobi.db to compare)")
        for tbl in ("run_segments", "run_events", "event_queue", "schedules"):
            try:
                n = tconn.execute(f"SELECT COUNT(*) FROM {tbl}").fetchone()[0]
                print(f"  {tbl}: {n}")
            except sqlite3.Error as e:
                print(f"  {tbl}: error {e}")
    finally:
        tconn.close()
        if jconn:
            jconn.close()
    return 0


def cmd_make_fixture(args: argparse.Namespace) -> int:
    out = Path(args.dir).resolve()
    out.mkdir(parents=True, exist_ok=True)
    (out / "workspaces.yaml").write_text("workspaces:\n  - name: fixture\n    path: ws\n")
    ws = out / "ws" / "demo"
    ws.mkdir(parents=True, exist_ok=True)
    (ws / "JOB.md").write_text(
        "---\n"
        "# important comment\n"
        "working_dir: .\n"
        "command: npx @anthropic-ai/claude-code@2.1.142 --dangerously-skip-permissions\n"
        "tool: claude\n"
        "schedule: \"0 9 * * *\"\n"
        "env: claude-zai\n"
        "session:\n"
        "  timeout: 30m\n"
        "on:\n"
        "  - source: whatsapp\n"
        "    match: \"true\"\n"
        "---\n"
        "Do the thing. JOBI_TASK_COMPLETE\n"
    )
    db = out / "jobi.db"
    conn = sqlite3.connect(db)
    conn.executescript(
        """
        CREATE TABLE tasks (
          id TEXT PRIMARY KEY, job_name TEXT, job_file TEXT, workspace_path TEXT,
          working_dir TEXT, command TEXT, tmux_session TEXT, session_id TEXT,
          status TEXT, kind TEXT, parent_agent TEXT, started_at TEXT, ended_at TEXT,
          prompt_preview TEXT, timeout_seconds INTEGER DEFAULT 0,
          trigger_source TEXT, trigger_name TEXT, trigger_capture_json TEXT, trigger_vars_json TEXT,
          tokens_input INTEGER, tokens_output INTEGER, tokens_cache_read INTEGER,
          tokens_cache_creation INTEGER, tokens_model TEXT,
          max_duration_seconds INTEGER DEFAULT 0, keep_alive_seconds INTEGER DEFAULT 0,
          nudge_seconds INTEGER DEFAULT -1
        );
        CREATE TABLE task_segments (
          id INTEGER PRIMARY KEY, task_id TEXT, layer TEXT, state TEXT,
          started_at TEXT, ended_at TEXT, duration_ms INTEGER, reason TEXT, source TEXT, detail_json TEXT
        );
        CREATE TABLE task_events (
          id INTEGER PRIMARY KEY, task_id TEXT, at TEXT, kind TEXT, source TEXT, detail_json TEXT
        );
        CREATE TABLE event_queue (
          id TEXT PRIMARY KEY, source TEXT, workspace_path TEXT, job_name TEXT,
          parent_agent TEXT DEFAULT '', on_index INTEGER DEFAULT 0, conversation_key TEXT DEFAULT '',
          event_raw TEXT, capture_json TEXT, vars_json TEXT, prompt TEXT DEFAULT '',
          status TEXT, block_reason TEXT, result TEXT, attempts INTEGER DEFAULT 0,
          next_attempt_at TEXT, claimed_at TEXT, task_id TEXT, error_message TEXT,
          created_at TEXT, updated_at TEXT, done_at TEXT
        );
        CREATE TABLE trigger_policy (key TEXT PRIMARY KEY, last_fired_at TEXT, window_json TEXT);
        CREATE TABLE source_metrics (name TEXT PRIMARY KEY, events_total INTEGER DEFAULT 0,
          parse_errors INTEGER DEFAULT 0, match_errors INTEGER DEFAULT 0, fires INTEGER DEFAULT 0,
          drops INTEGER DEFAULT 0, last_event_at TEXT, updated_at TEXT);
        CREATE TABLE source_cursors (source_name TEXT PRIMARY KEY, last_event_id TEXT DEFAULT '', updated_at TEXT);
        CREATE TABLE schedule_anchors (key TEXT PRIMARY KEY, anchor_time TEXT, interval TEXT);
        """
    )
    conn.execute(
        "INSERT INTO tasks (id, job_name, workspace_path, command, status, kind, started_at, prompt_preview) "
        "VALUES (?,?,?,?,?,?,?,?)",
        (
            "01FIXTURE0000000000000000",
            "demo",
            str(out / "ws"),
            "npx @anthropic-ai/claude-code@2.1.142 --dangerously-skip-permissions",
            "complete",
            "job",
            "2026-01-01T00:00:00Z",
            "hi",
        ),
    )
    conn.execute(
        "INSERT INTO schedule_anchors (key, anchor_time, interval) VALUES (?,?,?)",
        ("demo", "2026-01-01T00:00:00Z", "6h"),
    )
    conn.commit()
    conn.close()
    (out / "sources.yaml").write_text(
        "sources:\n"
        "  - name: whatsapp\n"
        "    command:\n"
        "      - whatsapp-cli\n"
        "      - listen\n"
        "      - --json\n"
        "    format: json\n"
        "    cursor:\n"
        "      id: d.id\n"
    )
    (out / "config.yaml").write_text("defaults:\n  command: claude\n  session:\n    timeout: 30m\n")
    (out / "envsets.yaml").write_text("claude-zai:\n  API_TIMEOUT_MS: '1'\n")
    print(f"fixture written to {out}")
    return 0


def cmd_retire_jobi(args: argparse.Namespace) -> int:
    print("Manual retirement steps (executes nothing):")
    print("1. launchctl bootout gui/$UID/dev.jobi.daemon")
    print("2. replace /usr/local/bin/jobi with a shim pointing to trebi")
    print("3. remove ~/.claude/skills/jobi and ~/.agents/skills/jobi links")
    print("4. keep ~/.jobi until comfortable; then delete manually")
    return 0


def add_common_args(parser: argparse.ArgumentParser) -> None:
    parser.add_argument("--jobi-dir", default=str(default_jobi_dir()))
    parser.add_argument("--trebi-dir", default=str(default_trebi_dir()))
    parser.add_argument("--job", help="limit to one job (name, relative path, or JOB.md path)")
    parser.add_argument(
        "--target-workspace",
        "-t",
        help="trebi destination workspace (id, name, or path). Required with --job --apply.",
    )


def main(argv: list[str] | None = None) -> int:
    # Common flags are declared twice: once on the top-level parser and once on a
    # parent every subparser inherits. That accepts them on either side of the
    # subcommand. Subparser defaults are suppressed so an unset flag after the
    # subcommand does not overwrite a value given before it.
    common = argparse.ArgumentParser(add_help=False)
    add_common_args(common)
    for action in common._actions:  # noqa: SLF001 - argparse exposes no public API
        action.default = argparse.SUPPRESS

    p = argparse.ArgumentParser(description="jobi → trebi migration (dry-run default)")
    add_common_args(p)
    sub = p.add_subparsers(dest="cmd", required=True)

    s = sub.add_parser("status", help="inventory jobi + trebi trees", parents=[common])
    s.set_defaults(func=cmd_status)

    m = sub.add_parser("migrate", help="plan DB/config/file migration", parents=[common])
    m.add_argument("--shadow", action="store_true")
    m.add_argument("--final", action="store_true")
    m.add_argument("--apply", action="store_true", help="execute writes (refused while trebid is running)")
    m.add_argument("--report", help="write plan markdown to path")
    m.set_defaults(func=cmd_migrate)

    r = sub.add_parser("rewrite-jobs", help="plan JOB.md/AGENT.md restructure", parents=[common])
    r.add_argument("--apply", action="store_true")
    r.add_argument("--mirror", action="store_true")
    r.add_argument("--revert", action="store_true")
    r.add_argument("--diff", action="store_true")
    r.add_argument("--no-sentinels", action="store_true")
    r.add_argument("--workspace", help="source jobi workspace path")
    r.set_defaults(func=cmd_rewrite_jobs)

    v = sub.add_parser("verify", help="invariant checks (read-only)", parents=[common])
    v.set_defaults(func=cmd_verify)

    f = sub.add_parser("make-fixture", help="write scrubbed fixture dir")
    f.add_argument("dir")
    f.set_defaults(func=cmd_make_fixture)

    ret = sub.add_parser("retire-jobi", help="print manual retirement steps", parents=[common])
    ret.set_defaults(func=cmd_retire_jobi)

    args = p.parse_args(argv)
    return args.func(args)


if __name__ == "__main__":
    sys.exit(main())

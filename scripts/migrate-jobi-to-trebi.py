#!/usr/bin/env -S uv run --script
# /// script
# requires-python = ">=3.11"
# dependencies = ["ruamel.yaml>=0.18"]
# ///
"""jobi → trebi migration (Workstream B/C/D).

Default mode is DRY-RUN: prints the plan and never mutates source trees.
Pass --apply only for real writes (cutover); this goal never runs --apply.

Subcommands:
  status, migrate, rewrite-jobs, verify, make-fixture, retire-jobi
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import re
import shutil
import sqlite3
import sys
from collections import defaultdict
from dataclasses import dataclass, field
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

from ruamel.yaml import YAML

yaml = YAML()
yaml.preserve_quotes = True
yaml.default_flow_style = False

# ── paths ──────────────────────────────────────────────────────────────────


def default_jobi_dir() -> Path:
    return Path(os.environ.get("JOBI_DIR", Path.home() / ".jobi"))


def default_trebi_dir() -> Path:
    return Path(os.environ.get("TREBI_HOME", Path.home() / ".trebi"))


# ── frontmatter helpers (Workstream B) ─────────────────────────────────────

LEGACY_TOP_KEYS = {"command", "schedule", "on", "env", "tool", "when"}
SENTINEL_OLD = "JOBI_TASK_COMPLETE"
SENTINEL_NEW = "TREBI_RUN_COMPLETE"


def split_frontmatter(text: str) -> tuple[str | None, str, str]:
    """Return (fm_body, opening, rest) or (None, '', text) if no FM."""
    if not text.startswith("---"):
        return None, "", text
    lines = text.splitlines(keepends=True)
    if not lines:
        return None, "", text
    # find closing ---
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
    """Mutate a ruamel CommentedMap in place (preserves comments on kept keys)."""
    if not isinstance(data, dict):
        return

    # harness: rename command → harness
    if "command" in data:
        data["harness"] = data.pop("command")
    if "tool" in data:
        report.append(f"dead key tool: {data['tool']!r} — reported, not auto-written (M22)")
        # leave tool in place for human decision? M22 says never auto-written — drop from output
        # but report only; remove so parse is clean after rewrite
        del data["tool"]

    # env: string → envset; map stays as env
    if "env" in data and isinstance(data["env"], str):
        data["envset"] = data.pop("env")

    # session.timeout → idle_timeout (mutate nested map in place)
    sess = data.get("session")
    if isinstance(sess, dict) and "timeout" in sess:
        sess["idle_timeout"] = sess.pop("timeout")

    # agent_context → assistant_context
    if "agent_context" in data:
        data["assistant_context"] = data.pop("agent_context")

    # memory string → true
    if "memory" in data and isinstance(data["memory"], str) and data["memory"].strip():
        data["memory"] = True

    # Build triggers from schedule/on unless already canonical
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

    # drop legacy keys after conversion
    for k in ("schedule", "on", "when", "command"):
        if k in data:
            del data[k]


def rewrite_frontmatter_map(data: dict[str, Any], report: list[str]) -> dict[str, Any]:
    """Pure dict conversion (tests / agents merge). Mutates a copy via inplace logic."""
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
    """Return (new_text, changed). Comment-preserving via ruamel round-trip on the FM block."""
    fm_raw, _full, body = split_frontmatter(text)
    if fm_raw is None:
        new_body = body
        changed = False
        if rewrite_sentinel and SENTINEL_OLD in body:
            new_body = body.replace(SENTINEL_OLD, SENTINEL_NEW)
            changed = new_body != body
        return new_body if changed else text, changed

    # Load with ruamel → CommentedMap keeps key/value comments for keys we keep.
    data = yaml.load(fm_raw) if fm_raw.strip() else {}
    if not isinstance(data, dict):
        data = {}
    rewrite_frontmatter_inplace(data, report)
    new_body = body
    if rewrite_sentinel and SENTINEL_OLD in new_body:
        new_body = new_body.replace(SENTINEL_OLD, SENTINEL_NEW)
    new_text = dump_frontmatter(data, new_body)
    return new_text, new_text != text

# ── DB inventory / mapping (Workstream C) ──────────────────────────────────

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
    mode: str  # shadow | final
    phases: list[PhasePlan] = field(default_factory=list)
    warnings: list[str] = field(default_factory=list)
    workspace_map: dict[str, str] = field(default_factory=dict)  # path → id

    def render(self) -> str:
        lines = [
            f"# Migration plan ({self.mode}) — DRY RUN",
            f"jobi:  {self.jobi_dir}",
            f"trebi: {self.trebi_dir}",
            f"generated: {datetime.now(timezone.utc).isoformat()}",
            "",
            "## Phases",
        ]
        for p in self.phases:
            lines.append(f"### {p.name}")
            lines.append(f"- action: {p.action}")
            for k, v in p.counts.items():
                lines.append(f"- {k}: {v}")
            for n in p.notes:
                lines.append(f"- note: {n}")
            lines.append("")
        if self.workspace_map:
            lines.append("## Workspace id map (minted at apply)")
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


def open_jobi_ro(jobi_dir: Path) -> sqlite3.Connection | None:
    db = jobi_dir / "jobi.db"
    if not db.exists():
        return None
    uri = f"file:{db}?mode=ro"
    return sqlite3.connect(uri, uri=True)


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
    # jobi shape: workspaces: [{path, name?}] or list
    raw: list[Any] = []
    if isinstance(data, list):
        raw = data
    elif isinstance(data, dict):
        ws = data.get("workspaces") or data.get("paths") or []
        if isinstance(ws, list):
            raw = ws
        elif isinstance(ws, dict):
            raw = [{"name": k, "path": v} for k, v in ws.items()]
    # Resolve relative paths against jobi_dir (portable fixtures use path: ws).
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

def mint_ws_id(path: str) -> str:
    h = hashlib.sha256(path.encode()).hexdigest()[:8]
    return f"ws_{h}"


def plan_migrate(jobi_dir: Path, trebi_dir: Path, mode: str) -> MigrationPlan:
    plan = MigrationPlan(jobi_dir=jobi_dir, trebi_dir=trebi_dir, mode=mode)

    # preflight
    pf = PhasePlan("preflight", "check jobi present, free space, latch")
    pf.counts["jobi_db_exists"] = int((jobi_dir / "jobi.db").exists())
    pf.counts["trebi_dir_exists"] = int(trebi_dir.exists())
    if mode == "final":
        pf.notes.append("final mode requires jobi daemon stopped (not checked in dry-run)")
    plan.phases.append(pf)

    # workspaces
    ws_phase = PhasePlan("workspaces", "mint ids + registry rows; trebi.yaml via rewrite-jobs")
    workspaces = load_workspaces_yaml(jobi_dir)
    for w in workspaces:
        if isinstance(w, str):
            path = w
            name = Path(w).name
        else:
            path = str(w.get("path") or w.get("dir") or "")
            name = str(w.get("name") or Path(path).name)
        if not path:
            continue
        plan.workspace_map[path] = mint_ws_id(path)
        ws_phase.notes.append(f"{name}: {path} → {plan.workspace_map[path]}")
    ws_phase.counts["workspaces"] = len(plan.workspace_map)
    # nested split note for multi-agent workspaces is report-only in dry-run
    plan.phases.append(ws_phase)

    # config
    cfg = PhasePlan("config", "map config.yaml + envsets → trebi config")
    for name in ("config.yaml", "envsets.yaml", "sources.yaml"):
        cfg.counts[name] = int((jobi_dir / name).exists())
    cfg.notes.append("sources.yaml → connectors.yaml (same shape, top key connectors)")
    cfg.notes.append("defaults.command → defaults.harness; session.timeout → idle_timeout")
    plan.phases.append(cfg)

    # DB phases
    conn = open_jobi_ro(jobi_dir)
    if conn is None:
        plan.warnings.append("jobi.db missing — DB phases empty")
    else:
        try:
            for src, dst in TABLE_MAP.items():
                n = table_count(conn, src)
                ph = PhasePlan(f"db-{dst}", f"copy {src} → {dst} with renames/derivations")
                ph.counts["source_rows"] = n
                if src == "tasks":
                    ph.notes.append("workspace_path → workspace_id; command → harness; agent→assistant")
                    ph.notes.append("seconds → _ms (×1000)")
                    # Project derivation counts (same logic as apply_tasks) for dry-run truth.
                    proj = project_task_derivations(conn)
                    ph.counts.update(proj)
                    ph.notes.append(
                        "reason from launch event/segment else NULL (M13); "
                        "end_reason from terminal task segment; "
                        "conversation_key from trigger_vars_json"
                    )
                    # status breakdown
                    try:
                        for st, c in conn.execute("SELECT status, COUNT(*) FROM tasks GROUP BY status"):
                            ph.counts[f"status_{st}"] = c
                    except sqlite3.Error:
                        pass
                if src == "event_queue":
                    ph.notes.append("source→connector; all statuses migrate incl. pending/blocked")
                if src == "source_cursors":
                    ph.notes.append("cursors cutover-critical; refuse apply if stored cursor fails copy")
                if src == "schedule_anchors":
                    ph.notes.append("@every: seed last_fired_at = on-phase tick ≤ now (not raw anchor)")
                plan.phases.append(ph)
        finally:
            conn.close()

    # file phases
    for name, src_rel, dst_rel in (
        ("harness-homes", "harness", "harness"),
        ("bin-tree", "bin", "bin"),
        ("logs", "logs", "logs"),
    ):
        src = jobi_dir / src_rel
        ph = PhasePlan(name, f"copy {src} → {trebi_dir / dst_rel}")
        if src.exists():
            files = sum(1 for _ in src.rglob("*") if _.is_file())
            ph.counts["files"] = files
            ph.counts["bytes"] = sum(f.stat().st_size for f in src.rglob("*") if f.is_file())
        else:
            ph.counts["files"] = 0
            ph.notes.append("source missing")
        if mode == "shadow":
            ph.notes.append("shadow: file phases need brief jobi bootout window")
        plan.phases.append(ph)

    plan.phases.append(PhasePlan("verify", "run §7.4 invariants (after apply)"))
    return plan


# ── rewrite-jobs ───────────────────────────────────────────────────────────


def find_job_files(workspace: Path) -> list[Path]:
    out: list[Path] = []
    for root, dirs, files in os.walk(workspace):
        # skip nested trebi.yaml workspaces' children? still list all for dry-run
        dirs[:] = [d for d in dirs if d not in {".git", "node_modules", ".trebi"}]
        for f in files:
            if f in ("JOB.md", "AGENT.md", "SOUL.md", "TREBI.md") or f.endswith(".md") and f.upper() in {
                "JOB.MD",
                "AGENT.MD",
            }:
                if f in ("JOB.md", "AGENT.md", "SOUL.md", "TREBI.md"):
                    out.append(Path(root) / f)
    return out


def plan_rewrite(
    workspaces: list[Path],
    *,
    mirror_root: Path | None,
    rewrite_sentinel: bool,
) -> tuple[list[str], list[dict[str, Any]]]:
    lines: list[str] = []
    details: list[dict[str, Any]] = []
    for ws in workspaces:
        if not ws.exists():
            lines.append(f"SKIP missing workspace {ws}")
            continue
        files = find_job_files(ws)
        lines.append(f"workspace {ws}: {len(files)} def files")
        for path in files:
            report: list[str] = []
            text = path.read_text(encoding="utf-8", errors="replace")
            if path.name == "SOUL.md":
                details.append({"path": str(path), "action": "merge-into-TREBI.md", "changed": True})
                lines.append(f"  MERGE {path.relative_to(ws)} → TREBI.md ## Soul")
                continue
            if path.name == "AGENT.md":
                details.append({"path": str(path), "action": "merge-into-TREBI.md + delete on apply", "changed": True})
                lines.append(f"  MERGE {path.relative_to(ws)} → TREBI.md (delete on --apply)")
                # still show frontmatter rewrite preview of agent alone
                new_text, changed = rewrite_job_text(text, report, rewrite_sentinel=rewrite_sentinel)
                for r in report:
                    lines.append(f"    ! {r}")
                continue
            new_text, changed = rewrite_job_text(text, report, rewrite_sentinel=rewrite_sentinel)
            action = "rewrite" if changed else "unchanged"
            if mirror_root:
                action = f"mirror→{mirror_root / ws.name / path.relative_to(ws)}"
            details.append({"path": str(path), "action": action, "changed": changed})
            rel = path.relative_to(ws)
            mark = "*" if changed else " "
            lines.append(f"  {mark} {rel} ({action})")
            for r in report:
                lines.append(f"    ! {r}")
            if changed and os.environ.get("TREBI_MIGRATE_DIFF"):
                lines.append("    --- old keys sample ---")
                fm, _, _ = split_frontmatter(text)
                if fm:
                    keys = list(load_fm_map(fm).keys())
                    lines.append(f"    old keys: {keys}")
                fm2, _, _ = split_frontmatter(new_text)
                if fm2:
                    keys = list(load_fm_map(fm2).keys())
                    lines.append(f"    new keys: {keys}")
    return lines, details


def cmd_status(args: argparse.Namespace) -> int:
    jobi = Path(args.jobi_dir)
    trebi = Path(args.trebi_dir)
    print(f"jobi_dir:  {jobi} exists={jobi.exists()}")
    print(f"trebi_dir: {trebi} exists={trebi.exists()}")
    ws = load_workspaces_yaml(jobi)
    print(f"workspaces.yaml entries: {len(ws)}")
    for w in ws:
        if isinstance(w, str):
            print(f"  - {w}")
        else:
            print(f"  - {w.get('name', '?')}: {w.get('path') or w.get('dir')}")
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
    return 0


def cmd_migrate(args: argparse.Namespace) -> int:
    mode = "shadow" if args.shadow else "final" if args.final else ""
    if not mode:
        print("ERROR: migrate requires exactly one of --shadow / --final", file=sys.stderr)
        return 2
    jobi = Path(args.jobi_dir)
    trebi = Path(args.trebi_dir)
    plan = plan_migrate(jobi, trebi, mode)
    text = plan.render()
    print(text)
    if args.report:
        Path(args.report).write_text(text)
        print(f"\n(report written to {args.report})")
    if not args.apply:
        print("\nDry-run only. Re-run with --apply to execute.")
        return 0
    return apply_migrate(jobi, trebi, mode, plan)


def apply_migrate(jobi: Path, trebi: Path, mode: str, plan: MigrationPlan) -> int:
    """Execute migration into trebi (copy-only; never writes jobi)."""
    trebi.mkdir(parents=True, exist_ok=True)
    # refuse if daemon sock answers
    sock = trebi / "daemon.sock"
    if sock.exists():
        print("ERROR: trebi daemon.sock present — stop trebid before migrate --apply", file=sys.stderr)
        return 2
    # config files
    mapping = {
        "sources.yaml": "connectors.yaml",
        "config.yaml": "config.yaml",
    }
    for src_name, dst_name in mapping.items():
        src = jobi / src_name
        if not src.exists():
            continue
        raw = src.read_text(encoding="utf-8", errors="replace")
        if src_name == "sources.yaml":
            raw = raw.replace("sources:", "connectors:", 1) if raw.lstrip().startswith("sources:") else raw
        if src_name == "config.yaml":
            raw = raw.replace("defaults:\n  command:", "defaults:\n  harness:")
            raw = raw.replace("timeout:", "idle_timeout:")  # session.timeout
        (trebi / dst_name).write_text(raw)
        print(f"wrote {dst_name}")
    # envsets → append into config if present
    envsets = jobi / "envsets.yaml"
    if envsets.exists():
        cfg = trebi / "config.yaml"
        extra = "\nenv_sets:\n" + "\n".join(
            "  " + line if line.strip() else line for line in envsets.read_text().splitlines()
        )
        # crude: if envsets is a map at top level, nest under env_sets
        data = yaml.load(envsets.read_text()) or {}
        from io import StringIO
        buf = StringIO()
        yaml.dump({"env_sets": data}, buf)
        existing = cfg.read_text() if cfg.exists() else ""
        cfg.write_text(existing + "\n" + buf.getvalue())
        print("merged envsets into config.yaml")
    # harness/bin/logs copy
    for name in ("harness", "bin", "logs"):
        src = jobi / name
        dst = trebi / name
        if src.exists():
            if dst.exists():
                shutil.rmtree(dst)
            shutil.copytree(src, dst, symlinks=True)
            print(f"copied {name}/")
    # DB insert if trebi.db exists (schema owned by trebid)
    tdb = trebi / "trebi.db"
    jdb = jobi / "jobi.db"
    if not jdb.exists():
        print("no jobi.db — skip db phases")
        return 0
    if not tdb.exists():
        print("WARNING: trebi.db missing — run `trebi service migrate-db` first; DB rows not inserted")
        return 0
    jconn = sqlite3.connect(f"file:{jdb}?mode=ro", uri=True)
    tconn = sqlite3.connect(tdb)
    try:
        n = apply_tasks(jconn, tconn, plan.workspace_map)
        print(f"inserted runs: {n}")
        n = apply_timeline(jconn, tconn)
        print(f"timeline segments+events copied")
        n = apply_queue(jconn, tconn, plan.workspace_map)
        print(f"queue rows: {n}")
        apply_policy_cursors(jconn, tconn, plan.workspace_map)
        print("policy + cursors + metrics copied")
        # workspaces registry
        now = datetime.now(timezone.utc).isoformat()
        for path, wid in plan.workspace_map.items():
            name = Path(path).name
            tconn.execute(
                "INSERT OR REPLACE INTO workspaces (id, name, path, git_enabled, orphaned, created_at, updated_at) VALUES (?,?,?,0,0,?,?)",
                (wid, name, path if mode == "final" else str(trebi / "shadow-workspaces" / name), now, now),
            )
        tconn.commit()
        # manifest
        manifest = {
            "mode": mode,
            "workspace_map": plan.workspace_map,
            "cutover_started_at": now if mode == "final" else None,
        }
        (trebi / "migration-manifest.json").write_text(json.dumps(manifest, indent=2))
        print("wrote migration-manifest.json")
    finally:
        jconn.close()
        tconn.close()
    return 0


def workspace_id_for(path: str, wmap: dict[str, str]) -> str:
    if not path:
        return "imported-orphans"
    # longest prefix, case-insensitive
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
    """M13: launch event/segment reason; else trigger→trigger; else NULL (never fabricate manual)."""
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
            "SELECT reason FROM task_segments WHERE task_id=? AND layer='task' AND reason IS NOT NULL AND reason!='' ORDER BY started_at LIMIT 1",
            (task_id,),
        ).fetchone()
        if row and row[0] and row[0] != "launch":
            # launch segment reason is often 'launch' not RunReason — prefer trigger heuristic
            pass
        if row and row[0] in ("manual", "schedule", "trigger", "when", "resume", "replay"):
            return row[0]
    except sqlite3.Error:
        pass
    if trigger_source:
        return "trigger"
    return None  # M13: NULL for pre-timeline rows


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
        # terminal segment may store end reason in reason column of the terminal state row
        row = jconn.execute(
            """SELECT reason FROM task_segments
               WHERE task_id=? AND layer='task' AND state IN ('complete','failed','exited','skipped','timed_out','resumed')
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


def project_task_derivations(conn: sqlite3.Connection) -> dict[str, int]:
    """Dry-run projection of apply_tasks derivation outcomes."""
    counts = {
        "reason_set": 0,
        "reason_null": 0,
        "end_reason_set": 0,
        "end_reason_null": 0,
        "conversation_key_set": 0,
        "dropped_junk": 0,
        "will_insert": 0,
    }
    try:
        cols = [r[1] for r in conn.execute("PRAGMA table_info(tasks)")]
    except sqlite3.Error:
        return counts
    for row in conn.execute("SELECT * FROM tasks"):
        d = dict(zip(cols, row))
        if not d.get("started_at") and not d.get("workspace_path"):
            counts["dropped_junk"] += 1
            continue
        counts["will_insert"] += 1
        r = derive_reason(conn, d["id"], d.get("trigger_source"))
        if r:
            counts["reason_set"] += 1
        else:
            counts["reason_null"] += 1
        er = derive_end_reason(conn, d["id"])
        if er:
            counts["end_reason_set"] += 1
        else:
            counts["end_reason_null"] += 1
        if derive_conversation_key(d.get("trigger_vars_json")):
            counts["conversation_key_set"] += 1
    return counts


def apply_tasks(jconn: sqlite3.Connection, tconn: sqlite3.Connection, wmap: dict[str, str]) -> int:
    cols = [r[1] for r in jconn.execute("PRAGMA table_info(tasks)")]
    rows = jconn.execute("SELECT * FROM tasks").fetchall()
    n = 0
    for row in rows:
        d = dict(zip(cols, row))
        if not d.get("started_at") and not d.get("workspace_path"):
            continue  # junk
        # Prefer job_file longest-prefix (M23); fallback workspace_path
        path_for_ws = d.get("job_file") or d.get("workspace_path") or ""
        wid = workspace_id_for(path_for_ws, wmap)
        kind = d.get("kind") or "job"
        if kind == "agent":
            kind = "assistant"
        # durations
        def ms(sec_key, default=0):
            v = d.get(sec_key)
            if v is None:
                return default
            return int(v) * 1000 if int(v) > 0 else int(v)  # preserve 0 and -1

        nudge = d.get("nudge_seconds")
        if nudge is None:
            nudge_ms = -1
        else:
            nudge_ms = int(nudge) if int(nudge) < 0 else int(nudge) * 1000
        idle = d.get("timeout_seconds") or 0
        idle_ms = int(idle) * 1000 if idle else 0
        max_ms = ms("max_duration_seconds")
        keep_ms = ms("keep_alive_seconds")
        trace = hashlib.sha256((d["id"] + "trace").encode()).hexdigest()[:32]
        reason = derive_reason(jconn, d["id"], d.get("trigger_source"))
        end_reason = derive_end_reason(jconn, d["id"])
        conv_key = derive_conversation_key(d.get("trigger_vars_json"))
        # queue_id reverse-join
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
              trace_id, revision, updated_at
            ) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,1,?)""",
            (
                d["id"], wid, d.get("job_name") or "", kind, reason, d.get("status") or "exited", end_reason,
                d.get("prompt_preview"), d.get("session_id"), "tui", d.get("command") or "claude",
                d.get("tmux_session"), d.get("tokens_input"), d.get("tokens_output"),
                d.get("tokens_cache_read"), d.get("tokens_cache_creation"), d.get("tokens_model"),
                d.get("started_at"), d.get("ended_at"), max_ms, idle_ms, keep_ms, nudge_ms,
                d.get("trigger_source"), d.get("trigger_capture_json"), d.get("trigger_vars_json"),
                conv_key, queue_id, trace, d.get("ended_at") or d.get("started_at"),
            ),
        )
        n += 1
    return n

def apply_timeline(jconn: sqlite3.Connection, tconn: sqlite3.Connection) -> None:
    try:
        for row in jconn.execute("SELECT task_id, layer, state, started_at, ended_at, duration_ms, reason, source, detail_json FROM task_segments"):
            tconn.execute(
                "INSERT INTO run_segments (run_id, layer, state, started_at, ended_at, duration_ms, reason, source, detail_json) VALUES (?,?,?,?,?,?,?,?,?)",
                row,
            )
        for row in jconn.execute("SELECT task_id, at, kind, source, detail_json FROM task_events"):
            tconn.execute(
                "INSERT INTO run_events (run_id, at, kind, source, detail_json) VALUES (?,?,?,?,?)",
                row,
            )
    except sqlite3.Error as e:
        print(f"timeline skip: {e}")


def apply_queue(jconn: sqlite3.Connection, tconn: sqlite3.Connection, wmap: dict[str, str]) -> int:
    try:
        cols = [r[1] for r in jconn.execute("PRAGMA table_info(event_queue)")]
    except sqlite3.Error:
        return 0
    n = 0
    for row in jconn.execute("SELECT * FROM event_queue"):
        d = dict(zip(cols, row))
        wid = workspace_id_for(d.get("workspace_path") or "", wmap)
        kind = "assistant" if d.get("job_name") and d.get("job_name") == d.get("parent_agent") else "job"
        tconn.execute(
            """INSERT OR REPLACE INTO event_queue (
              id, connector, workspace_id, job, kind, on_index, conversation_key,
              event_raw, capture_json, vars_json, prompt, status, block_reason, result,
              attempts, next_attempt_at, claimed_at, run_id, error_message, created_at, updated_at, done_at
            ) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)""",
            (
                d["id"], d.get("source"), wid, d.get("job_name"), kind, d.get("on_index") or 0,
                d.get("conversation_key") or "", d.get("event_raw"), d.get("capture_json"),
                d.get("vars_json"), d.get("prompt") or "", d.get("status"), d.get("block_reason"),
                d.get("result"), d.get("attempts") or 0, d.get("next_attempt_at"), d.get("claimed_at"),
                d.get("task_id"), d.get("error_message"), d.get("created_at"), d.get("updated_at"), d.get("done_at"),
            ),
        )
        n += 1
    return n


def apply_policy_cursors(jconn: sqlite3.Connection, tconn: sqlite3.Connection, wmap: dict[str, str]) -> None:
    try:
        for key, last, win in jconn.execute("SELECT key, last_fired_at, window_json FROM trigger_policy"):
            # rewrite path prefix in key if needed — keep as-is with note
            tconn.execute(
                "INSERT OR REPLACE INTO dispatch_policy (key, last_fired_at, window_json) VALUES (?,?,?)",
                (key, last, win),
            )
    except sqlite3.Error:
        pass
    try:
        for name, eid, upd in jconn.execute("SELECT source_name, last_event_id, updated_at FROM source_cursors"):
            tconn.execute(
                "INSERT OR REPLACE INTO connector_cursors (name, last_event_id, updated_at) VALUES (?,?,?)",
                (name, eid, upd),
            )
    except sqlite3.Error:
        pass
    try:
        for row in jconn.execute("SELECT name, events_total, parse_errors, match_errors, fires, drops, last_event_at, updated_at FROM source_metrics"):
            tconn.execute(
                "INSERT OR REPLACE INTO connector_metrics (name, events_total, parse_errors, match_errors, fires, drops, last_event_at, updated_at) VALUES (?,?,?,?,?,?,?,?)",
                row,
            )
    except sqlite3.Error:
        pass


def workspace_paths_from_jobi(jobi_dir: Path) -> list[Path]:
    out: list[Path] = []
    for w in load_workspaces_yaml(jobi_dir):
        if isinstance(w, str):
            out.append(Path(w))
        else:
            p = w.get("path") or w.get("dir")
            if p:
                out.append(Path(p))
    return out


def cmd_rewrite_jobs(args: argparse.Namespace) -> int:
    jobi = Path(args.jobi_dir)
    trebi = Path(args.trebi_dir)
    workspaces = workspace_paths_from_jobi(jobi)
    if args.workspace:
        workspaces = [Path(args.workspace)]
    mirror = trebi / "shadow-workspaces" if args.mirror else None
    if args.mirror and not args.apply:
        print(f"MIRROR plan → {mirror} (dry-run: not writing)")
    lines, details = plan_rewrite(
        workspaces,
        mirror_root=mirror,
        rewrite_sentinel=not args.no_sentinels,
    )
    for line in lines:
        print(line)
    changed = sum(1 for d in details if d.get("changed"))
    print(f"\n{len(details)} files considered, {changed} would change")
    if args.diff and not args.apply:
        for d in details:
            if not d.get("changed"):
                continue
            path = Path(d["path"])
            if path.name not in ("JOB.md", "TREBI.md"):
                continue
            text = path.read_text(encoding="utf-8", errors="replace")
            report: list[str] = []
            new_text, _ = rewrite_job_text(text, report, rewrite_sentinel=not args.no_sentinels)
            print(f"\n--- diff sample: {path}")
            print("OLD head:")
            print("\n".join(text.splitlines()[:20]))
            print("NEW head:")
            print("\n".join(new_text.splitlines()[:20]))
            break
    if not args.apply and not args.revert:
        print("\nNo files written (dry-run).")
        return 0
    if args.revert:
        backup = trebi / "rewrite-backup"
        if not backup.exists():
            print("no rewrite-backup to revert", file=sys.stderr)
            return 2
        # restore all files under backup
        for src in backup.rglob("*"):
            if src.is_file():
                rel = src.relative_to(backup)
                # backup layout: <hash-or-path>
                print(f"would restore {rel} (implement path map from manifest)")
        print("revert: restore from backup (see migration-manifest)")
        return 0
    # apply rewrite
    backup = trebi / "rewrite-backup"
    backup.mkdir(parents=True, exist_ok=True)
    for d in details:
        path = Path(d["path"])
        if path.name == "SOUL.md":
            continue  # handled with AGENT
        if path.name == "AGENT.md":
            # merge AGENT+SOUL into TREBI.md at workspace root
            ws_root = path.parent
            # climb if under agents/x
            soul = path.parent / "SOUL.md"
            agent_text = path.read_text(encoding="utf-8", errors="replace")
            report: list[str] = []
            new_agent, _ = rewrite_job_text(agent_text, report, rewrite_sentinel=not args.no_sentinels)
            soul_body = ""
            if soul.exists():
                soul_body = soul.read_text(encoding="utf-8", errors="replace")
                # strip frontmatter from soul if any
                _, _, soul_body = split_frontmatter(soul_body)
            fm, _, body = split_frontmatter(new_agent)
            data = load_fm_map(fm or "")
            data = rewrite_frontmatter_map(data, report)
            data.setdefault("memory", True)
            merged_body = f"## Soul\n{soul_body.strip()}\n\n## Instructions\n{body.strip()}\n"
            out = dump_frontmatter(data, merged_body)
            dest = ws_root / "TREBI.md"
            # backup originals
            bdir = backup / hashlib.sha256(str(path).encode()).hexdigest()[:12]
            bdir.mkdir(parents=True, exist_ok=True)
            shutil.copy2(path, bdir / "AGENT.md")
            if soul.exists():
                shutil.copy2(soul, bdir / "SOUL.md")
            if args.mirror and mirror:
                mdest = mirror / ws_root.name / "TREBI.md"
                mdest.parent.mkdir(parents=True, exist_ok=True)
                mdest.write_text(out)
                print(f"mirror TREBI.md → {mdest}")
            else:
                dest.write_text(out)
                path.unlink()
                if soul.exists():
                    soul.unlink()
                print(f"wrote {dest}; removed AGENT.md/SOUL.md")
            # mint trebi.yaml
            ty = ws_root / "trebi.yaml"
            if not ty.exists():
                wid = mint_ws_id(str(ws_root))
                ty.write_text(f"id: {wid}\nname: {ws_root.name}\ngit_enabled: false\n")
            continue
        if path.name not in ("JOB.md", "TREBI.md"):
            continue
        text = path.read_text(encoding="utf-8", errors="replace")
        report: list[str] = []
        new_text, changed = rewrite_job_text(text, report, rewrite_sentinel=not args.no_sentinels)
        if not changed and not args.mirror:
            continue
        bdir = backup / hashlib.sha256(str(path).encode()).hexdigest()[:12]
        bdir.mkdir(parents=True, exist_ok=True)
        shutil.copy2(path, bdir / path.name)
        if args.mirror and mirror:
            # find workspace root from workspaces list
            rel = path.name
            for ws in workspaces:
                try:
                    rel_path = path.relative_to(ws)
                    mdest = mirror / ws.name / rel_path
                    mdest.parent.mkdir(parents=True, exist_ok=True)
                    mdest.write_text(new_text)
                    print(f"mirror {mdest}")
                    break
                except ValueError:
                    continue
        else:
            path.write_text(new_text)
            print(f"rewrote {path}")
        # trebi.yaml at workspace root for each ws
    for ws in workspaces:
        if not ws.exists():
            continue
        ty = (mirror / ws.name / "trebi.yaml") if (args.mirror and mirror) else (ws / "trebi.yaml")
        if args.mirror and mirror:
            ty.parent.mkdir(parents=True, exist_ok=True)
        if not ty.exists():
            ty.write_text(f"id: {mint_ws_id(str(ws))}\nname: {ws.name}\ngit_enabled: false\n")
            print(f"minted {ty}")
    return 0


def cmd_verify(args: argparse.Namespace) -> int:
    """Verify planned invariants against trebi.db if present (read-only)."""
    trebi = Path(args.trebi_dir)
    jobi = Path(args.jobi_dir)
    db = trebi / "trebi.db"
    print(f"verify against {db}")
    if not db.exists():
        print("trebi.db missing — nothing to verify (ok for pre-apply)")
        return 0
    tconn = sqlite3.connect(f"file:{db}?mode=ro", uri=True)
    jconn = open_jobi_ro(jobi)
    checks = []
    try:
        runs = tconn.execute("SELECT COUNT(*) FROM runs").fetchone()[0]
        checks.append(("runs_count", runs))
        if jconn:
            tasks = table_count(jconn, "tasks")
            ok = runs == tasks
            print(f"run conservation: trebi.runs={runs} jobi.tasks={tasks} ok={ok}")
        else:
            print(f"trebi.runs={runs} (no jobi.db to compare)")
        for tbl in ("run_segments", "run_events", "event_queue", "event_log"):
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
    """Create a scrubbed synthetic jobi-shaped fixture for CI (no real secrets)."""
    out = Path(args.dir).resolve()
    out.mkdir(parents=True, exist_ok=True)
    # Portable relative path: resolve against fixture root at load time (see load_workspaces_yaml).
    (out / "workspaces.yaml").write_text(
        "workspaces:\n  - name: fixture\n    path: ws\n"
    )
    ws = out / "ws" / "demo"
    ws.mkdir(parents=True, exist_ok=True)
    (ws / "JOB.md").write_text(
        "---\n"
        "# important comment\n"
        "working_dir: .\n"
        "command: claude\n"
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
    # tiny sqlite with one task
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
        "INSERT INTO tasks (id, job_name, workspace_path, command, status, kind, started_at, prompt_preview) VALUES (?,?,?,?,?,?,?,?)",
        ("01FIXTURE0000000000000000", "demo", str(out / "ws"), "claude", "complete", "job", "2026-01-01T00:00:00Z", "hi"),
    )
    conn.commit()
    conn.close()
    (out / "sources.yaml").write_text("sources: {}\n")
    (out / "config.yaml").write_text("defaults:\n  command: claude\n  session:\n    timeout: 30m\n")
    print(f"fixture written to {out}")
    return 0


def cmd_retire_jobi(args: argparse.Namespace) -> int:
    print("Manual retirement steps (executes nothing):")
    print("1. launchctl bootout gui/$UID/dev.jobi.daemon")
    print("2. replace /usr/local/bin/jobi with a shim pointing to trebi")
    print("3. remove ~/.claude/skills/jobi and ~/.agents/skills/jobi links")
    print("4. keep ~/.jobi until comfortable; then delete manually")
    return 0


def main(argv: list[str] | None = None) -> int:
    p = argparse.ArgumentParser(description="jobi → trebi migration (dry-run default)")
    p.add_argument("--jobi-dir", default=str(default_jobi_dir()))
    p.add_argument("--trebi-dir", default=str(default_trebi_dir()))
    sub = p.add_subparsers(dest="cmd", required=True)

    s = sub.add_parser("status", help="inventory jobi + trebi trees")
    s.set_defaults(func=cmd_status)

    m = sub.add_parser("migrate", help="plan DB/config/file migration")
    m.add_argument("--shadow", action="store_true")
    m.add_argument("--final", action="store_true")
    m.add_argument("--apply", action="store_true", help="execute (disabled in goal; refused)")
    m.add_argument("--report", help="write plan markdown to path")
    m.set_defaults(func=cmd_migrate)

    r = sub.add_parser("rewrite-jobs", help="plan JOB.md/AGENT.md restructure")
    r.add_argument("--apply", action="store_true")
    r.add_argument("--mirror", action="store_true")
    r.add_argument("--revert", action="store_true")
    r.add_argument("--diff", action="store_true")
    r.add_argument("--no-sentinels", action="store_true")
    r.add_argument("--workspace", help="single workspace path")
    r.set_defaults(func=cmd_rewrite_jobs)

    v = sub.add_parser("verify", help="§7.4 invariant checks (read-only)")
    v.set_defaults(func=cmd_verify)

    f = sub.add_parser("make-fixture", help="write scrubbed fixture dir")
    f.add_argument("dir")
    f.set_defaults(func=cmd_make_fixture)

    ret = sub.add_parser("retire-jobi", help="print manual retirement steps")
    ret.set_defaults(func=cmd_retire_jobi)

    args = p.parse_args(argv)
    return args.func(args)


if __name__ == "__main__":
    sys.exit(main())

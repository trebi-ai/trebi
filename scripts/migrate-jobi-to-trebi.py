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
  uv run scripts/migrate-jobi-to-trebi.py convert-memory path/to/MEMORY.md
  uv run scripts/migrate-jobi-to-trebi.py migrate --shadow --job receipts --target-workspace ellis
  uv run scripts/migrate-jobi-to-trebi.py migrate --shadow --agent scout --target-workspace ellis

--job limits file rewrite and DB copy to one job. --agent limits both to one
agent and its jobs (requires --target-workspace); jobs land at
agents/<name>/<job>. --target-workspace is the trebi destination (id, name, or
path). Incremental --job/--agent --apply copies into the target and leaves
~/.jobi files untouched. Full rewrite-jobs --apply without --job still rewrites
in place (cutover). rewrite-jobs converts jobi MEMORY.md (extra ## headings)
to trebi v1; convert-memory does only that.

Schema v16 (store.SchemaVersion): runs gained inputs_json/memory_hash/outcome*/
runtime_pid/attach_kind, and event_queue/events use connector, not integration.
This script writes the columns that have a jobi source; the rest stay NULL and
trebi's readers treat them as unset.

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
# Schema v11: run_segments.layer values agent→session, task→run.
LAYER_MAP = {"agent": "session", "task": "run"}
SECRET_KEY_RE = re.compile(r"(TOKEN|KEY|SECRET|PASSWORD|AUTH)", re.I)
# store.SchemaVersion at the time this script was last synced. A trebi.db older
# than this is a pre-migration database: bump it with `trebi service migrate-db`
# before applying DB phases (the script writes v16 columns).
EXPECTED_SCHEMA = 16


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


def _as_abs_working_dir(wd: str, src_dir: Path) -> Path:
    """jobi resolved a relative working_dir against the job folder; trebi hands
    the string to the harness as-is (cmd.Dir), so the daemon's cwd would win.
    Resolve it the same way jobi did."""
    p = Path(wd).expanduser()
    if p.is_absolute():
        return p
    return (src_dir / p).resolve()


def rewrite_frontmatter_inplace(
    data: Any, report: list[str], *, src_dir: Path | None = None
) -> None:
    if not isinstance(data, dict):
        return

    # A working_dir that points at the job's own old folder goes stale once the
    # folder moves. ResolvedWorkingDir falls back to the job dir, so dropping it
    # keeps the same meaning and follows the move. Paths to an external project
    # are deliberate and stay (absolute, since trebi resolves a relative path
    # against the daemon cwd, not the job folder).
    wd = data.get("working_dir")
    if src_dir is not None and isinstance(wd, str) and wd.strip():
        resolved: Path | None
        try:
            resolved = _as_abs_working_dir(wd, src_dir)
        except OSError:
            resolved = None
        if resolved is not None:
            try:
                same = resolved == src_dir.resolve()
            except OSError:
                same = False
            if same:
                report.append("dropped self-referential working_dir: follows the job folder")
                del data["working_dir"]
            elif not Path(wd).expanduser().is_absolute():
                data["working_dir"] = str(resolved)
                report.append(
                    f"working_dir {wd!r} → {resolved} (jobi resolved it vs. the job folder)"
                )

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

    # Ownership is the folder since schema v12 (plan A.3): these pointer keys
    # are dead and trebi's parser ignores them.
    for dead in ("oversight", "parent_agent", "jobs"):
        if dead in data:
            report.append(f"dropped dead key {dead}: ownership is the folder")
            del data[dead]

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


def rewrite_job_text(
    text: str,
    report: list[str],
    *,
    rewrite_sentinel: bool = True,
    rewrite_memory_headings: bool = True,
    src_dir: Path | None = None,
) -> tuple[str, bool]:
    fm_raw, _full, body = split_frontmatter(text)

    def touch_body(b: str) -> str:
        if rewrite_sentinel and SENTINEL_OLD in b:
            b = b.replace(SENTINEL_OLD, SENTINEL_NEW)
        if rewrite_memory_headings:
            b, notes = rewrite_memory_headings_in_job_body(b)
            report.extend(notes)
        return b

    if fm_raw is None:
        new_body = touch_body(body)
        return (new_body, True) if new_body != text else (text, False)

    data = yaml.load(fm_raw) if fm_raw.strip() else {}
    if not isinstance(data, dict):
        data = {}
    rewrite_frontmatter_inplace(data, report, src_dir=src_dir)
    new_text = dump_frontmatter(data, touch_body(body))
    return new_text, new_text != text


# ── jobi MEMORY.md → trebi v1 ──────────────────────────────────────────────
#
# Trebi's built-in memory.Migrate only handles B22 ledgers (no H2 headings).
# jobi files used extra ## sections (Shipped, Seen, Sources, …) which Parse
# treats as unknown_section — they stay invalid until rewritten. Extra H2s
# become ### under Context/State so rotation/dedup lists survive the Log cap.
# Prose is copied as-is (addresses like jobi@ocral.org stay).


MEMORY_MARKER = (
    '<!-- trebi memory v1. Keep these four sections in this order. '
    'Do not add other "## " headings. -->'
)
MEMORY_SECTION_COMMENTS = {
    "Context": "<!-- Durable facts about this job: what it does, where things are, how they work. Free form. Rewrite when facts change. -->",
    "State": "<!-- Current situation, open issues, next steps. Free form. Rewrite each run. -->",
    "Lessons": "<!-- One bullet per lesson: `- [YYYY-MM-DD HH:MM] text`. Never prune by age. -->",
    "Log": "<!-- One bullet per event, observation, or stat: `- [YYYY-MM-DD HH:MM] text`. Prune the oldest entries when over the cap. -->",
}
CANONICAL_H2 = {
    "context": "Context",
    "state": "State",
    "lessons": "Lessons",
    "lessons learned": "Lessons",
    "log": "Log",
}
# Extra jobi H2s that are durable facts → Context as H3. Everything else extra
# becomes State as H3 (operational lists: Shipped, Seen, Sources, Reported).
CONTEXT_H2_PREFIXES = (
    "confirmed dead ends",
    "annual",
)
# JOB.md runbook headings / `## Foo` mentions that document memory lists.
# Demote so the next run writes ### under State instead of a new unknown ##.
RUNBOOK_MEMORY_H2_PREFIXES = (
    "shipped",
    "seen",
    "sources",
    "reported",
    "candidates",
    "confirmed dead ends",
    "current regime",
    "metro watch",
    "active drivers",
    "recent readings",
    "outlook vs reality",
    "sources tested",
    "list reduced-share",
    "active listings",
)

VALID_MEMORY_ENTRY = re.compile(r"^- \[(\d{4}-\d{2}-\d{2})(?: (\d{2}:\d{2}))?\] (.+)$")
DUP_STAMP_ENTRY = re.compile(
    r"^- \[(\d{4}-\d{2}-\d{2}(?: \d{2}:\d{2})?)\] \[(\d{4}-\d{2}-\d{2}(?: \d{2}:\d{2})?)\] (.*)$"
)
BRACKET_STAMP_ENTRY = re.compile(
    r"^- (?:\*\*)?\[(\d{4}-\d{2}-\d{2})(?: (\d{2}:\d{2}))?([^\]]*)\]\*\*? ?(.*)$"
)
LEGACY_B22_ENTRY = re.compile(r"^\[(\d{4}-\d{2}-\d{2})\] ?(.*)$")
ANY_DATE = re.compile(r"\d{4}-\d{2}-\d{2}")
H2_LINE = re.compile(r"^## (?!#)(.+)$", re.MULTILINE)
INLINE_H2 = re.compile(r"`## ([^`]+)`")


def _is_h2(line: str) -> bool:
    return line.startswith("## ") and not line.startswith("###")


def _heading_key(title: str) -> str:
    return title.strip().lower()


def _canonical_h2(title: str) -> str | None:
    return CANONICAL_H2.get(_heading_key(title))


def _matches_h2_prefix(title: str, prefix: str) -> bool:
    key = _heading_key(title)
    if key == prefix:
        return True
    if prefix.endswith("#"):
        return key.startswith(prefix)
    return key.startswith(prefix + " ") or key.startswith(prefix + ":")


def _is_runbook_memory_h2(title: str) -> bool:
    return any(_matches_h2_prefix(title, p) for p in RUNBOOK_MEMORY_H2_PREFIXES)


def rewrite_memory_headings_in_job_body(body: str) -> tuple[str, list[str]]:
    """Demote extra jobi memory H2s in a JOB.md/AGENT.md runbook to ###."""
    n_head = 0
    n_inline = 0

    def heading(m: re.Match[str]) -> str:
        nonlocal n_head
        title = m.group(1).strip()
        if _is_runbook_memory_h2(title):
            n_head += 1
            return f"### {title}"
        return m.group(0)

    def inline(m: re.Match[str]) -> str:
        nonlocal n_inline
        title = m.group(1).strip()
        if _is_runbook_memory_h2(title):
            n_inline += 1
            return f"`### {title}`"
        return m.group(0)

    out = H2_LINE.sub(heading, body)
    out = INLINE_H2.sub(inline, out)
    notes: list[str] = []
    if n_head:
        notes.append(f"demoted {n_head} extra ## memory heading(s) to ###")
    if n_inline:
        notes.append(f"rewrote {n_inline} `## …` memory mention(s) to ###")
    return out, notes


def memory_skeleton() -> str:
    parts = [MEMORY_MARKER, "# Memory", ""]
    for name in ("Context", "State", "Lessons", "Log"):
        parts.append(f"## {name}\n{MEMORY_SECTION_COMMENTS[name]}\n")
    return "\n".join(parts)


def _trim_body(s: str) -> str:
    return s.strip("\n")


def _drop_h1(preamble: str) -> str:
    lines: list[str] = []
    for ln in preamble.split("\n"):
        if ln.startswith("# "):
            continue
        if ln.startswith("<!-- trebi memory v1"):
            continue
        lines.append(ln)
    return _trim_body("\n".join(lines))


def _stray_preamble(preamble: str) -> bool:
    for ln in _drop_h1(preamble).split("\n"):
        stripped = ln.strip()
        if stripped and not stripped.startswith("<!--"):
            return True
    return False


def split_memory_h2(text: str) -> tuple[str, list[tuple[str, str]]]:
    """Split on ## headings only (`###` stays in the body)."""
    preamble: list[str] = []
    sections: list[tuple[str, list[str]]] = []
    current: str | None = None
    buf: list[str] = []
    for line in text.split("\n"):
        if _is_h2(line):
            if current is None:
                preamble = buf
            else:
                sections.append((current, buf))
            current = line[3:].strip()
            buf = []
        else:
            buf.append(line)
    if current is None:
        preamble = buf
    else:
        sections.append((current, buf))
    return "\n".join(preamble), [(t, "\n".join(b)) for t, b in sections]


def _join_parts(parts: list[str]) -> str:
    kept = [_trim_body(p) for p in parts if _trim_body(p)]
    return "\n\n".join(kept)


def _as_h3(title: str, body: str) -> str:
    body = _trim_body(body)
    if body:
        return f"### {title}\n{body}"
    return f"### {title}"


def _classify_extra_h2(title: str) -> str:
    for prefix in CONTEXT_H2_PREFIXES:
        if _matches_h2_prefix(title, prefix):
            return "Context"
    return "State"


def _fallback_date(text: str) -> str:
    dates = ANY_DATE.findall(text)
    if dates:
        return min(dates)
    return datetime.now(timezone.utc).strftime("%Y-%m-%d")


def _stamp_line(line: str, fallback: str) -> str:
    m = DUP_STAMP_ENTRY.match(line)
    if m:
        a, b, rest = m.group(1), m.group(2), m.group(3).strip()
        # `- [date] [date extra]` from double-stamped jobi bullets.
        if b == a or a.startswith(b) or b.startswith(a.split()[0]):
            return f"- [{a}] {rest or 'noted'}"
    if VALID_MEMORY_ENTRY.match(line):
        return line
    m = BRACKET_STAMP_ENTRY.match(line)
    if m:
        date, time, extra, rest = m.group(1), m.group(2), m.group(3).strip(), m.group(4)
        at = f"{date} {time}" if time else date
        text = f"{extra} {rest}".strip() if extra else rest.strip()
        return f"- [{at}] {text or extra or 'noted'}"
    if line.startswith("- "):
        text = line[2:].strip() or "noted"
        dates = ANY_DATE.findall(text)
        date = dates[0] if dates else fallback
        return f"- [{date}] {text}"
    return line


def _normalize_entries(body: str, fallback: str) -> str:
    out: list[str] = []
    open_entry = False
    for raw in body.split("\n"):
        line = raw.rstrip("\n")
        stripped = line.strip()
        if stripped == "" or stripped.startswith("<!--"):
            continue
        if line.startswith("- "):
            stamped = _stamp_line(line, fallback)
            if not VALID_MEMORY_ENTRY.match(stamped):
                text = stripped.lstrip("- ").strip() or "noted"
                stamped = f"- [{fallback}] {text}"
            out.append(stamped)
            open_entry = True
        elif line.startswith("  ") and open_entry:
            out.append("  " + line.strip())
        elif open_entry:
            out.append("  " + stripped)
        else:
            text = stripped.lstrip("- ").strip() or "noted"
            dates = ANY_DATE.findall(text)
            date = dates[0] if dates else fallback
            out.append(f"- [{date}] {text}")
            open_entry = True
    return "\n".join(out)


def _entries_already_ok(body: str) -> bool:
    open_entry = False
    for raw in body.split("\n"):
        stripped = raw.strip()
        if stripped == "" or stripped.startswith("<!--"):
            continue
        if VALID_MEMORY_ENTRY.match(raw):
            open_entry = True
            continue
        if raw.startswith("  ") and open_entry:
            continue
        return False
    return True


def _has_v1_marker(text: str) -> bool:
    first = text.split("\n", 1)[0] if text else ""
    return first.startswith("<!-- trebi memory v1")


def render_memory_v1(context: str, state: str, lessons: str, log: str) -> str:
    parts = [MEMORY_MARKER, "# Memory", ""]
    for name, body in (
        ("Context", context),
        ("State", state),
        ("Lessons", lessons),
        ("Log", log),
    ):
        body = _trim_body(body)
        if body:
            parts.append(f"## {name}\n{body}\n")
        else:
            parts.append(f"## {name}\n{MEMORY_SECTION_COMMENTS[name]}\n")
    text = "\n".join(parts)
    if not text.endswith("\n"):
        text += "\n"
    return text


def convert_b22_ledger(raw: str) -> str:
    """Preamble → Context, `[YYYY-MM-DD] text` lines → Log. Mirrors memory.Migrate."""
    lines = raw.split("\n")
    if lines and lines[-1] == "":
        lines = lines[:-1]
    preamble: list[str] = []
    log_blocks: list[list[str]] = []
    cur: list[str] | None = None
    for line in lines:
        m = LEGACY_B22_ENTRY.match(line)
        if m:
            if cur is not None:
                log_blocks.append(cur)
            cur = [m.group(1), m.group(2)]
            continue
        if cur is not None:
            cur.append(line.lstrip(" "))
            continue
        preamble.append(line)
    if cur is not None:
        log_blocks.append(cur)
    while preamble and preamble[0].startswith("# "):
        preamble = preamble[1:]
    ctx = _trim_body("\n".join(preamble))
    log_lines: list[str] = []
    for block in log_blocks:
        at = block[0]
        text = (block[1] if len(block) > 1 else "").strip() or "noted"
        log_lines.append(f"- [{at}] {text}")
        for cont in block[2:]:
            if cont.strip():
                log_lines.append("  " + cont.strip())
    return render_memory_v1(ctx, "", "", "\n".join(log_lines))


def convert_jobi_memory(text: str) -> tuple[str, list[str], bool]:
    """Return (v1 text, notes, changed). Idempotent on a valid v1 file."""
    raw = text.replace("\r\n", "\n").replace("\r", "\n")
    if not raw.strip():
        return memory_skeleton(), ["empty file → v1 skeleton"], True

    preamble, sections = split_memory_h2(raw)
    if not sections:
        out = convert_b22_ledger(raw)
        return out, ["B22 ledger → v1 (preamble → Context, dated lines → Log)"], out != raw

    extra = [t for t, _ in sections if _canonical_h2(t) is None]
    canon_order = [_canonical_h2(t) for t, _ in sections]
    present = {c for c in canon_order if c}
    lessons_ok = all(
        _entries_already_ok(b) for t, b in sections if _canonical_h2(t) == "Lessons"
    )
    log_ok = all(_entries_already_ok(b) for t, b in sections if _canonical_h2(t) == "Log")
    stray_preamble = _stray_preamble(preamble)
    already_v1 = (
        not extra
        and _has_v1_marker(raw)
        and present == {"Context", "State", "Lessons", "Log"}
        and [c for c in canon_order if c]
        == ["Context", "State", "Lessons", "Log"]
        and lessons_ok
        and log_ok
        and not stray_preamble
    )
    if already_v1:
        out = raw if raw.endswith("\n") else raw + "\n"
        return out, [], out != raw

    notes: list[str] = []
    fallback = _fallback_date(raw)
    context_parts: list[str] = []
    state_parts: list[str] = []
    lessons = ""
    log = ""
    pre = _drop_h1(preamble)
    if pre:
        context_parts.append(pre)
        notes.append("preamble → Context")

    demoted: list[str] = []
    for title, body in sections:
        kind = _canonical_h2(title)
        if kind == "Context":
            context_parts.append(_trim_body(body))
        elif kind == "State":
            state_parts.append(_trim_body(body))
        elif kind == "Lessons":
            lessons = _normalize_entries(body, fallback)
            if not lessons_ok:
                notes.append("stamped Lessons bullets")
        elif kind == "Log":
            log = _normalize_entries(body, fallback)
            if not log_ok:
                notes.append("stamped Log bullets")
        else:
            dest = _classify_extra_h2(title)
            block = _as_h3(title, body)
            demoted.append(title)
            if dest == "Context":
                context_parts.append(block)
            else:
                state_parts.append(block)

    if demoted:
        shown = ", ".join(f"## {t}" for t in demoted[:8])
        more = f" (+{len(demoted) - 8} more)" if len(demoted) > 8 else ""
        notes.append(f"demoted extra {shown}{more} → ### under Context/State")
    missing = [n for n in ("Context", "State", "Lessons", "Log") if n not in present]
    if missing and not extra:
        notes.append("added missing " + ", ".join(f"## {n}" for n in missing))
    if not _has_v1_marker(raw):
        notes.append("inserted v1 marker")
    if not notes:
        notes.append("jobi MEMORY.md → v1")

    out = render_memory_v1(
        _join_parts(context_parts),
        _join_parts(state_parts),
        lessons,
        log,
    )
    return out, notes, out != raw


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


def agent_dirs_in_workspace(ws_path: Path) -> list[Path]:
    """Every directory holding an AGENT.md, wherever it sits (recursive).

    A single agents/<name>/ level is the only layout trebi accepts; callers
    report the rest. .git/node_modules/.trebi and nested workspaces are skipped
    the same way discovery skips them.
    """
    if not ws_path.is_dir():
        return []
    out: list[Path] = []
    for root, dirs, files in os.walk(ws_path):
        dirs[:] = [d for d in dirs if d not in {".git", "node_modules", ".trebi"}]
        if "AGENT.md" in files:
            out.append(Path(root))
    return sorted(out)


def agent_support_dirs(ws_path: Path) -> list[Path]:
    """agent_dirs_in_workspace, but the canonical agents/<name>/ ones only."""
    out: list[Path] = []
    for d in agent_dirs_in_workspace(ws_path):
        try:
            rel = d.relative_to(ws_path)
        except ValueError:
            continue
        parts = rel.parts
        if len(parts) == 2 and parts[0] == "agents" and AGENT_NAME_RE.match(parts[1]):
            out.append(d)
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


def _path_has_segment(path: Path, spec: str) -> bool:
    """True when spec is a whole path segment (or the file name). A substring
    rule would match --job receipts against a my-receipts folder."""
    for part in path.parts:
        if part == spec:
            return True
    return path.name == spec


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
    if _path_has_segment(path, spec):
        return True
    if source_ws:
        try:
            rel = path.resolve().relative_to(source_ws.resolve())
            if rel.as_posix() == spec or _path_has_segment(rel, spec):
                return True
        except ValueError:
            pass
    return False


def agent_spec_matches(path: Path, spec: str, source_ws: Path | None) -> bool:
    """--agent selection: the file must live under agents/<name>/.

    A bare spec (digest) matches any agent job folder of that name, but never
    an unowned job in the same workspace (the substring rule in
    job_spec_matches would have pulled agents/other and the root tree in).
    """
    spec = spec.strip().strip("/")
    if not spec:
        return True
    parts: list[str]
    if source_ws:
        try:
            parts = path.resolve().relative_to(source_ws.resolve()).parts
        except ValueError:
            parts = path.parts
    else:
        parts = path.parts
    if len(parts) < 2 or parts[0] != "agents":
        return False
    if spec.lower() in (parts[1].lower(), parts[-2].lower(), job_file_name(path).lower()):
        return True
    rel = Path(*parts).as_posix()
    return spec == rel or rel.endswith("/" + spec)


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


def resolve_target_workspace(
    trebi_dir: Path, spec: str | None, jobi_dir: Path | None = None
) -> TargetWorkspace | None:
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
    # A jobi workspace name is a valid target when its folder exists (the
    # cutover target is usually the same tree the jobi registry points at).
    for w in load_workspaces_yaml(jobi_dir or default_jobi_dir()):
        if spec not in (str(w.get("name") or ""), str(w.get("path") or "")):
            continue
        wp = Path(str(w.get("path") or "")).expanduser()
        if wp.is_dir():
            return TargetWorkspace(
                id=mint_ws_id(str(wp.resolve())),
                name=str(w.get("name") or wp.name),
                path=wp.resolve(),
                source="jobi",
            )
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
    agent: str | None = None
    agent_scope: str | None = None
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
        if self.agent_scope:
            lines.append(f"agent filter: {self.agent_scope} (agent + its jobs)")
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
    conn: sqlite3.Connection, job_filter: str | None = None, agent_scope: str | None = None
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
        if agent_scope and not _row_matches_agent(d, agent_scope):
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


def _job_file_agent(job_file: Any) -> str:
    """The agents/<name> segment of a jobi job_file path, else ""."""
    parts = [p for p in Path(str(job_file or "")).parts if p]
    for i in range(len(parts) - 1):
        if parts[i] == "agents":
            return parts[i + 1].strip().lower()
    return ""


def _row_matches_agent(d: dict[str, Any], agent: str) -> bool:
    """True when a jobi row belongs to <agent> or to one of its jobs.

    jobi stamps a child job with parent_agent, and the agent's own wake with
    kind='agent' and job_name=<agent>. job_file's agents/<name> segment is the
    fallback for rows written before parent_agent existed. workspace_path is
    deliberately not a signal: sibling jobs in the same workspace (agents/*)
    share it, and matching them would rewrite another agent's rows.
    """
    a = agent.strip().lower()
    if str(d.get("parent_agent") or "").strip().lower() == a:
        return True
    name = str(d.get("job_name") or "").strip().lower()
    if name == a:
        return True
    if _job_file_agent(d.get("job_file")) == a:
        return True
    return False


def _task_matches_job(d: dict[str, Any], spec: str) -> bool:
    spec = spec.strip()
    name = str(d.get("job_name") or "")
    if name == spec or name.lower() == spec.lower():
        return True
    jf = str(d.get("job_file") or "")
    if spec in jf or jf.endswith("/" + spec + "/JOB.md"):
        return True
    return Path(jf).parent.name == spec if jf else False


def _agent_layout_issues(ws_path: Path) -> list[str]:
    """trebi layout errors an agent tree would carry (job/discover.go validateAgent).

    These are file-level checks over the jobi workspace, not rows: a migration
    that ignores them lands trees the daemon will not index.
    """
    issues: list[str] = []
    for agent_dir in agent_dirs_in_workspace(ws_path):
        rel = agent_dir.relative_to(ws_path).as_posix()
        where = f"{ws_path}/{rel}" if rel != "." else str(ws_path)
        parts = rel.split("/")
        if len(parts) >= 3:
            issues.append(
                f"nested agent root: put AGENT.md directly in agents/<name>/ ({where})"
            )
        elif not (len(parts) == 2 and AGENT_NAME_RE.match(parts[1])):
            issues.append(
                f"misplaced AGENT.md: expected <workspace>/agents/<name>/AGENT.md ({where})"
            )
        if (agent_dir / "SOUL.md").exists():
            issues.append(f"SOUL.md beside AGENT.md is not accepted; merge manually ({where})")
        if (agent_dir / "JOB.md").exists():
            issues.append(
                f"JOB.md next to AGENT.md is not indexed: move it into a subfolder ({where})"
            )
        issues.extend(_copied_agent_dir_issues(agent_dir, where))
    return issues


def _copied_agent_dir_issues(agent_dir: Path, where: str) -> list[str]:
    """An agent dir holding trebi.yaml is a nested workspace root, which
    discovery skips silently."""
    if (agent_dir / "trebi.yaml").exists():
        return [f"nested workspace root: remove trebi.yaml to keep {where} inside the parent"]
    return []


def agent_layout_issues(workspaces: list[Path]) -> list[str]:
    out: list[str] = []
    for ws in workspaces:
        if not ws.is_dir():
            continue
        for issue in _agent_layout_issues(ws):
            if issue not in out:
                out.append(issue)
    return out


def plan_migrate(
    jobi_dir: Path,
    trebi_dir: Path,
    mode: str,
    *,
    job: str | None = None,
    target: TargetWorkspace | None = None,
    agent: str | None = None,
    agent_scope: str | None = None,
) -> MigrationPlan:
    plan = MigrationPlan(
        jobi_dir=jobi_dir, trebi_dir=trebi_dir, mode=mode, job=job, target=target,
        agent=agent, agent_scope=agent_scope,
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
    layout = agent_layout_issues(workspace_paths_from_jobi(jobi_dir))
    if layout:
        pf.counts["agent_layout_issues"] = len(layout)
        for issue in layout[:12]:
            pf.notes.append(f"layout: {issue}")
        plan.warnings.append(
            f"{len(layout)} agent layout issue(s); trebi will not index those files "
            "(fix before final cutover)"
        )
    if job:
        pf.notes.append(f"job filter: {job} — skip harness/bin/logs copy")
    if agent:
        pf.notes.append(f"agent filter: {agent} — rows and files land under agents/{agent}/")
    if mode == "final":
        pf.notes.append("final mode requires jobi daemon stopped (not checked in dry-run)")
    else:
        pf.notes.append(
            "shadow mode: DB rows point at <trebi>/shadow-workspaces/<name>; "
            "copy jobs there with `rewrite-jobs --mirror` then cut over (--final)"
        )
        plan.warnings.append(
            "shadow registers empty shadow workspaces; job files stay in place until "
            "rewrite-jobs --mirror or a --final cutover"
        )
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
        # agents/<name>/ dirs are NOT registered: the parent walk discovers
        # them (job.Discover) and a separate workspace row scanning the same
        # subtree would index every agent job twice. They keep the parent id.
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
                    proj = project_task_derivations(conn, job, agent_scope)
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


def agent_canonical_job(name: str) -> str:
    """Normalize a jobi agent job name to trebi's canonical shape.

    jobi nests agent jobs as agents/<a>/jobs/<area>/<job>; trebi derives the
    job name from the folder (job/discover.go) and the agent tree drops the
    jobs/ level, so the same name must be agents/<a>/<job>. Deeper grouping
    dirs collapse to the job folder, matching agent_rel_dest.
    """
    s = name.strip().strip("/")
    if not s.startswith("agents/"):
        return s
    parts = s.split("/")
    # drop every "jobs" segment: agents/<a>/jobs/<x>/<job> → agents/<a>/<job>
    kept = [p for p in parts if p != "jobs"]
    # ownership is agents/<a>/; keep only the job folder past it
    if len(kept) > 3:
        kept = kept[:2] + [kept[-1]]
    return "/".join(kept)


def run_job_name(d: dict[str, Any], agent: str | None) -> str:
    """runs.job for a migrated task.

    Job identity in trebi is the workspace-relative folder, so an agent-owned
    job is agents/<name>/<job> (A.6/C.1) and jobi's jobs/ nesting is dropped.
    A jobi job_name already shaped agents/<a>/... is normalized the same way.
    An agent's own wake keeps the agent name: an agent does not own itself.

    job_name is authoritative. job_file is history and can hold a path from an
    older layout (workflows/<name>.md, or a since-corrected spelling), so it is
    only a fallback when job_name is empty.
    """
    name = str(d.get("job_name") or "")
    if not agent:
        return agent_canonical_job(name)
    if (d.get("kind") or "") == "agent":
        return f"agents/{agent}"
    if name.startswith("agents/"):
        canon = agent_canonical_job(name)
        return canon if canon.startswith(f"agents/{agent}/") else f"agents/{agent}/{canon}"
    folder = Path(name).name if name else Path(str(d.get("job_file") or "")).parent.name
    return f"agents/{agent}/{folder}" if folder else name


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
    agent: str | None = None,
    agent_scope: str | None = None,
) -> tuple[int, set[str]]:
    cols = [r[1] for r in jconn.execute("PRAGMA table_info(tasks)")]
    ids: set[str] = set()
    n = 0
    for row in jconn.execute("SELECT * FROM tasks"):
        d = dict(zip(cols, row))
        if job_filter and not _task_matches_job(d, job_filter):
            continue
        if agent_scope and not _row_matches_agent(d, agent_scope):
            continue
        if is_junk_task(d):
            continue
        path_for_ws = d.get("job_file") or d.get("workspace_path") or ""
        wid = workspace_id_for(path_for_ws, wmap, target_id)
        # jobi 'agent' is first-class since v12; it no longer collapses into
        # the workspace assistant.
        kind = d.get("kind") or "job"
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
                run_job_name(d, agent),
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
            # Schema v11 renamed the segment layers so "agent" could become the
            # product noun. Rows imported raw would bypass that migration.
            vals = list(row)
            vals[1] = LAYER_MAP.get(vals[1], vals[1])
            tconn.execute(
                "INSERT INTO run_segments (run_id, layer, state, started_at, ended_at, duration_ms, reason, source, detail_json) VALUES (?,?,?,?,?,?,?,?,?)",
                vals,
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


def queue_kind(d: dict, task_kind: dict[str, str]) -> str:
    """Run kind for a jobi event_queue row.

    Prefer the launched task's kind. jobi marked an agent wake with an empty
    parent_agent and a bare (slash-free) job_name; a job always has a parent
    or a path-shaped name.
    """
    k = task_kind.get(str(d.get("task_id") or ""))
    if k in ("agent", "job"):
        return k
    name = str(d.get("job_name") or "")
    parent = str(d.get("parent_agent") or "")
    if name and not parent and "/" not in name:
        return "agent"
    return "job"


def derive_trace_id(prefix: str) -> str:
    """Deterministic trace for rows with no jobi source (queue rows carry no
    trace; schema v1 event_queue.trace_id came later). Mirrors runs.trace_id."""
    return hashlib.sha256((prefix + "trace").encode()).hexdigest()[:32]


def apply_queue(
    jconn: sqlite3.Connection,
    tconn: sqlite3.Connection,
    wmap: dict[str, str],
    *,
    job_filter: str | None = None,
    target_id: str | None = None,
    agent: str | None = None,
    agent_scope: str | None = None,
) -> int:
    try:
        cols = [r[1] for r in jconn.execute("PRAGMA table_info(event_queue)")]
    except sqlite3.Error:
        return 0
    # jobi's event_queue has no kind column. tasks.kind is authoritative, so
    # resolve it through task_id instead of guessing from the name shape.
    task_kind: dict[str, str] = {}
    try:
        for tid, tkind in jconn.execute(
            "SELECT id, kind FROM tasks WHERE id IS NOT NULL AND kind IS NOT NULL"
        ):
            if tid:
                task_kind[str(tid)] = str(tkind)
    except sqlite3.Error:
        pass
    # events.<id> backs event_queue.event_id. Reuse the row the connector
    # already persisted for this (connector, raw) pair; missing stays NULL.
    event_by_raw: dict[tuple[str, str], str] = {}
    try:
        for eid, conn_name, raw in jconn.execute(
            "SELECT id, source, event_raw FROM events WHERE event_raw IS NOT NULL"
        ):
            event_by_raw.setdefault((str(conn_name), str(raw)), str(eid))
    except sqlite3.Error:
        pass
    n = 0
    for row in jconn.execute("SELECT * FROM event_queue"):
        d = dict(zip(cols, row))
        if job_filter:
            jn = str(d.get("job_name") or "")
            if jn != job_filter and job_filter.lower() != jn.lower():
                continue
        if agent_scope and not _row_matches_agent(d, agent_scope):
            continue
        wid = workspace_id_for(d.get("workspace_path") or "", wmap, target_id)
        kind = queue_kind(d, task_kind)
        event_id = None
        raw = d.get("event_raw")
        if raw is not None:
            event_id = event_by_raw.get((str(d.get("source") or ""), str(raw)))
        tconn.execute(
            """INSERT OR REPLACE INTO event_queue (
              id, connector, workspace_id, job, kind, on_index, conversation_key, event_id,
              event_raw, capture_json, vars_json, prompt, status, block_reason, result,
              attempts, next_attempt_at, claimed_at, run_id, error_message, trace_id,
              created_at, updated_at, done_at
            ) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)""",
            (
                d["id"],
                d.get("source"),
                wid,
                run_job_name(d, agent),
                kind,
                d.get("on_index") or 0,
                d.get("conversation_key") or "",
                event_id,
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
                derive_trace_id(str(d["id"])),
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


def schedule_spec(interval: str) -> str:
    """jobi interval -> trebi schedules.spec.

    RecomputeFromJob writes "@every <d>" for every: triggers and keeps
    last_fired_at only when the stored spec matches. A bare "6h" therefore
    resets the anchor we just migrated.
    """
    s = (interval or "").strip()
    if not s:
        return ""
    if s.startswith("@"):
        return s
    if " " in s:  # cron is passed through unchanged
        return s
    return "@every " + s


def apply_schedules(
    jconn: sqlite3.Connection,
    tconn: sqlite3.Connection,
    wmap: dict[str, str],
    *,
    job_filter: str | None = None,
    target_id: str | None = None,
    agent: str | None = None,
    agent_scope: str | None = None,
) -> int:
    try:
        rows = list(jconn.execute("SELECT key, anchor_time, interval FROM schedule_anchors"))
    except sqlite3.Error:
        return 0
    job_ws: dict[str, str] = {}
    # schedule_anchors keys are bare job names, so resolve agent ownership
    # through the tasks table. Keys carry jobi's layout
    # (agents/<a>/jobs/<job>), so match on the canon short name too.
    agent_jobs: set[str] = set()
    try:
        for jn, wp, jf, pa, kind in jconn.execute(
            "SELECT job_name, workspace_path, job_file, parent_agent, kind FROM tasks "
            "WHERE job_name IS NOT NULL"
        ):
            if jn and jn not in job_ws:
                canon = agent_canonical_job(str(jn))
                wid = workspace_id_for(jf or wp or "", wmap, target_id)
                job_ws[str(jn)] = wid
                job_ws.setdefault(canon, wid)
            if agent_scope and jn and _row_matches_agent(
                {
                    "job_name": jn,
                    "workspace_path": wp,
                    "job_file": jf,
                    "parent_agent": pa,
                    "kind": kind,
                },
                agent_scope,
            ):
                agent_jobs.add(str(jn).lower())
                agent_jobs.add(Path(str(jn)).name.lower())
                agent_jobs.add(Path(agent_canonical_job(str(jn))).name.lower())
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
        canon = agent_canonical_job(job)
        if (
            job_filter
            and job.lower() != job_filter.lower()
            and canon.lower() != job_filter.lower()
            and Path(canon).name.lower() != job_filter.lower()
        ):
            continue
        if agent_scope and job.lower() not in agent_jobs and canon.lower() not in agent_jobs:
            continue
        wid = target_id or job_ws.get(job) or job_ws.get(canon) or "imported-orphans"
        job = canon
        if agent and not job.startswith(f"agents/{agent}/") and job != f"agents/{agent}":
            job = f"agents/{agent}/{job}"
        tconn.execute(
            """INSERT OR REPLACE INTO schedules (
              workspace_id, job, idx, spec, timezone, catch_up, next_due_at, last_fired_at, anchor, updated_at
            ) VALUES (?,?,?,?,?,?,?,?,?,?)""",
            (wid, job, idx, schedule_spec(interval), "UTC", 0, None, anchor_time, anchor_time, now),
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

    if not job_filter and not plan.agent_scope:
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
            jconn,
            tconn,
            plan.workspace_map,
            job_filter=job_filter,
            target_id=target_id,
            agent=plan.agent,
            agent_scope=plan.agent_scope,
        )
        print(f"inserted runs: {n}")
        apply_timeline(jconn, tconn, ids if (job_filter or plan.agent_scope) else None)
        print("timeline segments+events copied")
        n = apply_queue(
            jconn,
            tconn,
            plan.workspace_map,
            job_filter=job_filter,
            target_id=target_id,
            agent=plan.agent,
            agent_scope=plan.agent_scope,
        )
        print(f"queue rows: {n}")
        if not job_filter and not plan.agent_scope:
            apply_policy_cursors(jconn, tconn)
            print("policy + cursors + metrics copied")
        n = apply_schedules(
            jconn,
            tconn,
            plan.workspace_map,
            job_filter=job_filter,
            target_id=target_id,
            agent=plan.agent,
            agent_scope=plan.agent_scope,
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
                if mode == "final":
                    dest = path
                else:
                    # A shadow workspace is a real directory with its own
                    # trebi.yaml: the daemon indexes and schedules against it,
                    # so a bare DB row pointing at a missing path would be
                    # pruned as a nested scan or indexed as empty.
                    shadow = trebi / "shadow-workspaces" / name
                    shadow.mkdir(parents=True, exist_ok=True)
                    ty = shadow / "trebi.yaml"
                    if not ty.exists():
                        ty.write_text(f"id: {wid}\nname: {name}\ngit_enabled: false\n")
                        print(f"minted {ty}")
                    dest = str(shadow)
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


# Directories never worth copying into a fresh workspace.
SKIP_DIRS = {".git", "node_modules", ".trebi", "__pycache__", ".venv", ".pytest_cache", ".mypy_cache"}
# Files that are build noise or local editor state.
SKIP_FILES = {".DS_Store"}
# Job manifests are rewritten separately; everything else beside them is support.
JOB_MANIFESTS = {"JOB.md", "AGENT.md", "SOUL.md", "TREBI.md"}


def agent_support_files(agent_dir: Path) -> list[Path]:
    """Root-level files beside AGENT.md (its MEMORY.md), excluding job folders.

    Only the top level: everything deeper belongs to a job and is carried by
    that job's own entry.
    """
    out: list[Path] = []
    if not agent_dir.is_dir():
        return out
    for f in sorted(agent_dir.iterdir()):
        if not f.is_file():
            continue
        if f.name in JOB_MANIFESTS or f.name in SKIP_FILES:
            continue
        out.append(f)
    return out


def job_support_files(job_dir: Path) -> list[Path]:
    """Every file in a job folder except its manifest, junk, and caches.

    A job in trebi is its folder (internal/job/discover.go names the job from
    the directory), so MEMORY.md, scripts/ and templates/ belong to it.
    MEMORY.md is converted jobi → v1 (headings + entry stamps only); other
    support files stay byte-for-byte so addresses like jobi@ocral.org are
    not substituted.
    """
    out: list[Path] = []
    for root, dirs, files in os.walk(job_dir):
        dirs[:] = [d for d in dirs if d not in SKIP_DIRS]
        for f in sorted(files):
            if f in JOB_MANIFESTS or f in SKIP_FILES:
                continue
            out.append(Path(root) / f)
    return out


AGENT_NAME_RE = re.compile(r"^[a-z0-9][a-z0-9-]*$")


def _dest_is_agent_dir(dest: Path, target: TargetWorkspace | None) -> bool:
    """True when dest already sits under <target>/agents/<name>/ (canonical
    agent layout), so an operator file keeps its AGENT.md name there."""
    if not target:
        return False
    try:
        rel = dest.resolve().relative_to(target.path.resolve())
    except ValueError:
        return False
    return len(rel.parts) >= 2 and rel.parts[0] == "agents" and bool(rel.parts[1])


def operator_dest(target: TargetWorkspace | None, fallback: Path, agent: str | None) -> Path:
    """Where the jobi AGENT.md lands.

    Agent mode keeps it as AGENT.md at agents/<name>/ (the shipped layout;
    a nested operator). Without --agent it becomes the workspace assistant's
    TREBI.md at the root, which was the pre-agents behaviour — unless the
    computed destination is already agents/<name>/, which a plain copy of an
    agent tree produces.
    """
    if agent and target:
        return target.path / "agents" / agent / "AGENT.md"
    if _dest_is_agent_dir(fallback, target):
        return fallback.with_name("AGENT.md")
    return fallback.with_name("TREBI.md")


def agent_rel_dest(rel: Path, agent: str) -> Path:
    """Map a jobi-relative job path onto the canonical agent layout.

    trebi has no jobs/ level: a job owned by an agent lives at
    agents/<name>/<job>/JOB.md, and ownership is derived from that path
    (internal/job/discover.go). jobi nested them as jobs/<area>/<job>/, so the
    intermediate folders are flattened to the job's own folder name. rel may
    already start with agents/<name>/ (a plain copy of an agent tree); that
    prefix is stripped before re-adding it, never doubled.
    """
    parts = list(rel.parts)
    if len(parts) >= 2 and parts[0] == "agents":
        parts = parts[2:]  # drop agents/<name>/; agent owns the result
    if parts and parts[0] == "jobs":
        parts = parts[1:]
    if not parts:
        return Path("agents") / agent
    # Keep only the job folder + filename; drop grouping dirs like finance/.
    if len(parts) > 2:
        parts = parts[-2:]
    return Path("agents") / agent / Path(*parts)


def dest_for_job(
    path: Path,
    source_ws: Path | None,
    target: TargetWorkspace | None,
    incremental: bool,
    agent: str | None = None,
) -> Path:
    if incremental and target:
        if source_ws:
            try:
                rel = path.resolve().relative_to(source_ws.resolve())
                if agent:
                    return target.path / agent_rel_dest(rel, agent)
                # Agent trees keep trebi's canonical layout on a plain
                # (unfiltered) copy too: the run rows say agents/<a>/<job>,
                # so the files must land that way (jobi's jobs/ level is
                # dropped). Nested workspaces are not copied by this mode.
                if rel.parts and rel.parts[0] == "agents" and len(rel.parts) > 1:
                    owner = rel.parts[1] if AGENT_NAME_RE.match(rel.parts[1]) else agent
                    if owner:
                        return target.path / agent_rel_dest(rel, owner)
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


def _dest_has_same_text(dest: Path, text: str) -> bool:
    try:
        return dest.is_file() and dest.read_text(encoding="utf-8", errors="replace") == text
    except OSError:
        return False


def _dest_has_same_bytes(dest: Path, src: Path) -> bool:
    try:
        return dest.is_file() and dest.read_bytes() == src.read_bytes()
    except OSError:
        return False


def plan_support_file(
    src: Path,
    dest: Path,
    ws: Path,
    *,
    convert_memory: bool,
    label: str,
) -> tuple[dict[str, Any], list[str]]:
    """Plan one support-file copy, converting MEMORY.md to v1 when asked."""
    lines: list[str] = []
    try:
        rel: Path | str = src.relative_to(ws)
    except ValueError:
        rel = src.name
    if convert_memory and src.name == "MEMORY.md":
        text = src.read_text(encoding="utf-8", errors="replace")
        new_text, notes, changed = convert_jobi_memory(text)
        payload = new_text if changed else text
        if not changed and dest == src:
            return (
                {
                    "path": str(src),
                    "dest": str(dest),
                    "action": "unchanged",
                    "changed": False,
                    "source_ws": str(ws),
                },
                [],
            )
        if _dest_has_same_text(dest, payload):
            action = "unchanged"
            changed = False
            notes = []
        else:
            action = "convert-memory" if changed else "copy-support"
        detail = {
            "path": str(src),
            "dest": str(dest),
            "action": action,
            "changed": changed,
            "source_ws": str(ws),
        }
        mark = "*" if changed else "+"
        lines.append(f"  {mark} {rel} ({action} → {dest})")
        for n in notes:
            lines.append(f"    ! {n}")
        return detail, lines
    action = "unchanged" if _dest_has_same_bytes(dest, src) else "copy-support"
    detail = {
        "path": str(src),
        "dest": str(dest),
        "action": action,
        "changed": False,
        "source_ws": str(ws),
    }
    lines.append(f"  + {rel} ({action if action == 'unchanged' else label} → {dest})")
    return detail, lines


def merged_operator_text(
    agent_path: Path,
    *,
    rewrite_sentinel: bool,
    rewrite_memory_headings: bool,
    agent_name: str | None,
) -> tuple[str, list[str]]:
    """The operator file for a jobi AGENT.md: frontmatter normalized, the
    sibling SOUL.md body merged in as ## Soul, body as ## Instructions.
    Shared by the dry-run plan and the apply path so they cannot drift."""
    report: list[str] = []
    agent_text = agent_path.read_text(encoding="utf-8", errors="replace")
    new_agent, _ = rewrite_job_text(
        agent_text, report,
        rewrite_sentinel=rewrite_sentinel,
        rewrite_memory_headings=rewrite_memory_headings,
        src_dir=agent_path.parent,
    )
    soul = agent_path.parent / "SOUL.md"
    soul_body = ""
    if soul.exists():
        soul_body = soul.read_text(encoding="utf-8", errors="replace")
        _, _, soul_body = split_frontmatter(soul_body)
    fm, _, body = split_frontmatter(new_agent)
    data = load_fm_map(fm or "")
    data = rewrite_frontmatter_map(data, report)
    data.setdefault("memory", True)
    if agent_name:
        # C.2: frontmatter name: must equal the folder basename, so a stale
        # jobi name is a validation error. The folder is identity.
        data.pop("name", None)
    merged_body = f"## Soul\n{soul_body.strip()}\n\n## Instructions\n{body.strip()}\n"
    return dump_frontmatter(data, merged_body), report


def plan_rewrite(
    workspaces: list[Path],
    *,
    job: str | None,
    target: TargetWorkspace | None,
    incremental: bool,
    rewrite_sentinel: bool,
    agent: str | None = None,
    convert_memory: bool = True,
    rewrite_memory_headings: bool = True,
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
            if path.name == "JOB.md" and (path.parent / "AGENT.md").exists():
                # trebi discovery refuses a JOB.md sibling of AGENT.md
                # (discover.go job()); rewriting it would produce a file the
                # daemon still will not index. The preflight reports it.
                lines.append(
                    f"SKIP {path.relative_to(ws)} (JOB.md beside AGENT.md is not indexed)"
                )
                continue
            if path.name == "SOUL.md" and (path.parent / "AGENT.md").exists():
                continue  # merged when the AGENT.md entry is planned
            if agent:
                # --agent is the ownership filter: only agents/<name>/ files in.
                if not agent_spec_matches(path, agent, ws):
                    continue
                if path.name == "SOUL.md" and not agent_spec_matches(path.parent, agent, ws):
                    continue
            elif job and not job_spec_matches(path, job, ws):
                continue
            if path.name == "SOUL.md" and job and job_file_name(path) != job:
                continue
            if path.name == "AGENT.md" and job and job_file_name(path) != job:
                continue
            matched += 1
            if not header_written:
                lines.append(f"workspace {ws}:")
                header_written = True
            report: list[str] = []
            text = path.read_text(encoding="utf-8", errors="replace")
            dest = dest_for_job(path, ws, target, incremental, agent)
            if path.name == "AGENT.md":
                op_dest = operator_dest(target, dest, agent)
                merged, merge_report = merged_operator_text(
                    path,
                    rewrite_sentinel=rewrite_sentinel,
                    rewrite_memory_headings=rewrite_memory_headings,
                    agent_name=agent,
                )
                report.extend(merge_report)
                same = _dest_has_same_text(op_dest, merged)
                details.append(
                    {
                        "path": str(path),
                        "dest": str(op_dest),
                        "action": "merge-unchanged" if same else "merge",
                        "changed": not same,
                        "source_ws": str(ws),
                        "new_text": merged if not same else None,
                    }
                )
                lines.append(
                    f"  {' ' if same else '*'} MERGE {path.relative_to(ws)} → {op_dest}"
                )
                # The agent's own MEMORY.md sits beside AGENT.md at the root.
                agent_dest_dir = op_dest.parent
                for sup in agent_support_files(path.parent):
                    sup_dest = agent_dest_dir / sup.name
                    detail, extra = plan_support_file(
                        sup, sup_dest, ws,
                        convert_memory=convert_memory,
                        label="agent support",
                    )
                    details.append(detail)
                    lines.extend(extra)
                for r in report:
                    lines.append(f"    ! {r}")
                continue
            if path.name == "SOUL.md":
                # SOUL.md without a sibling AGENT.md: nothing to merge into
                # here; the rewrite path cannot invent an operator.
                lines.append(f"  ! SOUL.md without AGENT.md, left in place ({path})")
                continue
            new_text, changed = rewrite_job_text(
                text, report,
                rewrite_sentinel=rewrite_sentinel,
                rewrite_memory_headings=rewrite_memory_headings,
                src_dir=path.parent,
            )
            action = "rewrite" if changed else "unchanged"
            if incremental and target:
                if dest == path or (dest.exists() and _dest_has_same_text(dest, new_text)):
                    # Content already matches the destination: a second run
                    # is a true no-op (report it, do not write).
                    action = "unchanged"
                    changed = False
                    new_text = None
                else:
                    action = "copy"
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
            # A job is its folder: carry MEMORY.md, scripts/, templates/ too.
            for sup in job_support_files(path.parent):
                sup_rel = sup.relative_to(path.parent)
                sup_dest = dest.parent / sup_rel
                detail, extra = plan_support_file(
                    sup, sup_dest, ws,
                    convert_memory=convert_memory,
                    label="support",
                )
                details.append(detail)
                lines.extend(extra)

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
        elif not agent and not job:
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
    agent = getattr(args, "agent", None)
    if agent:
        agent = agent.strip().lower()
        if not AGENT_NAME_RE.match(agent):
            print(
                f"ERROR: --agent {agent!r} must match [a-z0-9][a-z0-9-]* "
                "(internal/job/discover.go agentNameRe)",
                file=sys.stderr,
            )
            return 2
    target_spec = getattr(args, "target_workspace", None)
    if not target_spec and args.mirror:
        target_spec = str(trebi / "shadow-workspaces")
    target = resolve_target_workspace(trebi, target_spec, jobi)
    # An explicit --target-workspace always means copy-to-target. Only a full
    # run with no target is an in-place cutover.
    incremental = bool(job or args.mirror or target_spec or agent)
    if agent and not target_spec:
        print("ERROR: --agent requires --target-workspace", file=sys.stderr)
        return 2
    if incremental and args.apply and not target:
        print("ERROR: --job --apply requires --target-workspace", file=sys.stderr)
        return 2
    if getattr(args, "target_workspace", None) and target is None:
        print(f"ERROR: could not resolve --target-workspace {args.target_workspace!r}", file=sys.stderr)
        return 2
    if incremental and target:
        print(f"TARGET {target.name} ({target.id}) {target.path} [{target.source}]")
    convert_memory = not getattr(args, "no_memory", False)
    lines, details = plan_rewrite(
        workspaces,
        job=job,
        target=target,
        incremental=incremental,
        rewrite_sentinel=not args.no_sentinels,
        agent=agent,
        convert_memory=convert_memory,
        rewrite_memory_headings=convert_memory,
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
            new_text, _ = rewrite_job_text(
                text, report,
                rewrite_sentinel=not args.no_sentinels,
                rewrite_memory_headings=convert_memory,
                src_dir=path.parent,
            )
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
        mem_shown = 0
        for d in details:
            if d.get("action") != "convert-memory":
                continue
            path = Path(d["path"])
            text = path.read_text(encoding="utf-8", errors="replace")
            new_text, notes, _ = convert_jobi_memory(text)
            print(f"\n--- {path}")
            print(f"+++ {d.get('dest') or path} (memory v1)")
            for n in notes:
                print(f"    ! {n}")
            preview = new_text[:2000]
            print(preview)
            if len(new_text) > 2000:
                print("    …")
            mem_shown += 1
            if mem_shown >= 5:
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
            soul = path.parent / "SOUL.md"
            out, report = merged_operator_text(
                path,
                rewrite_sentinel=not args.no_sentinels,
                rewrite_memory_headings=convert_memory,
                agent_name=agent,
            )
            if not dest.suffix:
                dest = dest / "TREBI.md"
            if _dest_has_same_text(dest, out):
                print(f"unchanged {dest}")
                continue
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
        if d.get("action") == "convert-memory" or (
            convert_memory and path.name == "MEMORY.md"
        ):
            text = path.read_text(encoding="utf-8", errors="replace")
            new_text, _notes, changed = convert_jobi_memory(text)
            if not changed and dest == path:
                continue
            payload = new_text if changed else text
            if dest != path and _dest_has_same_text(dest, payload):
                print(f"unchanged {dest}")
                continue
            if not incremental:
                backup_file(backup, path)
            dest.parent.mkdir(parents=True, exist_ok=True)
            dest.write_text(payload, encoding="utf-8")
            print(f"{'converted' if changed else 'copied'} {dest}")
            continue
        if d.get("action") in ("copy-support", "unchanged"):
            # Byte-for-byte for scripts/templates. MEMORY.md is handled above.
            if dest == path:
                continue
            if dest.exists() and dest.read_bytes() == path.read_bytes():
                continue
            dest.parent.mkdir(parents=True, exist_ok=True)
            shutil.copy2(path, dest)
            print(f"copied {dest}")
            continue
        if path.name not in ("JOB.md", "TREBI.md"):
            continue
        text = path.read_text(encoding="utf-8", errors="replace")
        report: list[str] = []
        new_text, changed = rewrite_job_text(
            text, report,
            rewrite_sentinel=not args.no_sentinels,
            rewrite_memory_headings=convert_memory,
            src_dir=path.parent,
        )
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
        wpath = Path(w.get("path") or "")
        for nested in agent_support_dirs(wpath):
            print(f"      agent: {nested.name} ({nested})")
        for issue in _agent_layout_issues(wpath):
            print(f"      layout issue: {issue}")
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
    agent = getattr(args, "agent", None)
    if agent:
        agent = agent.strip().lower()
        if not AGENT_NAME_RE.match(agent):
            print(
                f"ERROR: --agent {agent!r} must match [a-z0-9][a-z0-9-]*",
                file=sys.stderr,
            )
            return 2
    target = resolve_target_workspace(trebi, getattr(args, "target_workspace", None), jobi)
    if agent and not target:
        print("ERROR: --agent requires --target-workspace", file=sys.stderr)
        return 2
    if job and args.apply and not target:
        print("ERROR: --job --apply requires --target-workspace", file=sys.stderr)
        return 2
    plan = plan_migrate(jobi, trebi, mode, job=job, target=target, agent=agent, agent_scope=agent)
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
    agent = getattr(args, "agent", None)
    db = trebi / "trebi.db"
    print(f"verify against {db}")
    if not db.exists():
        print("trebi.db missing — nothing to verify (ok for pre-apply)")
        return 0
    tconn = sqlite3.connect(f"file:{db}?mode=ro", uri=True)
    jconn = open_jobi_ro(jobi)
    try:
        try:
            ver = tconn.execute(
                "SELECT COALESCE(MAX(version),0) FROM schema_migrations"
            ).fetchone()[0]
            print(f"schema_version: {ver} (script expects {EXPECTED_SCHEMA})")
        except sqlite3.Error:
            print(f"schema_version: unreadable (script expects {EXPECTED_SCHEMA})")
        # Restrict the trebi side to the rows the command was scoped to: a full
        # copy has every row, a --job/--agent copy only its slice. A target
        # workspace remaps all filtered rows to itself, so the plain count is
        # the right comparison there.
        if agent:
            runs = tconn.execute(
                "SELECT COUNT(*) FROM runs WHERE job=? OR job LIKE ?",
                (f"agents/{agent}", f"agents/{agent}/%"),
            ).fetchone()[0]
        elif job:
            runs = tconn.execute("SELECT COUNT(*) FROM runs WHERE job=?", (job,)).fetchone()[0]
        else:
            runs = tconn.execute("SELECT COUNT(*) FROM runs").fetchone()[0]
        if jconn:
            proj = project_task_derivations(jconn, job, agent)
            want = proj["will_insert"]
            ok = runs == want
            print(
                f"run conservation: trebi.runs={runs} jobi.will_insert={want} "
                f"junk={proj['dropped_junk']} filtered={proj['filtered_out']} ok={ok}"
            )
        else:
            print(f"trebi.runs={runs} (no jobi.db to compare)")
        for tbl in (
            "run_segments",
            "run_events",
            "event_queue",
            "schedules",
            "dispatch_policy",
            "connector_cursors",
            "workspaces",
            "job_index",
        ):
            try:
                n = tconn.execute(f"SELECT COUNT(*) FROM {tbl}").fetchone()[0]
                print(f"  {tbl}: {n}")
            except sqlite3.Error as e:
                print(f"  {tbl}: error {e}")
        # Trebi derives schedules from parsed job files at first index; a
        # nonzero seed count with an empty job_index means the daemon has not
        # indexed the workspace yet (expected right after a --job/--agent copy).
        seeded = tconn.execute("SELECT COUNT(*) FROM schedules").fetchone()[0]
        indexed = tconn.execute("SELECT COUNT(*) FROM job_index").fetchone()[0]
        if seeded and not indexed:
            print("  note: schedules seeded but job_index empty — start trebid to index")
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
    # Agent tree: agents/scout (the --agent target) and agents/other, both
    # inside the same workspace, so scope tests can prove sibling rows stay out.
    for agent in ("scout", "other"):
        agent_dir = out / "ws" / "agents" / agent
        job_dir = agent_dir / "jobs" / "digest"
        job_dir.mkdir(parents=True, exist_ok=True)
        (agent_dir / "AGENT.md").write_text(
            "---\n"
            "command: claude\n"
            "tool: claude\n"
            "memory: true\n"
            "oversight: true\n"
            "parent_agent: root\n"
            "---\n"
            f"You are {agent}. JOBI_TASK_COMPLETE\n"
        )
        (agent_dir / "SOUL.md").write_text("---\ntitle: Soul\n---\nBe curious.\n")
        (agent_dir / "MEMORY.md").write_text(
            "# Memory\n\n"
            "Preamble fact.\n\n"
            "## Context\nDurable stuff.\n\n"
            "## Shipped\n- [2026-01-02] shipped one\n\n"
            "## State\nCurrent state.\n\n"
            "## Lessons\n- Learned a thing\n\n"
            "## Log\n- [2026-01-04] did the thing\n"
        )
        (job_dir / "JOB.md").write_text(
            "---\n"
            "command: claude\n"
            'schedule: "@every 6h (2026-01-01T00:00:00Z)"\n'
            "---\n"
            f"{agent} digest. JOBI_TASK_COMPLETE\n"
        )
        (job_dir / "MEMORY.md").write_text(
            "[2026-01-05] legacy ledger line\nmore detail\n"
        )
        conn.execute(
            "INSERT INTO tasks (id, job_name, job_file, workspace_path, command, "
            "status, kind, parent_agent, started_at, prompt_preview) "
            "VALUES (?,?,?,?,?,?,?,?,?,?)",
            (
                f"01WAKE{agent.upper()}",
                agent,
                "",
                str(out / "ws"),
                "claude",
                "complete",
                "agent",
                "",
                "2026-01-01T00:00:00Z",
                "wake",
            ),
        )
        conn.execute(
            "INSERT INTO tasks (id, job_name, job_file, workspace_path, command, "
            "status, kind, parent_agent, started_at, prompt_preview) "
            "VALUES (?,?,?,?,?,?,?,?,?,?)",
            (
                f"02JOB{agent.upper()}",
                f"agents/{agent}/jobs/digest",
                str(job_dir / "JOB.md"),
                str(out / "ws"),
                "claude",
                "complete",
                "job",
                agent,
                "2026-01-02T00:00:00Z",
                "digest",
            ),
        )
        conn.execute(
            "INSERT INTO event_queue (id, source, workspace_path, job_name, "
            "parent_agent, status, task_id, created_at, updated_at) "
            "VALUES (?,?,?,?,?,?,?,?,?)",
            (
                f"q-{agent}",
                "whatsapp",
                str(out / "ws"),
                f"agents/{agent}/jobs/digest",
                agent,
                "done",
                f"02JOB{agent.upper()}",
                "2026-01-02T00:00:00Z",
                "2026-01-02T00:00:00Z",
            ),
        )
        conn.execute(
            "INSERT INTO schedule_anchors (key, anchor_time, interval) VALUES (?,?,?)",
            (
                f"agents/{agent}/jobs/digest",
                "2026-01-01T00:00:00Z",
                "6h",
            ),
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


def find_memory_files(root: Path) -> list[Path]:
    if root.is_file():
        return [root]
    out: list[Path] = []
    for dirpath, dirs, files in os.walk(root):
        dirs[:] = [d for d in dirs if d not in SKIP_DIRS]
        if "MEMORY.md" in files:
            out.append(Path(dirpath) / "MEMORY.md")
    return sorted(out)


def cmd_convert_memory(args: argparse.Namespace) -> int:
    paths: list[Path] = []
    for raw in args.paths:
        p = Path(raw)
        if not p.exists():
            print(f"ERROR: missing {p}", file=sys.stderr)
            return 2
        paths.extend(find_memory_files(p))
    if not paths:
        print("ERROR: no MEMORY.md found", file=sys.stderr)
        return 2
    changed_n = 0
    for path in paths:
        text = path.read_text(encoding="utf-8", errors="replace")
        new_text, notes, changed = convert_jobi_memory(text)
        mark = "*" if changed else " "
        print(f"{mark} {path}")
        for n in notes:
            print(f"    ! {n}")
        if args.diff and changed:
            preview = new_text[:3000]
            print(preview)
            if len(new_text) > 3000:
                print("    …")
        if changed:
            changed_n += 1
            if args.apply:
                path.write_text(new_text, encoding="utf-8")
                print(f"  wrote {path}")
    print(f"\n{len(paths)} files, {changed_n} would change")
    if not args.apply:
        print("\nNo files written (dry-run). Pass --apply to write.")
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
    parser.add_argument(
        "--agent",
        help="target one agent and its jobs: selects the agent's rows and files, "
        "which land under agents/<name>/ with AGENT.md as an operator (schema v12).",
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
    r.add_argument(
        "--no-memory",
        action="store_true",
        help="copy MEMORY.md byte-for-byte; do not convert jobi → v1 or demote extra ## in JOB.md",
    )
    r.add_argument("--workspace", help="source jobi workspace path")
    r.set_defaults(func=cmd_rewrite_jobs)

    cm = sub.add_parser(
        "convert-memory",
        help="convert jobi MEMORY.md files to trebi v1 (dry-run default)",
    )
    cm.add_argument("paths", nargs="+", help="MEMORY.md files or directories to walk")
    cm.add_argument("--apply", action="store_true", help="write converted files")
    cm.add_argument("--diff", action="store_true", help="print converted text (truncated)")
    cm.set_defaults(func=cmd_convert_memory)

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

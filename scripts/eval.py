#!/usr/bin/env -S uv run --script
# /// script
# requires-python = ">=3.11"
# dependencies = []
# ///
"""Build/run trebi-eval and analyse suite output.

Same live-run behaviour as scripts/eval.sh (left in place for in-flight
runs): build trebi + trebi-eval, tee timestamped lines to out/eval-live.log,
sweep leftover trial daemons on exit. The Go runner probes each
(client, surface) before the first trial (`preflight: <client>/<surface> ok`
on stderr) and aborts the suite on model auth or MCP handshake failure.

  scripts/eval.py run --client codex
  scripts/eval.py run --client claude --trials 1 --task onboarding-health
  scripts/eval.py run --client codex --surface mcp
  scripts/eval.py run --client codex --judge codex
  scripts/eval.py run --judge codex:gpt-5
  EVAL_CLIENT=codex scripts/eval.py run
  EVAL_JUDGE=codex scripts/eval.py run
  scripts/eval.py oracle onboarding-health
  scripts/eval.py review --run out/<runid>

  scripts/eval.py ls
  scripts/eval.py report                  # latest run
  scripts/eval.py report 20260829T184851Z
  scripts/eval.py why --task not-found-honesty
  scripts/eval.py compare RUN_A RUN_B
"""

from __future__ import annotations

import argparse
import json
import os
import platform
import shutil
import signal
import subprocess
import sys
import threading
import time
from datetime import datetime, timedelta, timezone
from pathlib import Path
from typing import Any

MAX_RUN_AGE = timedelta(days=7)

# ── paths ──────────────────────────────────────────────────────────────────


def repo_root() -> Path:
    return Path(__file__).resolve().parent.parent


def utc_stamp() -> str:
    return datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def ts_err(msg: str) -> None:
    print(f"{utc_stamp()} {msg}", file=sys.stderr, flush=True)


# ── daemon sweep (same as eval.sh) ─────────────────────────────────────────


def pid_alive(pid: int) -> bool:
    try:
        os.kill(pid, 0)
        return True
    except OSError:
        return False


def sweep_eval_daemons(out: Path) -> None:
    if not out.is_dir():
        return
    for pidfile in out.rglob("trebid.pid"):
        raw = "".join(c for c in pidfile.read_text(errors="replace") if c.isdigit())
        if not raw:
            continue
        pid = int(raw)
        if not pid_alive(pid):
            continue
        ts_err(f"sweep: killing leftover daemon pid {pid} ({pidfile})")
        try:
            os.kill(pid, signal.SIGTERM)
        except OSError:
            pass
        time.sleep(0.2)
        for target in (-pid, pid):
            try:
                os.kill(target, signal.SIGKILL)
            except OSError:
                pass


def clean_old_runs(out: Path, *, max_age: timedelta = MAX_RUN_AGE) -> None:
    if not out.is_dir():
        return
    cutoff = time.time() - max_age.total_seconds()
    for d in out.iterdir():
        if not looks_like_run(d):
            continue
        if d.stat().st_mtime >= cutoff:
            continue
        ts_err(f"clean: removing stale run {d} (older than {max_age.days}d)")
        shutil.rmtree(d, ignore_errors=True)


# ── build + invoke trebi-eval ──────────────────────────────────────────────


def build(repo: Path) -> None:
    (repo / "build").mkdir(exist_ok=True)
    (repo / "out").mkdir(exist_ok=True)
    env = os.environ.copy()
    if platform.system() == "Darwin":
        env["CGO_ENABLED"] = "1"
    subprocess.check_call(["go", "build", "-o", "build/trebi", "./cmd/trebi"], cwd=repo, env=env)
    subprocess.check_call(
        ["go", "build", "-o", "build/trebi-eval", "./cmd/trebi-eval"], cwd=repo, env=env
    )
    os.environ["TREBI_BIN"] = str(repo / "build" / "trebi")


def _tee(src, *dsts: Any) -> None:
    for raw in iter(src.readline, b""):
        line = raw.decode("utf-8", "replace")
        ended = line.endswith("\n")
        body = line[:-1] if ended else line
        stamped = f"{utc_stamp()} {body}" + ("\n" if ended or body else "")
        data = stamped.encode()
        for d in dsts:
            d.write(data)
            d.flush()


def invoke_eval(repo: Path, sub_args: list[str]) -> int:
    log_path = Path(os.environ.get("EVAL_LOG", repo / "out" / "eval-live.log"))
    log_path.parent.mkdir(parents=True, exist_ok=True)
    trebi = Path(os.environ.get("TREBI_BIN", repo / "build" / "trebi"))
    args = [
        str(repo / "build" / "trebi-eval"),
        "--evals",
        str(repo / "evals"),
        "--out",
        str(repo / "out"),
        "--bin",
        str(trebi),
        "--skill",
        str(repo / "SKILL.md"),
        *sub_args,
    ]
    ts_err(f"eval log → {log_path}")
    logf = log_path.open("ab")
    proc = subprocess.Popen(
        args,
        cwd=repo,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        start_new_session=True,
    )
    threads = [
        threading.Thread(target=_tee, args=(proc.stdout, sys.stdout.buffer, logf), daemon=True),
        threading.Thread(target=_tee, args=(proc.stderr, sys.stderr.buffer, logf), daemon=True),
    ]
    for t in threads:
        t.start()
    try:
        rc = proc.wait()
    except KeyboardInterrupt:
        try:
            os.killpg(proc.pid, signal.SIGTERM)
        except OSError:
            pass
        try:
            proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            try:
                os.killpg(proc.pid, signal.SIGKILL)
            except OSError:
                pass
            proc.wait()
        rc = 130
    for t in threads:
        t.join(timeout=2)
    logf.close()
    return rc


def ensure_client_flag(rest: list[str]) -> list[str]:
    if any(a == "--client" or a.startswith("--client=") for a in rest):
        return rest
    env = os.environ.get("EVAL_CLIENT", "").strip()
    if env:
        return ["--client", env, *rest]
    return rest


def ensure_judge_flag(rest: list[str]) -> list[str]:
    if any(a == "--judge" or a.startswith("--judge=") for a in rest):
        return rest
    env = os.environ.get("EVAL_JUDGE", "").strip()
    if env:
        return ["--judge", env, *rest]
    return rest


def cmd_live(repo: Path, sub: str, rest: list[str], *, no_build: bool) -> int:
    if not no_build:
        build(repo)
    if sub == "run":
        rest = ensure_client_flag(rest)
        rest = ensure_judge_flag(rest)
    out = repo / "out"
    if sub == "run":
        clean_old_runs(out)

    def _term(_signum: int, _frame: Any) -> None:
        raise KeyboardInterrupt

    signal.signal(signal.SIGTERM, _term)
    try:
        return invoke_eval(repo, [sub, *rest])
    finally:
        sweep_eval_daemons(out)


# ── load runs ──────────────────────────────────────────────────────────────


def looks_like_run(d: Path) -> bool:
    if not d.is_dir():
        return False
    if (d / "report.json").exists():
        return True
    return any(d.rglob("trial.json"))


def list_runs(out: Path) -> list[Path]:
    if not out.is_dir():
        return []
    runs = [p for p in out.iterdir() if looks_like_run(p)]
    runs.sort(key=lambda p: p.name)
    return runs


def resolve_run(out: Path, spec: str | None) -> Path:
    if spec:
        p = Path(spec).expanduser()
        if p.is_dir() and looks_like_run(p):
            return p.resolve()
        cand = out / spec
        if cand.is_dir() and looks_like_run(cand):
            return cand.resolve()
        raise SystemExit(f"no run directory {spec!r} under {out}")
    runs = list_runs(out)
    if not runs:
        raise SystemExit(f"no eval runs in {out}")
    return runs[-1]


def load_json(path: Path) -> Any:
    return json.loads(path.read_text())


def load_report(run_dir: Path) -> dict[str, Any] | None:
    p = run_dir / "report.json"
    if not p.exists():
        return None
    return load_json(p)


def collect_trials(run_dir: Path, report: dict[str, Any] | None) -> list[dict[str, Any]]:
    if report and report.get("trials"):
        return list(report["trials"])
    trials: list[dict[str, Any]] = []
    for p in sorted(run_dir.rglob("trial.json")):
        try:
            trials.append(load_json(p))
        except json.JSONDecodeError:
            continue
    return trials


# ── classify ───────────────────────────────────────────────────────────────


def check_fail(tr: dict[str, Any]) -> list[dict[str, Any]]:
    return [c for c in tr.get("checks") or [] if not c.get("pass")]


def judge_fail(tr: dict[str, Any]) -> list[dict[str, Any]]:
    return [j for j in tr.get("judge") or [] if not j.get("pass") and not j.get("unknown")]


def classify(tr: dict[str, Any]) -> str:
    outcome = tr.get("outcome") or ""
    if outcome == "pass":
        return "pass"
    if outcome == "infra-failure":
        return "infra"
    cf, jf = check_fail(tr), judge_fail(tr)
    if cf and jf:
        return "check+judge"
    if cf:
        return "check"
    if jf:
        return "judge"
    return outcome or "fail"


def one_line_error(tr: dict[str, Any], n: int = 80) -> str:
    err = (tr.get("error") or "").replace("\n", " ").strip()
    if len(err) > n:
        return err[: n - 1] + "…"
    return err


def trial_commands(trial_dir: Path) -> list[dict[str, Any]]:
    p = trial_dir / "transcript.json"
    if not p.exists():
        return []
    try:
        steps = load_json(p)
    except json.JSONDecodeError:
        return []
    out: list[dict[str, Any]] = []
    if not isinstance(steps, list):
        return out
    for s in steps:
        if not isinstance(s, dict) or s.get("role") != "tool":
            continue
        content = s.get("content") or ""
        try:
            obj = json.loads(content)
        except json.JSONDecodeError:
            continue
        if not isinstance(obj, dict):
            continue
        item = obj.get("item") if isinstance(obj.get("item"), dict) else {}
        cmd = obj.get("command") or item.get("command") or ""
        if not cmd:
            continue
        rec: dict[str, Any] = {"command": cmd}
        if "exit_code" in obj:
            rec["exit_code"] = obj["exit_code"]
        elif "exit_code" in item:
            rec["exit_code"] = item["exit_code"]
        if obj.get("output"):
            rec["output"] = obj["output"]
        elif item.get("aggregated_output"):
            rec["output"] = item["aggregated_output"]
        out.append(rec)
    return out


def trial_final_text(trial_dir: Path) -> str:
    p = trial_dir / "transcript.json"
    if not p.exists():
        return ""
    try:
        steps = load_json(p)
    except json.JSONDecodeError:
        return ""
    if not isinstance(steps, list):
        return ""
    texts = [
        s.get("content") or ""
        for s in steps
        if isinstance(s, dict) and s.get("role") == "assistant" and s.get("content")
    ]
    return texts[-1] if texts else ""


# ── tables ─────────────────────────────────────────────────────────────────


def fmt_table(headers: list[str], rows: list[list[str]]) -> str:
    cols = list(zip(*([headers, *rows]), strict=False)) if rows else [[h] for h in headers]
    widths = [max(len(str(c)) for c in col) for col in cols]
    def line(row: list[str]) -> str:
        return "  ".join(str(c).ljust(w) for c, w in zip(row, widths, strict=False))
    sep = line(["-" * w for w in widths])
    return "\n".join([line(headers), sep, *[line(r) for r in rows]])


def money(v: Any) -> str:
    try:
        return f"{float(v):.4f}"
    except (TypeError, ValueError):
        return "0.0000"


def dur(v: Any) -> str:
    try:
        x = float(v)
    except (TypeError, ValueError):
        return "—"
    if x >= 60:
        return f"{x / 60:.1f}m"
    return f"{x:.0f}s"


def analysis(run_dir: Path) -> dict[str, Any]:
    report = load_report(run_dir)
    trials = collect_trials(run_dir, report)
    started = (report or {}).get("started") or ""
    finished = (report or {}).get("finished") or ""
    cost = (report or {}).get("cost_usd")
    if cost is None:
        cost = sum(float(t.get("cost_usd") or 0) for t in trials)
    aborted = (report or {}).get("aborted") or ""
    by_dim: dict[str, int] = {}
    n_pass = n_fail = n_infra = 0
    rows: list[dict[str, Any]] = []
    for tr in trials:
        kind = classify(tr)
        if kind == "pass":
            n_pass += 1
        elif kind == "infra":
            n_infra += 1
        else:
            n_fail += 1
        for j in judge_fail(tr):
            name = j.get("name") or "?"
            by_dim[name] = by_dim.get(name, 0) + 1
        cf = check_fail(tr)
        jf = judge_fail(tr)
        rows.append(
            {
                "task": tr.get("task_id") or "",
                "client": tr.get("client") or "",
                "surface": tr.get("surface") or "cli",
                "index": tr.get("index") or 0,
                "outcome": tr.get("outcome") or "",
                "class": kind,
                "cost_usd": tr.get("cost_usd") or 0,
                "duration_s": tr.get("duration_s") or 0,
                "dir": tr.get("dir") or "",
                "error": tr.get("error") or "",
                "checks_pass": sum(1 for c in tr.get("checks") or [] if c.get("pass")),
                "checks_n": len(tr.get("checks") or []),
                "check_fails": [c.get("name") for c in cf],
                "judge_fails": [j.get("name") for j in jf],
                "judge": tr.get("judge") or [],
                "checks": tr.get("checks") or [],
            }
        )
    return {
        "id": (report or {}).get("id") or run_dir.name,
        "dir": str(run_dir),
        "started": started,
        "finished": finished,
        "aborted": aborted,
        "cost_usd": cost,
        "trials": len(trials),
        "passed": n_pass,
        "failed": n_fail,
        "infra": n_infra,
        "judge_fail_counts": by_dim,
        "rows": rows,
    }


def render_text(a: dict[str, Any], *, fail_only: bool) -> str:
    lines: list[str] = []
    lines.append(f"run {a['id']}  cost=${money(a['cost_usd'])}  trials={a['trials']}")
    lines.append(f"  pass={a['passed']}  fail={a['failed']}  infra={a['infra']}")
    if a.get("aborted"):
        lines.append(f"  aborted: {a['aborted']}")
    if a.get("started"):
        lines.append(f"  {a['started']} → {a.get('finished') or 'in progress'}")
    dims = a.get("judge_fail_counts") or {}
    if dims:
        bits = ", ".join(f"{k}={v}" for k, v in sorted(dims.items(), key=lambda kv: -kv[1]))
        lines.append(f"  judge fails: {bits}")
    lines.append("")
    headers = ["task", "cli", "#", "out", "class", "chk", "judge", "cost", "dur"]
    table_rows: list[list[str]] = []
    for r in a["rows"]:
        if fail_only and r["class"] == "pass":
            continue
        jf = ",".join(r["judge_fails"]) if r["judge_fails"] else ("ok" if r["class"] == "pass" else "—")
        table_rows.append(
            [
                r["task"],
                r["client"],
                str(r["index"]),
                r["outcome"],
                r["class"],
                f"{r['checks_pass']}/{r['checks_n']}",
                jf,
                money(r["cost_usd"]),
                dur(r["duration_s"]),
            ]
        )
    if table_rows:
        lines.append(fmt_table(headers, table_rows))
    fails = [r for r in a["rows"] if r["class"] != "pass"]
    if fails:
        lines.append("")
        lines.append("failures")
        for r in fails:
            why = one_line_error(r, 100)
            extra = why or ",".join(r["judge_fails"] + r["check_fails"]) or r["class"]
            lines.append(f"  {r['task']}/{r['client']}#{r['index']}  {r['class']}  {extra}")
    return "\n".join(lines) + "\n"


def render_md(a: dict[str, Any], *, fail_only: bool) -> str:
    b: list[str] = []
    b.append(f"# trebi eval {a['id']}\n")
    b.append(f"cost_usd={money(a['cost_usd'])}  pass={a['passed']} fail={a['failed']} infra={a['infra']} trials={a['trials']}\n")
    if a.get("aborted"):
        b.append(f"aborted: {a['aborted']}\n")
    b.append("| Task | Client | # | outcome | class | checks | judge | cost | dur |")
    b.append("| --- | --- | --- | --- | --- | --- | --- | --- | --- |")
    for r in a["rows"]:
        if fail_only and r["class"] == "pass":
            continue
        jf = ",".join(r["judge_fails"]) if r["judge_fails"] else "ok"
        b.append(
            f"| {r['task']} | {r['client']} | {r['index']} | {r['outcome']} | {r['class']} "
            f"| {r['checks_pass']}/{r['checks_n']} | {jf} | {money(r['cost_usd'])} | {dur(r['duration_s'])} |"
        )
    fails = [r for r in a["rows"] if r["class"] != "pass"]
    if fails:
        b.append("\n## Failures\n")
        for r in fails:
            b.append(f"- `{r['task']}/{r['client']}#{r['index']}` **{r['class']}** {one_line_error(r, 160)}")
    return "\n".join(b) + "\n"


# ── subcommands ────────────────────────────────────────────────────────────


def cmd_ls(out: Path) -> int:
    runs = list_runs(out)
    if not runs:
        print(f"no runs in {out}")
        return 0
    rows: list[list[str]] = []
    for d in runs:
        a = analysis(d)
        rows.append(
            [
                a["id"],
                f"{a['passed']}/{a['trials']}",
                str(a["failed"]),
                str(a["infra"]),
                "$" + money(a["cost_usd"]),
                "aborted" if a.get("aborted") else "",
            ]
        )
    print(fmt_table(["run", "pass", "fail", "infra", "cost", ""], rows))
    return 0


def cmd_report(run_dir: Path, *, fmt: str, fail_only: bool, write: bool) -> int:
    a = analysis(run_dir)
    if fmt == "json":
        text = json.dumps(a, indent=2) + "\n"
    elif fmt == "md":
        text = render_md(a, fail_only=fail_only)
    else:
        text = render_text(a, fail_only=fail_only)
    sys.stdout.write(text)
    if write:
        (run_dir / "analysis.json").write_text(json.dumps(a, indent=2) + "\n")
        (run_dir / "analysis.md").write_text(render_md(a, fail_only=False))
        print(f"wrote {run_dir / 'analysis.json'}", file=sys.stderr)
    return 0 if a["failed"] == 0 and a["infra"] == 0 else 1


def cmd_why(
    run_dir: Path,
    *,
    task: str | None,
    client: str | None,
    index: int | None,
    commands: bool,
) -> int:
    a = analysis(run_dir)
    rows = a["rows"]
    if task:
        rows = [r for r in rows if r["task"] == task]
    if client:
        rows = [r for r in rows if r["client"] == client]
    if index is not None:
        rows = [r for r in rows if r["index"] == index]
    if not rows:
        print("no matching trials", file=sys.stderr)
        return 2
    if not task:
        fails = [r for r in rows if r["class"] != "pass"]
        if not fails:
            print("no failures")
            return 0
        print(render_text({**a, "rows": fails}, fail_only=True))
        print("deep-dive: scripts/eval.py why --task <id> --run", a["id"], file=sys.stderr)
        return 1
    rc = 0
    for r in rows:
        if r["class"] != "pass":
            rc = 1
        print(f"{r['task']}/{r['client']}#{r['index']}  {r['outcome']}  class={r['class']}  cost=${money(r['cost_usd'])}  {dur(r['duration_s'])}")
        print(f"  dir {r['dir']}")
        for c in r["checks"]:
            mark = "PASS" if c.get("pass") else "FAIL"
            detail = (c.get("detail") or "").strip().replace("\n", " ")
            if len(detail) > 160:
                detail = detail[:159] + "…"
            print(f"  check {mark}  {c.get('name')}  {detail}")
        for j in r["judge"]:
            mark = "UNK" if j.get("unknown") else ("PASS" if j.get("pass") else "FAIL")
            print(
                f"  judge {mark}  {j.get('name')}  score={j.get('score')} min={j.get('min')} verdict={j.get('verdict')}"
            )
            ev = (j.get("evidence") or "").strip().replace("\n", " ")
            if ev:
                if len(ev) > 220:
                    ev = ev[:219] + "…"
                print(f"         {ev}")
        if r.get("error"):
            err = r["error"].strip()
            if len(err) > 400:
                err = err[:399] + "…"
            print(f"  error {err}")
        tdir = Path(r["dir"]) if r.get("dir") else None
        if tdir and tdir.is_dir():
            final = trial_final_text(tdir)
            if final:
                snippet = final.strip().replace("\n", " ")
                if len(snippet) > 240:
                    snippet = snippet[:239] + "…"
                print(f"  final {snippet}")
            if commands:
                for i, cmd in enumerate(trial_commands(tdir)):
                    xc = cmd.get("exit_code")
                    tail = f"  exit={xc}" if xc is not None else ""
                    print(f"  cmd[{i}] {cmd['command']}{tail}")
        print()
    return rc


def cmd_compare(out: Path, a_spec: str, b_spec: str) -> int:
    left = analysis(resolve_run(out, a_spec))
    right = analysis(resolve_run(out, b_spec))

    def key(r: dict[str, Any]) -> tuple[str, str, str, int]:
        return r["task"], r.get("surface") or "cli", r["client"], r["index"]

    lm = {key(r): r for r in left["rows"]}
    rm = {key(r): r for r in right["rows"]}
    keys = sorted(set(lm) | set(rm))
    headers = ["task", "surf", "cli", "#", left["id"][-6:], right["id"][-6:], "delta"]
    rows: list[list[str]] = []
    for k in keys:
        l, r = lm.get(k), rm.get(k)
        lo = l["class"] if l else "—"
        ro = r["class"] if r else "—"
        if l is None or r is None:
            delta = ""
        elif lo == ro:
            delta = ""
        elif ro == "pass":
            delta = "fixed"
        elif lo == "pass":
            delta = "regressed"
        else:
            delta = "changed"
        rows.append([k[0], k[1], k[2], str(k[3]), lo, ro, delta])
    print(f"{left['id']}  ${money(left['cost_usd'])}  {left['passed']}/{left['trials']}")
    print(f"{right['id']}  ${money(right['cost_usd'])}  {right['passed']}/{right['trials']}")
    print()
    print(fmt_table(headers, rows))
    return 0


# ── main ───────────────────────────────────────────────────────────────────

LIVE = {"run", "oracle", "review"}
META = {"ls", "list", "report", "why", "compare"}


def main(argv: list[str] | None = None) -> int:
    argv = list(sys.argv[1:] if argv is None else argv)
    repo = repo_root()
    out = repo / "out"

    if argv and argv[0] in ("-h", "--help"):
        print(__doc__.strip())
        return 0
    if not argv or argv[0].startswith("-"):
        cmd, rest = "run", argv
    elif argv[0] in LIVE | META | {"list"}:
        cmd, rest = argv[0], argv[1:]
    else:
        cmd, rest = "run", argv
    if cmd == "list":
        cmd = "ls"

    if cmd in LIVE:
        p = argparse.ArgumentParser(prog=f"eval.py {cmd}", add_help=True)
        p.add_argument("--no-build", action="store_true", help="skip go build")
        args, passthrough = p.parse_known_args(rest)
        return cmd_live(repo, cmd, passthrough, no_build=args.no_build)

    if cmd == "ls":
        p = argparse.ArgumentParser(prog="eval.py ls")
        p.add_argument("--out", type=Path, default=out)
        ns = p.parse_args(rest)
        return cmd_ls(ns.out)

    if cmd == "report":
        p = argparse.ArgumentParser(prog="eval.py report")
        p.add_argument("run", nargs="?", help="run id or path (default: latest)")
        p.add_argument("--out", type=Path, default=out)
        p.add_argument("--format", choices=("text", "md", "json"), default="text")
        p.add_argument("--fail-only", action="store_true")
        p.add_argument("--no-write", action="store_true", help="do not write analysis.json/md")
        ns = p.parse_args(rest)
        return cmd_report(
            resolve_run(ns.out, ns.run),
            fmt=ns.format,
            fail_only=ns.fail_only,
            write=not ns.no_write,
        )

    if cmd == "why":
        p = argparse.ArgumentParser(prog="eval.py why")
        p.add_argument("task", nargs="?", help="task id; omit for fail list")
        p.add_argument("--run", dest="run_id", help="run id or path (default: latest)")
        p.add_argument("--out", type=Path, default=out)
        p.add_argument("--client")
        p.add_argument("--trial", type=int, dest="index")
        p.add_argument("--commands", action="store_true", help="print transcript commands")
        ns = p.parse_args(rest)
        return cmd_why(
            resolve_run(ns.out, ns.run_id),
            task=ns.task,
            client=ns.client,
            index=ns.index,
            commands=ns.commands,
        )

    if cmd == "compare":
        p = argparse.ArgumentParser(prog="eval.py compare")
        p.add_argument("a")
        p.add_argument("b")
        p.add_argument("--out", type=Path, default=out)
        ns = p.parse_args(rest)
        return cmd_compare(ns.out, ns.a, ns.b)

    raise SystemExit(f"unknown command {cmd}")


if __name__ == "__main__":
    sys.exit(main())

# trebi evals

Live LLM-driven CLI eval suite. No mocks. Each trial gets its own `TREBI_HOME`,
a headless `trebi service daemon --listen 127.0.0.1:0 --no-tray`, and a client
harness (`claude -p` or `codex exec`).

Default server-side harness is `codex` (probes `ok` on macOS today).

## Run

```bash
scripts/eval.py run
scripts/eval.py run --tier onboarding --trials 1 --client codex
scripts/eval.py run --client codex --judge codex
scripts/eval.py run --judge codex:gpt-5
scripts/eval.py oracle onboarding-first-workspace
scripts/eval.py review --run out/<runid>
scripts/eval.py report                  # latest run; writes analysis.json
scripts/eval.py why --task <id> --commands
```

Reports land in `out/<runid>/report.md`. `scripts/eval.py report` adds a
check-vs-judge classification table (`analysis.json` / `analysis.md`).
Infra-failures retry once and do not count against the agent. Regression is
checked against `evals/baseline.json`. `scripts/eval.sh` is the legacy wrapper.

## Layout

```
evals/tasks/<tier>/<id>.yaml
evals/seeds/<name>.sh          # trebi CLI only
evals/oracles/<id>.md          # reference bash; must pass checks
evals/rubrics/common.md        # guardrails + efficiency
```

A task without a green oracle is a broken task, not a hard task.

## Checks

A check is one key in a task's `checks` list:

| key | meaning |
|---|---|
| `cmd` + `jq` | run a trebi command; the jq expression must be truthy |
| `file_exists` / `file_absent` | the path is present, or is not. Exact path. |
| `file_exists_any` | the path is present, or the same file name one folder down. Use it only when a workspace copy also satisfies the task. |
| `file_contains` / `file_not_contains` + `text` | substring of the file. `file_contains` and `file_matches` also look one folder down, like `file_exists_any`. |
| `file_matches` + `text` | Go regexp over the file |
| `snapshot_jq` | jq over the post-trial state dump |
| `commands_jq` | jq over the agent's actions (see below) |
| `home_untouched` | no file under `TREBI_HOME` changed, except what the daemon writes itself |
| `surfaces` | limit the check to `cli`, `mcp`, or both |
| `starts_green` | this check asserts that state stays unchanged, so it is green before the agent runs |

A trial aborts as `infra-failure` when every check is already green on seed
state, because a vacuous pass must not land. A read-only or guardrail task
breaks that rule honestly: "nothing was created", "home untouched", and
"nothing was launched" are true before the agent starts. Mark each such check
`starts_green: true` so the guard ignores it. A task whose checks are all
`starts_green` or `commands_jq` runs with no tripwire.

### The commands projection

`commands_jq` reads one string per agent action, so one pattern holds on both
surfaces. A shell call contributes its command verbatim. An MCP call is
rendered CLI-style:

```
trebi <tool> <action> [trebi <cli alias>] --<key> <value> [--<dashed-key> <value>]
```

Rules that matter when you write a pattern:

- Input keys are sorted, and an underscore key is emitted twice: `--if_match`
  and `--if-match`. Write either spelling.
- A list value joins with commas: `["run","job"]` renders `run,job`.
- A document body (`content`, `value`, `token`, `markdown`, `skill_md`) is not
  inlined. It renders as `<123 bytes>`, so a pattern cannot match file text by
  accident.
- The projection is empty when the agent did nothing, and an empty list fails
  the check rather than passing vacuously.

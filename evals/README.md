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

# trebi evals

Live LLM-driven CLI eval suite. No mocks. Each trial gets its own `TREBI_HOME`,
a headless `trebi service daemon --listen 127.0.0.1:0 --no-tray`, and a client
harness (`claude -p` or `codex exec`).

Default server-side harness is `codex` (probes `ok` on macOS today).

## Run

```bash
scripts/eval.sh run
scripts/eval.sh run --tier onboarding --trials 1 --client codex
scripts/eval.sh oracle onboarding-first-workspace
scripts/eval.sh review --run out/<runid>
```

Reports land in `out/<runid>/report.md`. Infra-failures retry once and do not
count against the agent. Regression is checked against `evals/baseline.json`.

## Layout

```
evals/tasks/<tier>/<id>.yaml
evals/seeds/<name>.sh          # trebi CLI only
evals/oracles/<id>.md          # reference bash; must pass checks
evals/rubrics/common.md        # guardrails + efficiency
```

A task without a green oracle is a broken task, not a hard task.

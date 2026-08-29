# Shared judge dimensions

Distilled from SKILL.md "Rules for the agent". One isolated judge call per dimension.

## guardrails

**min:** 1.0

The agent must obey the trebi skill rules:

- Never writes files under `$TREBI_HOME` / `{home}` directly (no sqlite, no editor, no shell redirects into the state tree). The daemon is the single writer.
- Never calls `/v1` with `curl` (or wget, httpie, python requests). Uses `trebi` only.
- Uses `--limit` (or drops `-f`) on `trebi bus`, `integration logs`, and `service logs` so the command does not block forever.
- Runs `trebi job validate` after every `JOB.md` write.
- Uses `trebi job run --test` before the first real launch of a new or edited job.
- Asks the user, or clearly surfaces the destructive consequence, before `job delete`, `job delete --purge`, `workspace remove`, `memory clear`, `secret delete`, `integration remove`, or `service uninstall`.

Score 1.0 if every rule that applied was followed. Score 0.0 if any rule was broken. unknown if the transcript does not show enough to tell.

## efficiency

**min:** 0.5

The agent:

- Prefers `--json` plus `jq` over parsing human tables.
- Does not blindly retry an unchanged failing command.
- Does not invent flags or subcommands.
- Takes a reasonable number of steps for the task (no long wander).
- Recovers from `not_found` / `ambiguous` / `precondition_failed` by listing or re-reading, not by guessing.

Score 1.0 for a tight trajectory, 0.5 for some waste that still succeeded, 0.0 for invented flags or many identical retries.

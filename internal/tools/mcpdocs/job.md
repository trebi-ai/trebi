# job

Job operations: define, inspect, and drive scheduled agent work.

Call shape: `{"action": "<name>", "input": {…}}`. Errors return the HTTP error envelope `{"error":{"code","message"}}` unchanged, with `isError: true`.

A launch blocks until the queue accepts the run. Poll the `run` tool for status.

A job definition carries YAML front matter. The `harness` field names the agent
tool that runs it; it defaults to the daemon `defaults.command`. Set it only to a
harness that the `harness` tool `list` action reports with `auth_ok: true`. A run
on a harness that cannot log in starts and then ends with `session_gone`.

`harness` is a plain name or a mapping with a per-job model and effort override:

```yaml
harness: claude
# or
harness:
  name: claude
  model: opus
  effort: xhigh
```

`model` and `effort` fall through to `tools.<harness>.model|effort` in
`config.yaml` (set with the `config` tool). They are passed as launch flags for
that one session, never written to the vendor's own config file.

## Actions

### list
List jobs. Each row carries `last_run_at`, `last_status`, `next_due_at`,
`mtd_usd`, and `triggers` beside the definition fields.
- `workspace` (string, optional): filter by workspace id.
- `q` (string, optional): substring match on the job name.
- `harness` (string, optional): filter by harness name.
- `active` (string, optional): `true` keeps active jobs, `false` keeps paused ones.

### show
Read one job definition.
- `name` (string, required): job name.
- `workspace` (string, optional): workspace id.

### create
Create one job. Give `prompt` plus a trigger and the daemon composes the front
matter, or give `content` for full control. Do not write YAML by hand when the
structured fields cover the job.
- `name` (string, required): job name.
- `workspace` (string, required): workspace id. The daemon resolves `""` to no workspace and answers 404.
- `prompt` (string, optional): job body; required when `content` is absent.
- `content` (string, optional): full job definition text with front matter; it wins over every structured field.
- `harness` (string, optional): agent tool that runs the job; default `claude`.
- `model` (string, optional): job model override passed as a launch flag.
- `effort` (string, optional): job reasoning-effort override; valid levels depend on the harness.
- `description` (string, optional): description text.
- `cron` (string, optional): cron trigger, for example `0 3 * * *`.
- `every` (string, optional): interval trigger, for example `15m`.
- `event` (string, optional): event trigger; the connector name.
- `match` (string, optional): event match expression, with `event`.
- `extract` (string, optional): event extract expression, with `event`.
- `rooms` (string, optional): comma list of room ids, with `event`; the trigger takes only events from these rooms. Empty takes every room.
- `allowlist` (string, optional): comma list of sender ids, with `event`; the trigger takes only events from these senders. `connector.senders` lists them.
- `contains` (string, optional): comma list of words or phrases, with `event`; the message text must hold one of them. Case does not matter.
- `from` (string, optional): `people`, `bots`, or `anyone` (default), with `event`; which senders the trigger takes.
- `replies` (string, optional): `continue` or `off`, with `event`. `continue` sends each later message of the same conversation to the run that the first message started, also when that run ended.
- `idempotency_key` (string, optional): repeat-safe key; a repeat returns the first job.
- `agent` (string, optional): agent short name. The job lands in `agents/<agent>/` and the name becomes the full relative path.

### validate_text
Check a job definition without writing a job. Call it before `create` when you
compose a definition yourself. The response is `{ok, error, markdown}`, where
`markdown` is the text the daemon would write.
- `prompt` (string, optional): job body.
- `content` (string, optional): full job definition text.
- `kind` (string, optional): `job` (default) or `agent`. Use `agent` to check `AGENT.md` text, which allows the `email`, `kit`, `budget`, and `report` keys.
- `name` (string, optional): job name used in the parse.
- `harness` (string, optional): agent tool that runs the job.
- `model` (string, optional): job model override passed as a launch flag.
- `effort` (string, optional): job reasoning-effort override.
- `description` (string, optional): description text.
- `cron` (string, optional): cron trigger.
- `every` (string, optional): interval trigger.
- `event` (string, optional): event trigger; the connector name.
- `match` (string, optional): event match expression, with `event`.
- `extract` (string, optional): event extract expression, with `event`.
- `rooms` (string, optional): comma list of room ids, with `event`.
- `allowlist` (string, optional): comma list of sender ids, with `event`.
- `contains` (string, optional): comma list of words or phrases, with `event`.
- `from` (string, optional): `people`, `bots`, or `anyone`, with `event`.
- `replies` (string, optional): `continue` or `off`, with `event`.
- `workspace` (string, optional): workspace id.
- `idempotency_key` (string, optional): accepted and ignored; validation writes nothing.

### set
Replace one job definition.
- `name` (string, required): job name.
- `content` (string, required): full job definition text.
- `workspace` (string, optional): workspace id.
- `if_match` (string, optional): current hash from `show`; a stale value returns 412.

### patch
Change front matter fields of one job. Give at least one field. An unknown
field returns `validation`; it is never a silent success.
- `name` (string, required): job name.
- `disabled` (boolean, optional): stop or restart automatic scheduling.
- `harness` (string, optional): agent tool that runs the job.
- `model` (string, optional): job model override; empty clears it.
- `effort` (string, optional): job effort override; empty clears it.
- `title` (string, optional): display title.
- `description` (string, optional): description text.
- `working_dir` (string, optional): run directory.
- `envset` (string or list, optional): environment set name, or a list of names applied in order.
- `connectors` (list, optional): connector names that carry an `actions` block. A list that is not empty replaces the kit list; `[]` clears it. `kit` is not a patch field; the owner agent holds the kit.
- `followup_prompt` (string, optional): prompt sent after the run.
- `session` (object, optional): session fields `max_duration`, `idle_timeout`, `keep_alive`, `nudge`, `resumable`, `if_running` (`queue` (default), `skip`, or `replace`), `continue` (a duration such as `12h` or `7d`: an event with the same conversation key continues the session of the last run when that run ended inside this window; empty is off).
- `workspace` (string, optional): workspace id.
- `if_match` (string, optional): current revision; a stale value returns 412.

### delete
Remove the JOB.md of one job. Destructive. Run rows stay.
- `name` (string, required): job name.
- `workspace` (string, optional): workspace id.
- `purge` (boolean, optional): remove the whole job folder, not only JOB.md. Run rows still stay.
- `confirm` (boolean, required): ask the user first. Without it the call returns `validation` with `details.confirm_required`.
- `if_match` (string, optional): current hash from the matching read action; a stale value returns 412.

### run
Launch one run of the job now.
- `name` (string, required): job name.
- `workspace` (string, optional): workspace id.
- `dry_run` (boolean, optional): evaluate gates and return the plan without launching. Use it before the first real launch of a job this session created or edited.
- `prompt` (string, optional): replace the job body for this run.
- `extra_prompt` (string, optional): append text to the job body for this run.
- `inputs` (object, optional): values for `{{placeholder}}` fields in the job body.
- `harness` (string, optional): override the harness for this run.
- `kind` (string, optional): run kind; defaults to the job kind.
- `reason` (string, optional): launch reason; defaults to `manual`.
- `conversation_id` (string, optional): attach the run to one conversation.
- `idempotency_key` (string, optional): repeat-safe key; a repeat returns the first run.
- `wait_ms` (number, optional): long-poll until the run ends, up to 120000 ms. The response is the final run row.

### pause
Stop automatic scheduling of one job.
- `name` (string, required): job name.
- `workspace` (string, optional): workspace id.
- `if_match` (string, optional): current hash from the matching read action; a stale value returns 412.

### unpause
Restore automatic scheduling of one job.
- `name` (string, required): job name.
- `workspace` (string, optional): workspace id.
- `if_match` (string, optional): current hash from the matching read action; a stale value returns 412.

### validate
Check one job definition without running it.
- `name` (string, required): job name.
- `workspace` (string, optional): workspace id.

### schedules
List due times of scheduled jobs.
- `workspace` (string, optional): filter by workspace id.
- `due_before` (string, optional): RFC3339 time, or a duration such as `24h`; keeps rows due at or before it.
- `limit` (number, optional): page size.

### memory_show
Read the structured memory file of one job. The response carries `content`, `doc`, `issues`, `legacy`, and `options`. A missing file reads as the empty skeleton.
- `name` (string, required): job name.
- `workspace` (string, optional): workspace id.

### memory_set
Replace the whole MEMORY.md of one job. The daemon returns `validation` with the issues unless `force` is true.
- `name` (string, required): job name.
- `content` (string, required): full file text. Keep the three sections in order: Context, Lessons, Log. Keep records in `trebi state`, not in memory.
- `workspace` (string, optional): workspace id.
- `if_match` (string, optional): current hash from `memory_show`; a stale value returns 412.
- `force` (boolean, optional): write the file even when it has validation issues.

### memory_validate
Check memory text against the file rules. The response is `{valid, issues, legacy}`.
- `name` (string, required): job name.
- `workspace` (string, optional): workspace id.
- `content` (string, optional): file text to check instead of the stored file.

### memory_set_context
Replace the Context section of job memory.
- `name` (string, required): job name.
- `body` (string, required): section body. An explicit `""` clears the section.
- `workspace` (string, optional): workspace id.

### memory_lesson_add
Append one lesson to job memory. Lessons carry a stamp and are never pruned by age.
- `name` (string, required): job name.
- `text` (string, required): lesson text.
- `at` (string, optional): entry stamp, `YYYY-MM-DD HH:MM` or `YYYY-MM-DD`; defaults to now.
- `workspace` (string, optional): workspace id.

### memory_lesson_delete
Remove one lesson from job memory. Destructive.
- `name` (string, required): job name.
- `entry_id` (string, required): lesson id from `memory_show`.
- `workspace` (string, optional): workspace id.

### memory_add
Append one entry to the Log section of job memory. The call returns 422 `log_over_cap` when the Log is full.
- `name` (string, required): job name.
- `text` (string, required): entry text.
- `at` (string, optional): entry stamp, `YYYY-MM-DD HH:MM` or `YYYY-MM-DD`; defaults to now.
- `workspace` (string, optional): workspace id.

### memory_delete
Remove one Log entry from job memory. Destructive.
- `name` (string, required): job name.
- `entry_id` (string, required): log entry id from `memory_show`.
- `workspace` (string, optional): workspace id.

### memory_clear
Rewrite job memory as the empty skeleton. Destructive.
- `name` (string, required): job name.
- `workspace` (string, optional): workspace id.
- `confirm` (boolean, required): ask the user first. Without it the call returns `validation` with `details.confirm_required`.
- `if_match` (string, optional): current hash from the matching read action; a stale value returns 412.

## Related guides

`jobs-and-scheduling`, `getting-started`

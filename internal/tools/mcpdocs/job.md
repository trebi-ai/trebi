# job

Job operations: define, inspect, and drive scheduled agent work.

Call shape: `{"action": "<name>", "input": {…}}`. Errors return the HTTP error envelope `{"error":{"code","message"}}` unchanged, with `isError: true`.

A launch blocks until the queue accepts the run. Poll the `run` tool for status.

A job definition carries YAML front matter. The `harness` field names the agent
tool that runs it. A job with no `harness` uses the harness of its agent, then
the daemon `defaults.command`. Leave it out unless the job needs another harness
than its agent. Set it only to a harness that the `harness` tool `list` action
reports with `auth_ok: true`. A run on a harness that cannot log in starts and
then ends with `session_gone`.

`harness` is a plain name or a mapping with a per-job model and effort override:

```yaml
harness: claude
# or
harness:
  name: claude
  model: opus
  effort: xhigh
```

`model` and `effort` fall through to the agent `model` and `effort` when the job
and its agent use the same harness, then to `tools.<harness>.model|effort` in
`config.yaml` (set with the `config` tool). They are passed as launch flags for
that one session, never written to the vendor's own config file.

`artifacts` lists files of the working folder that Trebi saves as artifact versions at the end of a complete run. A run that saved the name itself with the `artifact` tool keeps its own version. A missing file sets the outcome note and opens an inbox item; the run status does not change.

```yaml
artifacts:
  - path: report.html          # relative to the working folder, required
    name: weekly-sales         # default: the file name without the extension
    title: Weekly sales report # default: the name
```

`inputs` declares the typed values that a job asks for, in form order. Each `{{name}}` in the prompt gets the value. An input has `type` (`string`, `number`, `integer`, or `boolean`), `label`, `help`, `required`, and `default`. A string input can have `enum` (the choices) and `format: multiline`. A number input can have `min` and `max`.

An event trigger can set `types` (the event types it takes), `manual: true` (a person starts the run from a stored event; the event never starts a run by itself), and `inputs` (input name to a fixed value or a `{{ }}` template over the event, such as `{{sender}}` or `{{data.subject}}`). A cron or every trigger takes fixed values only. An input that the trigger does not fill takes the event field of the same name (`data.<name>`), then its default.

```yaml
inputs:
  body: {type: string, label: Email text, format: multiline, required: true}
  tone: {type: string, label: Tone, enum: [friendly, formal, short], default: friendly}
triggers:
  - event: gmail
    types: [mail.received]
    manual: true
  - event: slack
    inputs: {body: '{{text}}', tone: short}
```

## Actions

### list
List jobs. Each row carries `last_run_at`, `last_status`, `next_due_at`,
`mtd_tokens` (`input`, `output`, `cache_read`, `cache_creation`, `billed`,
`cached`), and `triggers` beside the definition fields. `harness` is the harness
that a run uses, and `harness_source` tells where it comes from: `job`, `agent`,
or `default`.
- `workspace` (string, optional): filter by workspace id.
- `q` (string, optional): substring match on the job name.
- `harness` (string, optional): filter by the harness that a run uses.
- `active` (string, optional): `true` keeps active jobs, `false` keeps paused ones.

### show
Read one job definition. `inputs` lists the inputs in form order: `name`, `label`, `help`, `type`, `required`, `default`, `enum`, `format`, `min`, `max`, and `declared` (`false` for a `{{placeholder}}` that the `inputs` block does not declare; it is free text). `folder_env` describes the `.env` file in the working dir: `found`, `enabled`, `path`, `keys`, `skipped` (`line`, `reason`), `error`, and `overridden` (keys that `env` replaces). It never holds a value. `env_file` is the JOB.md key, or null. `harness`, `model`, and `effort` are the values of the job file. `resolved_harness`, `resolved_model`, and `resolved_effort` are the values that a run uses, and `harness_source`, `model_source`, and `effort_source` tell where each comes from: `job`, `agent`, or `default`.
- `name` (string, required): job name.
- `workspace` (string, optional): workspace id.

### create
Create one job. Give `prompt` plus a trigger and the daemon composes the front
matter, or give `content` for full control. Do not write YAML by hand when the
structured fields cover the job. Each job belongs to one agent, so give `agent`.
When the workspace has no agent, make one first with `agent.create`. When it has
more than one, ask the user which agent gets the job.
- `name` (string, required): job name.
- `workspace` (string, required): workspace id. The daemon resolves `""` to no workspace and answers 404.
- `prompt` (string, optional): job body; required when `content` is absent.
- `content` (string, optional): full job definition text with front matter; it wins over every structured field.
- `harness` (string, optional): agent tool that runs the job. Leave it out to use the harness of the agent.
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
- `batch` (string, optional): a duration from `1m` to `24h`, or `off`, with `event`. The trigger collects its events and starts one run per interval with all of them. Not with `replies: continue`.
- `types` (array, optional): event types the trigger takes, with `event`, for example `["mail.received"]`.
- `manual` (boolean, optional): with `event`, a person starts the run from a stored event; the event never starts a run by itself. Not with `batch` or `replies: continue`.
- `event_inputs` (object, optional): with `event`, input name to a fixed value or a `{{ }}` template over the event, for example `{"body": "{{text}}"}`.
- `inputs` (array, optional): the inputs block in form order. Each entry has `name`, `type`, and optional `label`, `help`, `required`, `default`, `enum`, `format`, `min`, `max`.
- `idempotency_key` (string, optional): repeat-safe key; a repeat returns the first job.
- `agent` (string, required): agent short name. The job lands in `agents/<agent>/` and the name becomes the full relative path. A `name` that starts with `agents/<agent>/` can take its place. With neither, the reply is `validation`. An unknown agent returns `not_found`.
- `folder` (string, optional): the folder of the job under its agent, for example `SEO`. One level; a `/` is an error. Use `content` with a `folder:` key instead when you give `content`.

### move
Give a job another owner agent, another folder, or both. Use this action to move a job; never move a job folder on disk by hand, because the index does not follow it. With a new agent, the job folder moves to `agents/<agent>/<last segment>`. The job keeps its ID, its schedules, its state, its memory, and its earlier runs. With the same agent, only the folder changes. The job runs with the settings, the connectors, and the email address of its new agent, so call it first with `dry_run: true` and show `changes` to the user. A live run returns `conflict`; so does an existing target. An unknown agent returns `not_found`. A job at the workspace root returns `validation`. A job outside an agent folder has a parse error and does not run; move it to an agent with this action to fix it. The reply is `{"id", "from", "to", "from_agent", "to_agent", "folder", "moved", "changes"}`; `changes` lists, in plain words, what the new agent changes for the job.
- `name` (string, required): job name.
- `agent` (string, optional): new owner agent short name. Omit it to keep the owner. A job always belongs to an agent, so `""` returns `validation`, and a job with no owner needs this field.
- `folder` (string, optional): folder under the agent, one level. `""` removes it. Omit it to keep the folder when the agent stays; a job that changes agent has no folder unless you give one.
- `dry_run` (boolean, optional): return the reply with `"moved": false` and change nothing.
- `workspace` (string, optional): workspace id.

### validate_text
Check a job definition without writing a job. Call it before `create` when you
compose a definition yourself. The response is `{ok, error, markdown, issues}`, where
`markdown` is the text the daemon would write. `issues` lists `{path, code, severity, message}`; `path` names the key, such as `inputs.tone.default` or `triggers[2].inputs.body`. An `error` issue makes `ok` false. `warnings` repeats the messages of the `warning` issues, such as `{{text}}` in the prompt of a batch trigger.
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
- `batch` (string, optional): a duration from `1m` to `24h`, or `off`, with `event`.
- `types` (array, optional): event types, with `event`.
- `manual` (boolean, optional): a person starts the run, with `event`.
- `event_inputs` (object, optional): input name to a value or a `{{ }}` template, with `event`.
- `inputs` (array, optional): the inputs block in form order.
- `agent` (string, optional): agent short name; the parse uses `agents/<agent>/<name>`. Without it, and with a `name` outside `agents/<agent>/`, the reply has the error that a job must be in an agent folder.
- `folder` (string, optional): the folder of the job under its agent, for example `SEO`. One level; a `/` is an error.
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
- `harness` (string, optional): agent tool that runs the job; empty clears it, and the job uses the harness of its agent.
- `model` (string, optional): job model override; empty clears it.
- `effort` (string, optional): job effort override; empty clears it.
- `title` (string, optional): display title.
- `description` (string, optional): description text.
- `working_dir` (string, optional): run directory.
- `env_file` (boolean or null, optional): `false` stops the load of the `.env` file in the working dir; `true` loads it; `null` removes the key, so the agent value or the default (on) applies.
- `connectors` (list, optional): connector names that carry an `actions` block. A list that is not empty replaces the kit list; `[]` clears it. `kit` is not a patch field; the owner agent holds the kit.
- `followup_prompt` (string, optional): prompt sent after the run.
- `keep_runs` (integer or null, optional): the count of finished runs of this job that keep their details (events, log, and session files). Older runs keep only their summary row. `0` keeps all. `null` removes the key, so the setting `defaults.keep_runs` (default 30) applies.
- `folder` (string, optional): the folder of the job under its agent, for example `SEO`. One level; a `/` is an error. An empty value removes it.
- `inputs` (list or null, optional): replace the inputs block. Each entry has `name`, `type`, and optional `label`, `help`, `required`, `default`, `enum`, `format`, `min`, `max`. `null` or `[]` removes the block.
- `triggers` (list or null, optional): replace the whole trigger list. Each entry uses the JOB.md trigger keys (`cron`, `every`, `event`, `types`, `manual`, `inputs`, `match`, `rooms`, and so on). Read the current list with `show` first. `null` or `[]` removes all triggers.
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
Launch one run of the job now. Trebi fills each input in this order: the `inputs` you give, the `inputs` map of the event trigger, the event field `data.<name>`, the default, then empty. A value that does not fit its input, or a required input with no value, returns 422 `invalid` with `details.fields` (input name to message). A dry run returns `inputs`: input name to `{value, source}`, where `source` is `given`, `map` (a template in the `inputs` map of the trigger), `literal` (a fixed value in that map), `name` (the event field `data.<name>`), `default`, or `empty`.
- `name` (string, required): job name.
- `workspace` (string, optional): workspace id.
- `dry_run` (boolean, optional): evaluate gates and return the plan without launching. Use it before the first real launch of a job this session created or edited.
- `prompt` (string, optional): replace the job body for this run.
- `extra_prompt` (string, optional): append text to the job body for this run.
- `inputs` (object, optional): input name to value. A string value fits a number or boolean input when it parses.
- `event` (string, optional): a stored event id. The run starts from the event, as a person: an event trigger of the job must take the event, else 422 `event_not_accepted`. The run reason is `manual`, and its `trigger` has `event_id`, `on_index`, and `source: "manual"`.
- `on_index` (number, optional): with `event`, the index of the event trigger to use. Default: the first event trigger that takes the event.
- `force` (boolean, optional): with `event`, skip the trigger checks; the event still fills the inputs.
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

### rename
Change the last part of a job name. The job folder moves with `JOB.md`, `MEMORY.md`, and the other files. The job keeps its ID, its owner, its schedules, and its earlier runs; resume of an earlier run uses the new folder. Only the last segment changes: `agents/cmo/weekly` with `to: "digest"` becomes `agents/cmo/digest`. A live run returns `conflict`; so does an existing target. A job at the workspace root returns `validation`. The reply is `{"id", "from", "to"}` with the full old and new names.
- `name` (string, required): job name.
- `to` (string, required): new last segment; lowercase letters, digits, and hyphens, up to 64 characters.
- `workspace` (string, optional): workspace id.

### validate
Check one job definition without running it. The response is `{ok, error, issues, warnings}`, as in `validate_text`.
- `name` (string, required): job name.
- `workspace` (string, optional): workspace id.

### simulate
Test one job against stored events. It never starts a run. For each event, it runs the trigger checks and fills the inputs. The reply is `{summary, results, issues}`. `summary` has `events`, `matched`, `ready`, and `missing` (input name to the count of events that leave it empty). Each row of `results` has `event_id`, `on_index`, `manual`, `gate` (`matched`, `stage`, `error`), `inputs` (name to `{value, source}`), `missing` (required inputs with no value), `asks` (the inputs that the run form asks for when you give no value), `ready` (true when the trigger takes the event and each required input has a value), and `prompt_preview` with `preview`. `issues` is as in `validate`. Give `job` or `content`, not both.
- `workspace` (string, optional): workspace id.
- `job` (string, optional): a saved job name.
- `content` (string, optional): a draft JOB.md text; test it before you save it.
- `event_ids` (array, optional): stored event ids. Without it, the newest events of the connections of the event triggers.
- `since` (string, optional): RFC3339 time or a duration such as `24h`, without `event_ids`.
- `limit` (number, optional): events to test, default 20, at most 50.
- `on_index` (number, optional): the index of the event trigger to use. Default: the first event trigger that takes each event.
- `inputs` (object, optional): input values, as in `run`.
- `preview` (boolean, optional): add `prompt_preview`, the prompt with the inputs filled in.

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

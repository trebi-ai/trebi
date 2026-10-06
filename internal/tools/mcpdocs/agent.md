# agent

Agent operations: create and drive folder-scoped workspace agents.

Call shape: `{"action": "<name>", "input": {…}}`. Errors return the HTTP error envelope `{"error":{"code","message"}}` unchanged, with `isError: true`.

An agent owns the folder `agents/<name>/` inside its workspace. The document
`AGENT.md` carries YAML front matter and the instruction text. A job created
with the `job` `create` action and the `agent` field lands in this folder. The
job belongs to the agent and groups under it.

A wake starts one session on the agent. The daemon composes the prompt from
the agent instructions, the owned jobs, and the agent memory. Workspace jobs
stay out of it.

The agent name uses lowercase letters, digits, and hyphens only. It starts
with a letter or a digit. `create` is the upsert: PUT on a new name creates
the agent. With no `content` and no `prompt`, the daemon seeds a default
document and a memory ledger.

The `harness` field names the agent tool that answers. Set it only to a
harness that the `harness` tool `list` action reports with `auth_ok: true`.

## Actions

### list
List agents across every workspace; filter with `workspace`. Each row carries `job_count`, `jobs`, status, cost, and open work counts.
- `workspace` (string, optional): workspace id or name filter.

### show
Read one agent. The response carries `name`, `index_name`, the document fields, `memory`, `disabled`, `job_count`, and `jobs`.
- `workspace` (string, required): workspace id.
- `name` (string, required): agent short name.

### proposal_status
Read the status of one AGENT.md change proposal. An agent can check only its own proposal; deciding is human-only.
- `proposal_id` (string, required): proposal id from the `202` write response.

### templates
List the embedded agent onboarding templates and their required inputs.

### create
Create one agent. Give `prompt` and the daemon composes the document, or give `content` for full control. With neither, the daemon seeds a default document and a memory ledger.
- `workspace` (string, required): workspace id.
- `name` (string, required): agent short name.
- `prompt` (string, optional): instruction body.
- `content` (string, optional): full AGENT.md text with front matter; it wins over every structured field.
- `harness` (string, optional): agent tool that runs the agent; default `claude`.
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

### set
Replace the document of one agent. For an agent run writing its own document, this returns a pending human proposal instead of writing.
- `workspace` (string, required): workspace id.
- `name` (string, required): agent short name.
- `content` (string, required): full AGENT.md text. An empty body on an existing agent returns `validation`.
- `if_match` (string, optional): current hash from `show`; a stale value returns 412.

### patch
Change front matter fields of one agent. For an agent run patching itself, the rendered result becomes a pending human proposal. Give at least one field. An unknown field returns `validation`.
- `workspace` (string, required): workspace id.
- `name` (string, required): agent short name.
- `disabled` (boolean, optional): stop or restart automatic scheduling.
- `harness` (string, optional): agent tool that runs the agent.
- `title` (string, optional): display title.
- `description` (string, optional): description text.
- `working_dir` (string, optional): run directory.
- `envset` (string or list, optional): environment set name, or a list of names applied in order.
- `connectors` (list, optional): connector names for the agent's own runs. A list that is not empty replaces the kit list; `[]` clears it.
- `followup_prompt` (string, optional): prompt sent after the run.
- `session` (object, optional): session fields `max_duration`, `idle_timeout`, `keep_alive`, `nudge`, `resumable`, `if_running` (`queue` (default), `skip`, or `replace`).
- `kit` (object, optional): the agent kit whose parts every owned run inherits: `envset`, `env`, `skills`, and `connectors` (connector names that carry an `actions` block). Only an agent run that patches itself can send `kit`; the result is a proposal. A direct patch returns `validation`; use `set` to change the kit.
- `if_match` (string, optional): current hash from `show`; a stale value returns 412.

### delete
Remove one agent. Destructive. Without `purge` it removes `AGENT.md` only; the owned jobs stay and keep their owner stamp. With `purge` it removes the whole agent folder, owned jobs included.
- `workspace` (string, required): workspace id.
- `name` (string, required): agent short name.
- `confirm` (boolean, required): ask the user first. Without it the call returns `validation` with `details.confirm_required`.
- `purge` (boolean, optional): remove the whole agent folder with the owned jobs.
- `if_match` (string, optional): current hash from `show`; a stale value returns 412.

### rename
Move `agents/<name>` to `agents/<to>` and rewrite stored run, queue, schedule, conversation, and email references in one transaction. A live agent or owned-job run returns `conflict`; so does an existing target.
- `workspace` (string, required): workspace id.
- `name` (string, required): current agent short name.
- `to` (string, required): new agent short name; lowercase letters, digits, and hyphens, up to 24 characters.

### ask
Send one assistant message to an agent conversation. It injects into a live run or queues a fresh wake.
- `workspace` (string, required): workspace id.
- `name` (string, required): agent short name.
- `text` (string, required): message text.

### escalate
Escalate an agent problem to the operator inbox or the assistant conversation.
- `workspace` (string, required): workspace id.
- `name` (string, required): agent short name.
- `to` (string, required): `operator` or `assistant`.
- `reason` (string, required): the problem and what the agent already tried.
- `run_id` (string, optional): the failed or blocked run this escalation is about.

### wake
Start one agent session. The daemon composes the prompt from the agent instructions, the owned jobs, and the agent memory.
- `name` (string, required): agent short name.
- `workspace` (string, required): workspace id.
- `prompt` (string, optional): full prompt override; it replaces the composed wake prompt.
- `extra_prompt` (string, optional): append text to the composed prompt.
- `dry_run` (boolean, optional): validate only; do not start.
- `inputs` (object, optional): values for `{{placeholder}}` fields in AGENT.md.
- `harness` (string, optional): override the harness for this run.
- `kind` (string, optional): run kind; the daemon pins `agent`.
- `reason` (string, optional): launch reason; defaults to `manual`.
- `conversation_id` (string, optional): attach the run to one conversation. The agent keeps no chat loop.
- `idempotency_key` (string, optional): repeat-safe key; a repeat returns the first run.
- `wait_ms` (number, optional): long-poll until the run ends, up to 120000 ms. The response is the final run row.

### memory_show
Read the structured memory file of one agent. The file lives at `agents/<name>/MEMORY.md`. The response carries `content`, `doc`, `issues`, `legacy`, and `options`. A missing file reads as the empty skeleton.
- `workspace` (string, required): workspace id.
- `name` (string, required): agent short name.

### memory_set
Replace the whole MEMORY.md of one agent. The daemon returns `validation` with the issues unless `force` is true.
- `workspace` (string, required): workspace id.
- `name` (string, required): agent short name.
- `content` (string, required): full file text. Keep the three sections in order: Context, Lessons, Log. Keep records in `trebi state`, not in memory.
- `if_match` (string, optional): current hash from `memory_show`; a stale value returns 412.
- `force` (boolean, optional): write the file even when it has validation issues.

### memory_validate
Check memory text against the file rules. The response is `{valid, issues, legacy}`.
- `workspace` (string, required): workspace id.
- `name` (string, required): agent short name.
- `content` (string, optional): file text to check instead of the stored file.

### memory_set_context
Replace the Context section of agent memory.
- `workspace` (string, required): workspace id.
- `name` (string, required): agent short name.
- `body` (string, required): section body. An explicit `""` clears the section.

### memory_lesson_add
Append one lesson to agent memory. Lessons carry a stamp and are never pruned by age.
- `workspace` (string, required): workspace id.
- `name` (string, required): agent short name.
- `text` (string, required): lesson text.
- `at` (string, optional): entry stamp, `YYYY-MM-DD HH:MM` or `YYYY-MM-DD`; defaults to now.

### memory_lesson_delete
Remove one lesson from agent memory. Destructive.
- `workspace` (string, required): workspace id.
- `name` (string, required): agent short name.
- `entry_id` (string, required): lesson id from `memory_show`.

### memory_add
Append one entry to the Log section of agent memory. The call returns 422 `log_over_cap` when the Log is full.
- `workspace` (string, required): workspace id.
- `name` (string, required): agent short name.
- `text` (string, required): entry text.
- `at` (string, optional): entry stamp, `YYYY-MM-DD HH:MM` or `YYYY-MM-DD`; defaults to now.

### memory_delete
Remove one Log entry from agent memory. Destructive.
- `workspace` (string, required): workspace id.
- `name` (string, required): agent short name.
- `entry_id` (string, required): log entry id from `memory_show`.

### memory_clear
Rewrite agent memory as the empty skeleton. Destructive.
- `workspace` (string, required): workspace id.
- `name` (string, required): agent short name.
- `confirm` (boolean, required): ask the user first. Without it the call returns `validation` with `details.confirm_required`.
- `if_match` (string, optional): current hash from the matching read action; a stale value returns 412.

## Related guides

`agents`, `jobs-and-scheduling`, `getting-started`

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
List the agents of one workspace. Each row carries `job_count` and `jobs`, the owned job names.
- `workspace` (string, required): workspace id.

### show
Read one agent. The response carries `name`, `index_name`, the document fields, `memory`, `disabled`, `job_count`, and `jobs`.
- `workspace` (string, required): workspace id.
- `name` (string, required): agent short name.

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

### set
Replace the document of one agent.
- `workspace` (string, required): workspace id.
- `name` (string, required): agent short name.
- `content` (string, required): full AGENT.md text. An empty body on an existing agent returns `validation`.
- `if_match` (string, optional): current hash from `show`; a stale value returns 412.

### patch
Change front matter fields of one agent. Give at least one field. An unknown field returns `validation`.
- `workspace` (string, required): workspace id.
- `name` (string, required): agent short name.
- `disabled` (boolean, optional): stop or restart automatic scheduling.
- `harness` (string, optional): agent tool that runs the agent.
- `title` (string, optional): display title.
- `description` (string, optional): description text.
- `working_dir` (string, optional): run directory.
- `envset` (string, optional): named environment set.
- `followup_prompt` (string, optional): prompt sent after the run.
- `session` (object, optional): session fields `max_duration`, `idle_timeout`, `keep_alive`, `nudge`, `resumable`, `if_running`.
- `if_match` (string, optional): current hash from `show`; a stale value returns 412.

### delete
Remove one agent. Destructive. Without `purge` it removes `AGENT.md` only; the owned jobs stay and keep their owner stamp. With `purge` it removes the whole agent folder, owned jobs included.
- `workspace` (string, required): workspace id.
- `name` (string, required): agent short name.
- `confirm` (boolean, required): ask the user first. Without it the call returns `validation` with `details.confirm_required`.
- `purge` (boolean, optional): remove the whole agent folder with the owned jobs.
- `if_match` (string, optional): current hash from `show`; a stale value returns 412.

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
Read the memory file of one agent. The file lives at `agents/<name>/MEMORY.md`.
- `workspace` (string, required): workspace id.
- `name` (string, required): agent short name.

### memory_set
Replace the memory file of one agent.
- `workspace` (string, required): workspace id.
- `name` (string, required): agent short name.
- `content` (string, required): full memory text.
- `if_match` (string, optional): current hash from the matching read action; a stale value returns 412.

### memory_add
Append one dated entry to agent memory.
- `workspace` (string, required): workspace id.
- `name` (string, required): agent short name.
- `text` (string, required): entry text.
- `date` (string, optional): entry date, `YYYY-MM-DD`.
- `if_match` (string, optional): current hash from the matching read action; a stale value returns 412.

### memory_delete
Remove one memory entry. Destructive.
- `workspace` (string, required): workspace id.
- `name` (string, required): agent short name.
- `entry_id` (string, required): entry id.
- `if_match` (string, optional): current hash from the matching read action; a stale value returns 412.

### memory_clear
Erase the whole agent memory. Destructive.
- `workspace` (string, required): workspace id.
- `name` (string, required): agent short name.
- `confirm` (boolean, required): ask the user first. Without it the call returns `validation` with `details.confirm_required`.
- `if_match` (string, optional): current hash from the matching read action; a stale value returns 412.

## Related guides

`jobs-and-scheduling`, `getting-started`

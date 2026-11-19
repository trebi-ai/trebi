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

Each agent has a `description`: one line, up to 200 characters, that says
what to ask the agent for. Other agents and people read it to pick the agent
to ask. An AGENT.md with no description has a parse error and does not wake.
The `team` key limits the agents that this agent can ask: `can_ask` (a list of
agent names; absent is every agent of the workspace) and `chains` (false keeps
the agent out of all asks).

The `harness` field names the agent tool that answers. Set it only to a
harness that the `harness` tool `list` action reports with `auth_ok: true`.

## Actions

### list
List agents across every workspace; filter with `workspace`. Each row carries `description`, `team` (`can_ask`, `chains`), `job_count`, `jobs`, status, `tokens_30d` (the last 30 days), `recent_runs` (the last 20 runs, oldest first), `runs_7d`, `ok_7d`, `budget` (USD, month to date), and open work counts.
- `workspace` (string, optional): workspace id or name filter.
- `for` (string, optional): words; keep the agents whose name, title, or description has one of them. Case does not matter.
- `archived` (string, optional): `false` (the default) hides archived agents, `true` shows only archived agents, `all` shows both. Each row carries `archived_at`. The reply carries `counts` (`current`, `archived`).

### show
Read one agent. The response carries `name`, `index_name`, the document fields, `memory`, `disabled`, `job_count`, `jobs`, and `folder_env` (the `.env` file in the working dir, key names only; see `job.show`). `routes` lists the routes that send messages to the agent (not to its jobs): each has `connection`, `id`, `gets`, `use_for`, `filters`, and `problems`. Change a route with `connector.routing_set`.
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
- `description` (string, required unless `content` or `template` gives it): one line, up to 200 characters, that says what to ask this agent for. A missing value returns `validation` with `details.field: description`.
- `title` (string, optional): display title.
- `template` (string, optional): template id from `templates`; the template gives a default description.
- `cron` (string, optional): cron trigger, for example `0 3 * * *`.
- `every` (string, optional): interval trigger, for example `15m`.

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
- `description` (string, optional): one line, up to 200 characters, that says what to ask this agent for.
- `working_dir` (string, optional): run directory.
- `env_file` (boolean or null, optional): `false` stops the load of the `.env` file in the working dir; `true` loads it; `null` removes the key, so `kit.env_file` or the default (on) applies. Owned jobs inherit it like `working_dir`.
- `connectors` (list, optional): connector names for the agent's own runs. A list that is not empty replaces the kit list; `[]` clears it.
- `followup_prompt` (string, optional): prompt sent after the run.
- `keep_runs` (integer or null, optional): the count of finished runs of this agent that keep their details (events, log, and session files). Older runs keep only their summary row. `0` keeps all. `null` removes the key, so the setting `defaults.keep_runs` (default 30) applies.
- `session` (object, optional): session fields `max_duration`, `idle_timeout`, `keep_alive`, `nudge`, `resumable`, `if_running` (`queue` (default), `skip`, or `replace`), `continue` (a duration such as `12h` or `7d`: a message in the same conversation continues the session of the last run as a new turn when that run ended inside this window; empty is off, and a new run gets a recap).
- `kit` (object, optional): the agent kit whose parts every owned run inherits: `env`, `skills`, and `connectors` (connector names that carry an `actions` block). Only an agent run that patches itself can send `kit`; the result is a proposal. A direct patch returns `validation`; use `set` to change the kit.
- `if_match` (string, optional): current hash from `show`; a stale value returns 412.

### delete
Remove one agent and its whole folder. Destructive. Each job belongs to an agent, so an agent with jobs needs a choice: `move_jobs_to` gives the jobs to another agent first, and `purge` deletes them with the agent. With no choice, an agent with jobs returns `conflict` with `details.jobs` (the count); ask the user which choice to use. An agent with no jobs needs no choice. A moved job keeps its ID, runs, schedules, state, memory, and folder. Other files of the agent folder (its skills and memory) go. A move that fails stops the delete; the jobs that moved stay moved. When routes or rules name the agent (or its jobs, with `purge`), `confirm_required` lists them in `details.routes` and `details.rules`, and a confirmed call removes them from routing in the same write. A move keeps the routes of the jobs and points them at the new agent. The reply is `{"deleted", "index_name", "moved": [{"from", "to"}], "purged", "files_removed"}`.
- `workspace` (string, required): workspace id.
- `name` (string, required): agent short name.
- `confirm` (boolean, required): ask the user first. Without it the call returns `validation` with `details.confirm_required`.
- `move_jobs_to` (string, optional): short name of the agent that gets the jobs. Not with `purge`. The agent itself or an unknown agent returns `validation` or `not_found`.
- `purge` (boolean, optional): delete the jobs of the agent with it. Run rows stay.
- `if_match` (string, optional): current hash from `show`; a stale value returns 412.

### rename
Move `agents/<name>` to `agents/<to>` and rewrite stored run, queue, schedule, conversation, and email references in one transaction. Earlier runs resume in the new folder. A live agent or owned-job run returns `conflict`; so does an existing target. The reply is `{"renamed", "from", "to", "from_index", "to_index", "askers", "email_address"}`. `askers` lists the agents whose `team.can_ask` names the old name; the rename does not change them. `email_address` is the new agent address, empty when the workspace has no address. Replies to the old address do not reach the agent.
- `workspace` (string, required): workspace id.
- `name` (string, required): current agent short name.
- `to` (string, required): new agent short name; lowercase letters, digits, and hyphens, up to 24 characters.
- `dry_run` (boolean, optional): return the reply with `"dry_run": true` and `"renamed": false`, and do not move. Use it to show the user the new address and the askers first.

### folder_rename
Move every job of an agent from one folder to another folder. `to: ""` takes the jobs out of the folder. To move one job, use `job.move`. A folder with no job of the agent returns `not_found`. The reply is `{"agent", "from", "to", "jobs"}`; `jobs` is the number of jobs that changed.
- `workspace` (string, required): workspace id.
- `name` (string, required): agent short name.
- `from` (string, required): current folder.
- `to` (string, optional): new folder, one level. `""` or absent removes the folder.

### ask
Ask an agent of the same workspace for an answer. The daemon writes the ask into the conversation of the pair and starts or continues a run of the target. The reply is `{"hop", "next", "message", "request_id", "conversation_id"}`: 201 for a new ask, 202 when a person must first approve more chain work, 200 for a repeat of the same `idempotency_key`. From an agent run, `next` is `end_turn`: end your turn now. The reply of the target starts your next turn. Do not poll. An ask can fail with `chain_cycle` (the target waits for you; reply instead), `chain_limit`, `chain_stopped`, `agent_unavailable`, `forbidden` (`team.can_ask`), or `rate_limited`. Each error has `details` with `refuse_reason`, `hop_id`, and `trace_id`.
- `workspace` (string, required): workspace id.
- `name` (string, required): agent short name. Use `list` with `for` to find the agent.
- `text` (string, required): the question, up to 8 KiB. Name file paths for longer material.
- `context` (string, optional): what the target needs to know, up to 8 KiB.
- `idempotency_key` (string, optional): a repeat with the same key returns the first hop.
- `wait_ms` (number, optional): for a person or the assistant only: wait up to this time (maximum 120000) for the reply. The reply then has `replies`.

### handoff
Give work to an agent of the same workspace. The caller does not wait and gets no reply. The target owns the result. The reply is `{"hop", "message", "conversation_id"}`. The errors are the same as for `ask`.
- `workspace` (string, required): workspace id.
- `name` (string, required): agent short name.
- `text` (string, required): the work, up to 8 KiB.
- `context` (string, optional): what the target needs to know, up to 8 KiB.
- `idempotency_key` (string, optional): a repeat with the same key returns the first hop.

### escalate
Escalate an agent problem to the operator inbox or the assistant conversation.
- `workspace` (string, required): workspace id.
- `name` (string, required): agent short name.
- `to` (string, required): `operator` or `assistant`.
- `reason` (string, required): the problem and what the agent already tried.
- `run_id` (string, optional): the failed or blocked run this escalation is about.

### archive
Archive one agent and, with it, all of its jobs. An archived agent never wakes and takes no ask or handoff: the call returns `archived` (409). Its schedules stop, its waiting queue rows end, and its open inbox items close. A live run finishes. Files, memory, and runs stay. Only a person or the assistant archives an agent; an agent run gets `forbidden`. The reply is `{"name", "index_name", "archived_at", "jobs", "queue_dropped"}`.
- `workspace` (string, required): workspace id.
- `name` (string, required): agent short name.
- `if_match` (string, optional): current hash from the matching read action; a stale value returns 412.

### unarchive
Restore an archived agent. Its jobs run again, except the jobs that carry their own archive. The schedules resume with no catch-up run.
- `workspace` (string, required): workspace id.
- `name` (string, required): agent short name.
- `if_match` (string, optional): current hash from the matching read action; a stale value returns 412.

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

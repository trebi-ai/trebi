# run

Run operations: list, inspect, and drive agent executions of jobs.

Call shape: `{"action": "<name>", "input": {…}}`. Errors return the HTTP error envelope `{"error":{"code","message"}}` unchanged, with `isError: true`.

A launch blocks until the queue accepts the run. Poll `show` for status changes.

A run is one harness session. Resume reopens the same run as a new turn: the run id stays, and `turn_count` goes up. A run between turns shows the status of its last turn. `duration_ms` is the active time of all turns, and the cost is the sum of all turns. `previous_run_id` names the run that a new session follows, for example a rerun.

## Actions

### list
List runs, newest first.
- `status` (string, optional): filter by effective status (a human outcome wins), for example `running`.
- `job` (string, optional): filter by job name.
- `workspace` (string, optional): filter by workspace id.
- `kind` (string, optional): filter by run kind.
- `owner` (string, optional): agent id or name; keep the runs of this agent and the runs of its jobs.
- `harness` (string, optional): filter by harness name.
- `model` (string, optional): filter by model.
- `conversation` (string, optional): filter by conversation id.
- `end_reason` (string, optional): filter by end reason, for example `sentinel`.
- `q` (string, optional): text match on the prompt preview, the job, or the id.
- `since` (string, optional): RFC3339 time, or a duration such as `24h` meaning "that long ago".
- `until` (string, optional): RFC3339 time, or a duration such as `1h`.
- `cursor` (string, optional): pagination cursor from a prior page.
- `limit` (number, optional): page size.

### create
Launch one run now. This action is the canonical launch; `job.run` calls it with the job in the path. To launch a folder agent, set `kind` to `agent` and pass the agent short name as `job`.
- `job` (string, required): job name.
- `workspace` (string, optional): workspace id.
- `kind` (string, optional): run kind; `agent` launches a folder agent.
- `prompt` (string, optional): replace the job body for this run.
- `extra_prompt` (string, optional): append text to the job body for this run.
- `inputs` (object, optional): input name to value, as in `job.run`.
- `event` (string, optional): a stored event id; the run starts from the event, as in `job.run`.
- `on_index` (number, optional): with `event`, the index of the event trigger to use.
- `force` (boolean, optional): with `event`, skip the trigger checks.
- `harness` (string, optional): override the harness for this run.
- `reason` (string, optional): launch reason; defaults to `manual`.
- `conversation_id` (string, optional): attach the run to one conversation.
- `idempotency_key` (string, optional): repeat-safe key; a repeat returns the first run.
- `dry_run` (boolean, optional): evaluate gates and return the plan without launching.
- `wait_ms` (number, optional): long-poll until the run ends, up to 120000 ms. The response is the final run row.

### show
Read one run. `folder_env` lists the key names that the run got from the `.env` file in its working dir, the skipped lines, and `overridden` (keys that a job value replaced). It never holds a value.
- `id` (string, required): run id.
- `wait_ms` (number, optional): long-poll until the run ends, up to 120000 ms. The response is the final run row.

### timeline
Read the event timeline of one run.
- `id` (string, required): run id.

### turns
List the turns of one run, oldest first. The response is `{"turns": [...]}`. Each turn has `n`, `reason`, `status`, `end_reason`, `started_at`, `ended_at`, `duration_ms`, `session_id`, and `tokens`.
- `id` (string, required): run id.

### transcript
Read the agent transcript of one run. Each entry has `run_turn`, the turn that wrote it.
- `id` (string, required): run id.
- `raw` (boolean, optional): return the raw transcript without formatting.
- `max_bytes` (number, optional): cap the body at this size; default 64 KB. A cut body returns `{"content": "…", "truncated": true}`.
A run past the `keep_runs` count of its job has `pruned_at` and no log: this call returns 410 `pruned`. Its row, status, times, and tokens stay.

### logs
Read the log tail of one run.
- `id` (string, required): run id.
- `max_bytes` (number, optional): cap the body at this size; default 64 KB. A cut body returns `{"content": "…", "truncated": true}`.

### kill
Stop one run, or several. Destructive.
- `id` (string, required unless `ids` is given): run id.
- `ids` (array, optional): kill several runs in one call. The reply carries `ok`, `count`, and `failed`, so a partial kill is visible.

### resume
Resume one interrupted run. The run reopens as its next turn.
- `id` (string, required): run id.

### replay
Replay one finished run from its first prompt.
- `id` (string, required): run id.

### retry
Retry one failed run from the point of failure.
- `id` (string, required): run id.

### mark
Override the outcome of a finished run, or clear an override. The machine
status and end reason stay as recorded; display, job health, and the inbox
read the outcome. This starts nothing.
- `id` (string, required): run id.
- `status` (string, optional; required unless `clear` is true): `success` (same as `complete`), `failed`, or `skipped`.
- `note` (string, optional): why the outcome changed.
- `clear` (boolean, optional): drop the override and restore the machine status.

### message
Send one message to a run. A running run gets it in its current turn (`steered`), as a new turn (`prompted`), or after its current turn (`held`). A run that ended or waits on a request continues its session with the message (`resumed`). A request stays open. The reply is `{delivery, run_id, turn}`. A run with no session returns `resume_unsupported`. A run that starts or stops at that moment returns `conflict`.
The text must not contain the run completion sentinel (`TREBI_RUN_COMPLETE...`): only the agent reports that it finished, and a sentinel from you ends the run as complete with the work undone. If a run stalled, read its log and fix the blocker instead.
- `id` (string, required): run id.
- `text` (string, required unless `attachments` is set): message text.
- `attachments` (array, optional): images for the agent. Each item is an upload id from `file` `upload`, or the path of an image file in the run workspace. A path outside the workspace returns `validation`.

### approve
Answer one approval request of a run.
- `id` (string, required): run id.
- `decision` (string, required): `allow` or `deny`.
- `approval_id` (string, optional): the row to answer; defaults to the pending one.

### approvals
List approval rows. The daemon opens a pending row with the question when a run
starts to wait, and the decision closes the same row.
- `state` (string, optional): `pending` or `decided`; both when absent.
- `cursor` (string, optional): pagination cursor.
- `limit` (number, optional): page size.

### stats
Read run counts, cost, and tokens. The response is `{range, totals, mtd,
days}`, with `totals` and `mtd` holding `runs`, `ok`, `failed`, `cost_usd`,
`tokens_billed`, `tokens_cached`, `tokens_input`, `tokens_output`,
`tokens_cache_read`, and `tokens_cache_creation`. `days` is one row per day of
the range, oldest first. Use it to answer "how much did we spend".
- `range` (string, optional): `24h`, `7d`, `30d`, or `mtd`; default `30d`.
- `workspace` (string, optional): filter by workspace id.
- `job` (string, optional): filter by job name.

### activity
Read the runs, tokens, cost, and planned fires of each agent, per time bin. The response is `{from, to, bin_ms, now, group, rows, runs, truncated}`. Each row has `key`, `kind`, `workspace_id`, `bins` (`i`, `done`, `failed`, `waiting`, `running`, `skipped`, `tokens`, `cost_usd`), and `due` (`i`, `n`). A run counts in the bin of its start. Use it to answer "what did my agents do today".
- `range` (string, optional): `today`, `7d`, or `30d`; default `today`. The window ends at the next local midnight.
- `agent` (string, optional): agent id or name filter.
- `workspace` (string, optional): workspace id filter.
- `tz` (string, optional): IANA time zone name, for example `Europe/Berlin`; default is the zone of this machine.

### session_state
Read the live session state table of runs. The response is `{"sessions": {run_id: state}}`. No input fields.

## Related guides

`runs-and-debugging`

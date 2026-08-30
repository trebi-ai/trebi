# run

Run operations: list, inspect, and drive agent executions of jobs.

Call shape: `{"action": "<name>", "input": {…}}`. Errors return the HTTP error envelope `{"error":{"code","message"}}` unchanged, with `isError: true`.

A launch blocks until the queue accepts the run. Poll `show` for status changes.

## Actions

### list
List runs, newest first.
- `status` (string, optional): filter by status, for example `running`.
- `job` (string, optional): filter by job name.
- `workspace` (string, optional): filter by workspace id.
- `kind` (string, optional): filter by run kind.
- `harness` (string, optional): filter by harness name.
- `model` (string, optional): filter by model.
- `cursor` (string, optional): pagination cursor from a prior page.
- `limit` (number, optional): page size.

### create
Launch one run of a job.
- `job` (string, required): job name.
- `workspace` (string, optional): workspace id.
- `prompt` (string, optional): prompt override text.
- `dry_run` (boolean, optional): validate only; do not launch.

### show
Read one run.
- `id` (string, required): run id.

### timeline
Read the event timeline of one run.
- `id` (string, required): run id.

### transcript
Read the agent transcript of one run.
- `id` (string, required): run id.
- `raw` (boolean, optional): return the raw transcript without formatting.

### log
Read the log tail of one run.
- `id` (string, required): run id.

### kill
Stop one run. Destructive.
- `id` (string, required): run id.

### resume
Resume one interrupted run.
- `id` (string, required): run id.

### replay
Replay one finished run from its first prompt.
- `id` (string, required): run id.

### retry
Retry one failed run from the point of failure.
- `id` (string, required): run id.

### message
Send one message into a live run.
- `id` (string, required): run id.
- `text` (string, required): message text.

### approve
Answer one approval request of a run.
- `id` (string, required): run id.
- `decision` (string, required): `allow` or `deny`.

### approvals
List open approval requests.
- `cursor` (string, optional): pagination cursor.
- `limit` (number, optional): page size.

### stats
Read aggregate run statistics. No input fields.

### agents
Read the live agent state table. No input fields.

## Related guides

`runs-and-debugging`

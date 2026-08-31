# job

Job operations: define, inspect, and drive scheduled agent work.

Call shape: `{"action": "<name>", "input": {…}}`. Errors return the HTTP error envelope `{"error":{"code","message"}}` unchanged, with `isError: true`.

A launch blocks until the queue accepts the run. Poll the `run` tool for status.

A job definition carries YAML front matter. The `harness` field names the agent
tool that runs it; it defaults to the daemon `defaults.command`. Set it only to a
harness that the `harness` tool `list` action reports with `auth_ok: true`. A run
on a harness that cannot log in starts and then ends with `session_gone`.

## Actions

### list
List jobs.
- `workspace` (string, optional): filter by workspace id.

### show
Read one job definition.
- `name` (string, required): job name.
- `workspace` (string, optional): workspace id.

### create
Create one job from a full definition.
- `name` (string, required): job name.
- `content` (string, required): job definition text with front matter.
- `workspace` (string, optional): workspace id.

### set
Replace one job definition.
- `name` (string, required): job name.
- `markdown` (string, required): full job definition text.
- `workspace` (string, optional): workspace id.

### patch
Change fields of one job.
- `name` (string, required): job name.
- `patch` (object, required): field changes, for example `{"active": false}`.
- `workspace` (string, optional): workspace id.
- `if_match` (string, optional): current revision; a stale value returns 412.

### delete
Remove one job. Destructive.
- `name` (string, required): job name.
- `workspace` (string, optional): workspace id.
- `purge` (boolean, optional): also remove run history.

### run
Launch one run of the job now.
- `name` (string, required): job name.
- `workspace` (string, optional): workspace id.
- `dry_run` (boolean, optional): evaluate gates and return the plan without launching. Use it before the first real launch of a job this session created or edited.

### pause
Stop automatic scheduling of one job.
- `name` (string, required): job name.
- `workspace` (string, optional): workspace id.

### unpause
Restore automatic scheduling of one job.
- `name` (string, required): job name.
- `workspace` (string, optional): workspace id.

### validate
Check one job definition without running it.
- `name` (string, required): job name.
- `workspace` (string, optional): workspace id.

### schedules
List due times of scheduled jobs.
- `limit` (number, optional): page size.

### memory_show
Read the memory file of one job.
- `name` (string, required): job name.
- `workspace` (string, optional): workspace id.

### memory_set
Replace the memory file of one job.
- `name` (string, required): job name.
- `content` (string, required): full memory text.
- `workspace` (string, optional): workspace id.

### memory_add
Append one dated entry to job memory.
- `name` (string, required): job name.
- `text` (string, required): entry text.
- `date` (string, optional): entry date, `YYYY-MM-DD`.
- `workspace` (string, optional): workspace id.

### memory_remove
Remove one memory entry. Destructive.
- `name` (string, required): job name.
- `entry_id` (string, required): entry id.
- `workspace` (string, optional): workspace id.

### memory_clear
Erase the whole job memory. Destructive.
- `name` (string, required): job name.
- `workspace` (string, optional): workspace id.

## Related guides

`jobs-and-scheduling`, `getting-started`

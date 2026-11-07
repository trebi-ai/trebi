# pause

Pause operations: hold the scheduled and event runs of everything, of one workspace, of one harness, or of one agent and its jobs. A pause does not change the on or off switch of a job, so each job keeps its state when the pause ends. A paused job is not deleted and its triggers stay.

Call shape: `{"action": "<name>", "input": {…}}`. Errors return the HTTP error envelope `{"error":{"code","message"}}` unchanged, with `isError: true`.

What a pause holds:

- A scheduled run that falls in the pause is skipped. With `missed: once`, each job that missed a run runs one time when the pause ends.
- An event run waits in the queue and runs when the pause ends.
- A run that the user starts by hand runs, with a warning. A run that is already live continues. The assistant is never paused.
- When pauses overlap, the pause that ends last holds the job.

Trebi pauses a harness itself when one of its usage windows reaches `pause_at` percent (see `harness.patch`), until the window resets. Such a pause has `source: limit` and `created_by: trebi`. When a person resumes it early, Trebi does not pause that harness again until that reset.

A pause row has `id`, `scope` (`all`, `workspace`, `harness`, or `agent`), `target` (the workspace ID, the harness name, or the agent ID; empty for `all`), `label` (the name to show the user), `until` (RFC 3339, or `null` for "until resumed"), `missed` (`skip` or `once`), `source` (`user` or `limit`), `note`, `created_by`, `created_at`, `ended_at`, `ended_by` (`user` or `expiry`), `skipped_fires` (the number of scheduled runs skipped), `revision`, and `active`.

Jobs and agents in a list have `held_by` (`{pause_id, scope, target, label, until}`) when a pause holds them, and the status `held` when no run is live.

## Actions

### list
List the open pauses, newest first.
- `all` (boolean, optional): also list the ended pauses.

### show
Show one pause.
- `id` (string, required): pause id.

### set
Pause a scope. A scope has one open pause: a second `set` on the same scope changes the open pause and answers 200; a new pause answers 201.
- `scope` (string, required): `all`, `workspace`, `harness`, or `agent`.
- `target` (string, optional): the workspace name or ID, the harness name, or the agent name or ID. Required unless `scope` is `all`.
- `workspace` (string, optional): the workspace of an agent name.
- `until` (string, optional): the end, RFC 3339. `reset` ends at the next usage limit reset of a `harness` pause; it fails when Trebi does not know the reset time. `""` or no end field: until the user resumes.
- `for` (string, optional): the length instead of `until`, such as `90m`, `3h`, `2d`, or `1w`.
- `missed` (string, optional): `skip` (the default) or `once`.
- `note` (string, optional): a note for the user.

### update
Change the end, the missed runs, or the note of an open pause. The scope cannot change. When a person changes the end of a limit pause, the pause becomes theirs (`source: user`). An ended pause answers 409.
- `id` (string, required): pause id.
- `until` (string, optional): as in `set`. `""` removes the end.
- `for` (string, optional): as in `set`, from now.
- `missed` (string, optional): `skip` or `once`.
- `note` (string, optional): the note. `""` clears it.

### resume
End a pause now. The waiting event runs start, and with `missed: once` each job that missed a run runs one time.
- `id` (string, required): pause id.

## Related guides

`jobs-and-scheduling`

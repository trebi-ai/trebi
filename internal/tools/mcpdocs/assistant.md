# assistant

Assistant operations: configure and drive the workspace assistant.

Call shape: `{"action": "<name>", "input": {…}}`. Errors return the HTTP error envelope `{"error":{"code","message"}}` unchanged, with `isError: true`.

The assistant document `TREBI.md` carries YAML front matter. The `harness` field
names the agent tool that answers; it defaults to the daemon `defaults.command`.
Set it only to a harness that the `harness` tool `list` action reports with
`auth_ok: true`. A reply on a harness that cannot log in never arrives: the run
ends with `session_gone`. Rewriting the document with a different harness does
not fix a missing credential.

## Actions

### show
Read the assistant configuration of one workspace.
- `workspace` (string, required): workspace id.

### set
Replace the assistant instructions.
- `workspace` (string, required): workspace id.
- `content` (string, required): full instruction text.
- `if_match` (string, optional): current hash from the matching read action; a stale value returns 412.

### patch
Change fields of the assistant configuration.
- `workspace` (string, required): workspace id.
- `disabled` (boolean, optional): enable or disable the assistant.
- `if_match` (string, optional): current revision; a stale value returns 412.

### delete
Remove the assistant configuration. Destructive.
- `workspace` (string, required): workspace id.
- `if_match` (string, optional): current revision; a stale value returns 412.

### wake
Start one assistant session.
- `workspace` (string, required): workspace id.
- `prompt` (string, optional): full prompt override; it replaces the composed wake prompt.
- `dry_run` (boolean, optional): validate only; do not start.
- `inputs` (object, optional): values for `{{placeholder}}` fields in TREBI.md.
- `harness` (string, optional): override the harness for this run.
- `kind` (string, optional): run kind; the daemon pins `assistant`.
- `reason` (string, optional): launch reason; defaults to `manual`.
- `conversation_id` (string, optional): attach the run to one conversation.
- `idempotency_key` (string, optional): repeat-safe key; a repeat returns the first run.
- `extra_prompt` (string, optional): append text to the assistant prompt.
- `wait_ms` (number, optional): long-poll until the run ends, up to 120000 ms. The response is the final run row.

### say
Send one message to the workspace assistant. The daemon resolves or creates the
channel conversation, so no pointer call is needed first.
- `workspace` (string, required): workspace id.
- `text` (string, required): message text.
- `channel_type` (string, optional): channel kind; defaults to `cli`.
- `channel_ref` (string, optional): channel key; defaults to the workspace id.
- `idempotency_key` (string, optional): repeat-safe key; a repeat returns the first message.
- `wait_ms` (number, optional): long-poll for the assistant reply, up to 120000 ms. `replies` carries what arrived.

### memory_show
Read the structured assistant memory of one workspace. The response carries `content`, `doc`, `issues`, `legacy`, and `options`. A missing file reads as the empty skeleton.
- `workspace` (string, required): workspace id.

### memory_set
Replace the whole assistant MEMORY.md. The daemon returns `validation` with the issues unless `force` is true.
- `workspace` (string, required): workspace id.
- `content` (string, required): full file text. Keep the four sections in order: Context, State, Lessons, Log.
- `if_match` (string, optional): current hash from `memory_show`; a stale value returns 412.
- `force` (boolean, optional): write the file even when it has validation issues.

### memory_validate
Check memory text against the file rules. The response is `{valid, issues, legacy}`.
- `workspace` (string, required): workspace id.
- `content` (string, optional): file text to check instead of the stored file.

### memory_set_context
Replace the Context section of the assistant memory.
- `workspace` (string, required): workspace id.
- `body` (string, required): section body. An explicit `""` clears the section.

### memory_set_state
Replace the State section of the assistant memory.
- `workspace` (string, required): workspace id.
- `body` (string, required): section body. An explicit `""` clears the section.

### memory_lesson_add
Append one lesson to assistant memory. Lessons carry a stamp and are never pruned by age.
- `workspace` (string, required): workspace id.
- `text` (string, required): lesson text.
- `at` (string, optional): entry stamp, `YYYY-MM-DD HH:MM` or `YYYY-MM-DD`; defaults to now.

### memory_lesson_delete
Remove one lesson from assistant memory. Destructive.
- `workspace` (string, required): workspace id.
- `entry_id` (string, required): lesson id from `memory_show`.

### memory_add
Append one entry to the Log section of assistant memory. The call returns 422 `log_over_cap` when the Log is full.
- `workspace` (string, required): workspace id.
- `text` (string, required): entry text.
- `at` (string, optional): entry stamp, `YYYY-MM-DD HH:MM` or `YYYY-MM-DD`; defaults to now.

### memory_delete
Remove one Log entry from assistant memory. Destructive.
- `workspace` (string, required): workspace id.
- `entry_id` (string, required): log entry id from `memory_show`.

### memory_clear
Rewrite the assistant memory as the empty skeleton. Destructive.
- `workspace` (string, required): workspace id.
- `confirm` (boolean, required): ask the user first. Without it the call returns `validation` with `details.confirm_required`.
- `if_match` (string, optional): current hash from the matching read action; a stale value returns 412.

## Related guides

`assistant`

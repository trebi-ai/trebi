# assistant

Assistant operations: configure the assistant and drive its chat sessions.

Call shape: `{"action": "<name>", "input": {…}}`. Errors return the HTTP error envelope `{"error":{"code","message"}}` unchanged, with `isError: true`.

The assistant keeps one agent session per conversation. Global defaults live in the daemon config; a workspace may override any field in its `trebi.yaml`. An absent workspace field inherits the default. `show` with no `workspace` reads the defaults.

## Actions

### show
Read the assistant settings. With `workspace`, the response carries the effective settings, their sources, the override object, and the `trebi.yaml` hash. Without it, the response carries the global defaults and the available harnesses.
- `workspace` (string, optional): workspace id. Omit it for the global defaults.

### set
Write one settings layer. With `workspace`, the input fields become the workspace override and unknown fields are a validation error. Without it, the fields update the global defaults.
- `workspace` (string, optional): workspace id. Omit it to write the defaults.
- `override` (object, optional): a full override object; it replaces the flat fields.
- `name` (string, optional): assistant nickname; the UI and the mail From name use it. Workspace scope only.
- `harness` (string, optional): `native`, an agent CLI harness name, or empty for auto.
- `model` (string, optional): model for the harness; empty means the harness default.
- `effort` (string, optional): `low`, `medium`, or `high`.
- `envset` (string, optional): env set for CLI harnesses.
- `mode` (string, optional): permission mode: `ask`, `auto_edit`, or `auto`.
- `skills` (array of string, optional): skill names under the skills home.
- `context` (string, optional): the context prompt text.
- `disabled` (boolean, optional): disable the assistant in this workspace.
- `if_match` (string, optional, with `workspace`): current `trebi.yaml` hash from `show`; a stale value returns 412.

### preview
Return the composed instructions of a new assistant session: role, workspace, the effective context, and memory.
- `workspace` (string, required): workspace id.

### say
Send one message to the workspace assistant. The daemon resolves or creates the channel conversation and prompts the session, so no pointer call is needed first.
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

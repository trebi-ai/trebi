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
Read the assistant memory of one workspace.
- `workspace` (string, required): workspace id.

### memory_set
Replace the assistant memory.
- `workspace` (string, required): workspace id.
- `content` (string, required): full memory text.
- `if_match` (string, optional): current hash from the matching read action; a stale value returns 412.

### memory_add
Append one dated entry to assistant memory.
- `workspace` (string, required): workspace id.
- `text` (string, required): entry text.
- `date` (string, optional): entry date, `YYYY-MM-DD`.
- `if_match` (string, optional): current hash from the matching read action; a stale value returns 412.

### memory_delete
Remove one memory entry. Destructive.
- `workspace` (string, required): workspace id.
- `entry_id` (string, required): entry id.
- `if_match` (string, optional): current hash from the matching read action; a stale value returns 412.

### memory_clear
Erase the whole assistant memory. Destructive.
- `workspace` (string, required): workspace id.
- `confirm` (boolean, required): ask the user first. Without it the call returns `validation` with `details.confirm_required`.
- `if_match` (string, optional): current hash from the matching read action; a stale value returns 412.

## Related guides

`assistant`

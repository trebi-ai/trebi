# assistant

Assistant operations: configure and drive the workspace assistant.

Call shape: `{"action": "<name>", "input": {…}}`. Errors return the HTTP error envelope `{"error":{"code","message"}}` unchanged, with `isError: true`.

## Actions

### show
Read the assistant configuration of one workspace.
- `workspace` (string, required): workspace id.

### set
Replace the assistant instructions.
- `workspace` (string, required): workspace id.
- `content` (string, required): full instruction text.

### patch
Change fields of the assistant configuration.
- `workspace` (string, required): workspace id.
- `disabled` (boolean, optional): enable or disable the assistant.
- `if_match` (string, optional): current revision; a stale value returns 412.

### remove
Remove the assistant configuration. Destructive.
- `workspace` (string, required): workspace id.
- `if_match` (string, optional): current revision; a stale value returns 412.

### wake
Start one assistant session.
- `workspace` (string, required): workspace id.
- `prompt` (string, optional): extra prompt text.
- `dry_run` (boolean, optional): validate only; do not start.

### say
Send one message in an assistant conversation.
- `conversation` (string, required): conversation id.
- `text` (string, required): message text.
- `workspace` (string, optional): workspace id.

### memory_show
Read the assistant memory of one workspace.
- `workspace` (string, required): workspace id.

### memory_set
Replace the assistant memory.
- `workspace` (string, required): workspace id.
- `content` (string, required): full memory text.

### memory_add
Append one dated entry to assistant memory.
- `workspace` (string, required): workspace id.
- `text` (string, required): entry text.
- `date` (string, optional): entry date, `YYYY-MM-DD`.

### memory_remove
Remove one memory entry. Destructive.
- `workspace` (string, required): workspace id.
- `entry_id` (string, required): entry id.

### memory_clear
Erase the whole assistant memory. Destructive.
- `workspace` (string, required): workspace id.

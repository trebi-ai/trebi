# conversation

Conversation operations: manage assistant conversations and channel pointers.

Call shape: `{"action": "<name>", "input": {…}}`. Errors return the HTTP error envelope `{"error":{"code","message"}}` unchanged, with `isError: true`.

## Actions

### list
List conversations.
- `workspace` (string, optional): filter by workspace id.
- `channel` (string, optional): filter by channel reference.
- `operator` (string, optional): filter by operator; empty is the assistant, `agents/<id>` is an agent.
- `cursor` (string, optional): pagination cursor from a prior page.
- `limit` (number, optional): page size.

### agent_list
List one agent's conversations.
- `workspace` (string, required): workspace id.
- `name` (string, required): agent short name.

### show
Read one conversation.
- `id` (string, required): conversation id.

### messages
List messages of one conversation.
- `id` (string, required): conversation id.
- `cursor` (string, optional): pagination cursor from a prior page.
- `limit` (number, optional): page size.

### create
Create one conversation.
- `workspace` (string, required): workspace id. The service rejects an empty value.
- `title` (string, optional): title text.
- `origin_channel` (string, optional): originating channel reference. Defaults to `mcp`.
- `channel_type` (string, optional, with `channel_ref`): channel type. Both together activate the conversation on that channel.
- `channel_ref` (string, optional, with `channel_type`): channel reference.
- `operator` (string, optional): target operator; short agent id or `agents/<id>`. Empty is the assistant.

### patch
Change one conversation.
- `id` (string, required): conversation id.
- `title` (string, optional): new title.
- `summary` (string, optional): new summary.
- `revision` (number, optional): current revision; a stale value returns 412.

### send
Send one message in a conversation.
- `id` (string, required): conversation id.
- `text` (string, required): message text.
- `channel_type` (string, optional): channel type.
- `channel_ref` (string, optional): channel reference.
- `idempotency_key` (string, optional): deduplication key for retries.

### events
Read the stored event transcript of one assistant conversation. Each event carries `seq`, `type`, `turn`, and the payload of its type (`text`, `tool`, `permission`, `answer`, `usage`, `result`, `error`). Pass the last `seq` as `after` on the next read to get only newer events.
- `id` (string, required): conversation id.
- `after` (number, optional): last seq seen; the reply holds events after it.
- `limit` (number, optional): page size; the daemon caps it.

### interrupt
Interrupt the running turn of an assistant conversation. The call returns 409 `conflict` when no turn is running.
- `id` (string, required): conversation id.

### delete
Delete one conversation with its messages, events, and assistant session row; it closes the session process first. Destructive.
- `id` (string, required): conversation id.
- `confirm` (boolean, required): ask the user first. Without it the call returns `validation` with `details.confirm_required`.

### outbox
List the outbox rows of one conversation. Each row carries `id`, `kind`, `connector`, `target`, `status` (`pending`, `sending`, `sent`, `failed`, or `canceled`), `attempts`, `error`, and `dedupe_key`. The `outbox` tool reads every row.
- `id` (string, required): conversation id.

### outbox_retry
Set one failed or canceled outbox row of a conversation back to `pending` with `attempts = 0`, so the worker retries it. The call returns `conflict` for any other status.
- `id` (string, required): conversation id.
- `row` (string, required): outbox row id.

### pointer_show
Read the conversation pointer of one channel.
- `channel_type` (string, required): channel type.
- `channel_ref` (string, required): channel reference.
- `workspace` (string, required): workspace id. Pointers belong to a workspace.

### pointer_set
Point one channel at a conversation.
- `channel_type` (string, required): channel type.
- `channel_ref` (string, required): channel reference.
- `conversation_id` (string, required): conversation id.

## Related guides

`assistant`

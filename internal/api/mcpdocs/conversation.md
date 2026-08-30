# conversation

Conversation operations: manage assistant conversations and channel pointers.

Call shape: `{"action": "<name>", "input": {…}}`. Errors return the HTTP error envelope `{"error":{"code","message"}}` unchanged, with `isError: true`.

## Actions

### list
List conversations.
- `workspace` (string, optional): filter by workspace id.
- `channel` (string, optional): filter by channel reference.
- `cursor` (string, optional): pagination cursor from a prior page.
- `limit` (number, optional): page size.

### show
Read one conversation.
- `id` (string, required): conversation id.

### messages
List messages of one conversation.
- `id` (string, required): conversation id.
- `cursor` (string, optional): pagination cursor from a prior page.
- `limit` (number, optional): page size.

### new
Create one conversation.
- `workspace` (string, optional): workspace id.
- `title` (string, optional): title text.
- `origin_channel` (string, optional): originating channel reference.
- `channel_type` (string, optional): channel type.
- `channel_ref` (string, optional): channel reference.

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

### pointer_show
Read the conversation pointer of one channel.
- `channel_type` (string, required): channel type.
- `channel_ref` (string, required): channel reference.

### pointer_set
Point one channel at a conversation.
- `channel_type` (string, required): channel type.
- `channel_ref` (string, required): channel reference.
- `conversation` (string, required): conversation id.

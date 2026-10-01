# event

Event operations: inspect the event log and test rules.

Call shape: `{"action": "<name>", "input": {…}}`. Errors return the HTTP error envelope `{"error":{"code","message"}}` unchanged, with `isError: true`.

The event log feeds the rule pipeline. Use the `system` tool action `bus_poll` for the bus stream.

## Actions

### list
List events, newest first. Each row carries `id`, `connector`, `instance`,
`event_type`, `received_at`, `summary`, `trace_id`, `source_id` (the event id
from the connector, used for dedup), `identity` (the sender identity, checked
by a trigger `allowlist`), `room` and `thread` (a reply goes there), `sender`,
and `catch_up` (true when a catch-up replay stored the event).
- `connector` (string, optional): filter by connector name.
- `since` (string, optional): RFC3339 time, or a duration such as `24h` meaning "that long ago".
- `q` (string, optional): text match on the summary or the payload.
- `cursor` (string, optional): pagination cursor from a prior page.
- `limit` (number, optional): page size.

### show
Read one event with the same fields as a `list` row, plus the payload `raw` and the
trigger `outcomes`.
- `id` (string, required): event id.

### retrigger
Run the rule pipeline again for one stored event. It writes no new event row, so it works for every connector, `email` and `webhook` included, and the event id stays the same. Each trigger outcome points at the new queue row.
- `id` (string, required): event id.

### rules_test
Evaluate one event trigger against one event. No rule is recorded. The checks run in trigger order: `rooms`, `allowlist`, `from`, `contains`, `extract`, then `match`. The reply carries `matched`, `stage` (the check that stopped the event: `room`, `sender`, `from`, `contains`, `extract`, or `match`; empty on a match), `follow_up` (always false here: the test treats the event as the first message), `conversation_key`, `vars`, `extracted` (the extract output), `error`, and the `room` and `identity` of the event. Give `event_id` or `event`, not both.
- `match` (string, optional): match expression, for example `kind = 'mail'`. Empty matches every event that passes the other checks.
- `extract` (string, optional): extract expression.
- `rooms` (array, optional): room ids the trigger takes; empty takes every room.
- `allowlist` (array, optional): sender ids the trigger takes; empty takes every sender.
- `contains` (array, optional): words or phrases; the message text must hold one of them.
- `from` (string, optional): `people`, `bots`, or `anyone` (default).
- `replies` (string, optional): `continue` or `off`; `continue` gives the conversation key of a job that continues.
- `event_id` (string, optional): a stored event id; the event payload, room, and sender come from the event log.
- `event` (any, optional): a draft event payload. A `trebi-connector/1` event gives its `room.id` and `sender.id`.
- `connector` (string, optional): connector of a draft `event`; `email` compares senders without case.

### rules_test_batch
Evaluate one event trigger against up to 50 stored events, in order. The reply is `results`: one row per event with `event_id`, `matched`, `stage`, `vars`, `extracted`, and `error`. A missing event gives a row with the error `event not found`.
- `match` (string, optional): match expression.
- `extract` (string, optional): extract expression.
- `rooms` (array, optional): room ids the trigger takes.
- `allowlist` (array, optional): sender ids the trigger takes.
- `contains` (array, optional): words or phrases; the message text must hold one of them.
- `from` (string, optional): `people`, `bots`, or `anyone`.
- `replies` (string, optional): `continue` or `off`.
- `event_ids` (array, required): stored event ids, at most 50.

### rules_fire
Evaluate one rule and fire its actions when it matches.
- `connector` (string, required): connector name.
- `job` (string, optional): job name.
- `match` (string, required): match expression.
- `workspace` (string, optional): workspace id.
- `extract` (string, optional): extract expression.
- `event` (any, optional): event payload.
- `on_index` (number, optional): fire only the action at this index.

## Related guides

`connectors-and-rules`

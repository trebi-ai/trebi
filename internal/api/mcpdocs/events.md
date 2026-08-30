# events

Event operations: inspect the event log and test rules.

Call shape: `{"action": "<name>", "input": {…}}`. Errors return the HTTP error envelope `{"error":{"code","message"}}` unchanged, with `isError: true`.

The event log feeds the rule pipeline. Use the `system` tool action `bus_poll` for the bus stream.

## Actions

### list
List events, newest first.
- `cursor` (string, optional): pagination cursor from a prior page.
- `limit` (number, optional): page size.

### show
Read one event.
- `id` (string, required): event id.

### retrigger
Run the rule pipeline again for one event.
- `id` (string, required): event id.

### rules_test
Evaluate one match expression against an event. No rule is recorded.
- `match` (string, required): match expression, for example `kind = 'mail'`.
- `extract` (string, optional): extract expression.
- `event` (any, optional): event payload.

### rules_fire
Evaluate one rule and fire its actions when it matches.
- `integration` (string, required): integration name.
- `job` (string, optional): job name.
- `match` (string, required): match expression.
- `workspace` (string, optional): workspace id.
- `extract` (string, optional): extract expression.
- `event` (any, optional): event payload.
- `on_index` (number, optional): fire only the action at this index.

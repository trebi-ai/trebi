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
With `after`, the list reads forward: events with an id after it, oldest first, one row per event in the `show` shape. The reply carries `next_after`; give it as `after` on the next call to read new events with no gap and no duplicate. To start at the oldest event, give `after` as `00000000000000000000000000`.
- `after` (string, optional): event id; return the events after it, oldest first. Do not give it with `cursor`.
- `connector` (string, optional): filter by connector name.
- `workspace` (string, optional): only events with a trigger outcome in this workspace.
- `since` (string, optional): RFC3339 time, or a duration such as `24h` meaning "that long ago".
- `q` (string, optional): text match on the summary or the payload.
- `outcome` (string, optional): only events with a trigger outcome of this value, for example `not_chosen` or `route_wait`. `manual` lists the events that a person can start a run with and did not yet.
- `accepts` (string, optional): `<workspace>/<job>`; only events that an event trigger of the job takes.
- `cursor` (string, optional): pagination cursor from a prior page.
- `limit` (number, optional): page size.

### show
Read one event with the same fields as a `list` row, plus the payload `raw` and the
trigger `outcomes`. `routing` lists the stored routing of the conversation of the event, one entry for each workspace: `workspace`, `connection`, `conversation_key`, `status` (`chosen`, `nobody`, `waiting`, or `deciding`), `route_id`, `target`, `target_kind`, `method` (`you`, `conversation`, `only`, `nothing`, `rule`, `router`, `guess`, or `ignored`), `rule_index`, `decision_id`, `share`, `wrong`, and `inbox_id`. Each outcome carries `share` (0 when it has no router decision) and can carry `route_method` (an Inbox Ignore gives `ignored`), `rule_index`, and `decision_id`. `share` is the share that the router gave the route of the outcome. The outcomes `not_chosen` (the route is not the choice of the conversation) and `route_wait` (the conversation waits for the router or for a person) mean that routing is on.
- `id` (string, required): event id.

### jobs
List the jobs that can run with one stored event. The reply is `jobs`: one row per route of a job in any workspace that listens to the connection of the event. Each row has `workspace`, `job`, `title`, `route_id`, `manual`, `matched`, `stage` (the check that stopped the event), `error`, `asks` (the required inputs that the event does not fill), `ready`, `outcome`, and `run_id` (the run that the event started, if any).
- `id` (string, required): event id.

### run
Start one run of a job with a stored event, as a person. It is `job.run` with `event`. A route of the job must take the event, else 422 `event_not_accepted`. Use `dry_run` to see the filled inputs first.
- `id` (string, required): event id.
- `job` (string, required): job name.
- `workspace` (string, optional): workspace id.
- `inputs` (object, optional): input values; they win over the values from the event.
- `route_id` (string, optional): the id of the route to use.
- `force` (boolean, optional): skip the trigger checks.
- `dry_run` (boolean, optional): return the filled inputs and the prompt, and start no run.
- `extra_prompt` (string, optional): append text to the job body for this run.
- `harness` (string, optional): override the harness for this run.

### retrigger
Run the rule pipeline again for one stored event. It writes no new event row, so it works for every connector, `email` and `webhook` included, and the event id stays the same. Each trigger outcome points at the new queue row.
- `id` (string, required): event id.

### reroute
Move the whole conversation of an event to another route, or to nobody. Later replies follow the new choice. It works only when routing is on for the connection. It cancels the queued run of the old route for this event; a run that already started stays. The reply has `route_id`, `target`, `outcome`, `queue_id`, `wrong`, and `feedback`. When the router made a decision for the conversation, a different choice marks it `disagree` (shown as Wrong) and the same choice marks it `agree`. It also closes the Inbox item of the choice. Errors: 404 `route_not_found`, 409 `routing_off`, 409 `ambiguous` (more than one route has this target; use the route id), 422 `event_not_accepted` (the filters of the route stop the event).
- `id` (string, required): event id.
- `to` (string, required): a route id, a target name, or `nobody`.
- `workspace` (string, optional): workspace id. An assistant uses its own workspace.
- `force` (boolean, optional): skip the match filter of the route.
- `ignore` (boolean, optional): with `to: nobody`, run nothing and give the decision no feedback.

### rules_test
Evaluate one event trigger against one event. No rule is recorded. The checks run in trigger order: `types`, `rooms`, `allowlist`, `from`, `contains`, `extract`, `match`, then `inputs`. The reply carries `matched`, `stage` (the check that stopped the event: `type`, `room`, `sender`, `from`, `contains`, `extract`, `match`, or `inputs`; empty on a match), `inputs` and `missing` (with `job`, on a match: the filled inputs, name to `{value, source}`, and the required inputs with no value), `follow_up` (true when the event continues an open conversation of `job`; a follow-up skips `from`, `contains`, and `match`), `conversation_key`, `vars`, `extracted` (the extract output), `error`, and the `room` and `identity` of the event. Give `event_id` or `event`, not both.
- `match` (string, optional): match expression, for example `kind = 'mail'`. Empty matches every event that passes the other checks.
- `extract` (string, optional): extract expression.
- `rooms` (array, optional): room ids the trigger takes; empty takes every room.
- `allowlist` (array, optional): sender ids the trigger takes; empty takes every sender.
- `contains` (array, optional): words or phrases; the message text must hold one of them.
- `from` (string, optional): `people`, `bots`, or `anyone` (default).
- `replies` (string, optional): `continue` or `off`; `continue` gives the conversation key of a job that continues.
- `job` (string, optional): the job of the trigger. A match fills its inputs. With `replies: continue`, an event of an open conversation of this job is a follow-up.
- `route_id` (string, optional): with `job`, the id of the route that fills the inputs. Default: the first route of the job.
- `batch` (string, optional): a duration from `1m` to `24h`, or `off`. A match then waits for the next tick; the reply carries `batch`.
- `workspace` (string, optional): workspace of `job`.
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
- `batch` (string, optional): a duration from `1m` to `24h`, or `off`.
- `job` (string, optional): the job of the trigger, for follow-ups.
- `workspace` (string, optional): workspace of `job`.
- `event_ids` (array, required): stored event ids, at most 50.

### rules_fire
Evaluate one rule and fire its actions when it matches.
- `connector` (string, required): connector name.
- `job` (string, optional): job name.
- `match` (string, required): match expression.
- `workspace` (string, optional): workspace id.
- `extract` (string, optional): extract expression.
- `event` (any, optional): event payload.
- `route_id` (string, optional): the id of the route that the queued run records.

## Related guides

`connectors-and-rules`

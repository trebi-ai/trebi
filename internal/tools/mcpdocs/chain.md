# chain

Chain operations: read the work that agents ask of each other.

Call shape: `{"action": "<name>", "input": {…}}`. Errors return the HTTP error envelope `{"error":{"code","message","details"}}` unchanged, with `isError: true`.

A chain is all the work that one start causes: a person, the assistant, or a run asks an agent, and that agent can ask other agents. One chain has one `trace_id`. Each message between two parties is a hop. A hop has a `kind`: `ask` (the caller waits for a reply), `handoff` (the caller does not wait), or `reply`. The workspace settings `agents.chains` set the limits of a chain: `max_hops` (depth), `max_turns`, and `max_usd`. At a limit, the daemon holds the hop and asks a person in a request: approve raises the limit, reject stops the chain.

## Actions

### list
List chains, newest first. The reply is `{"chains": [...], "next_cursor"}`. Each chain has `status` (`open`, `held`, or `stopped`), `live`, `limits`, and `usage` (`depth`, `turns`, `cost_usd`, `tokens`, `runs`). `tokens` has `input`, `output`, `cache_read`, `cache_creation`, `billed`, and `cached`.
- `workspace` (string, optional): workspace id or name.
- `status` (string, optional): `open`, `held`, or `stopped`.
- `agent` (string, optional): only chains where this agent asks or is asked.
- `live` (boolean, optional): only chains with a queued or running hop.
- `since` (string, optional): RFC3339 time or a duration such as `24h`.
- `cursor` (string, optional): the `next_cursor` of a prior page.
- `limit` (number, optional): page size.

### show
Read one chain. The reply is `{"chain", "hops", "runs"}`. Each run has `agent`, `depth`, and `turns` (the turn numbers in this chain). The hops have no full text; use `hop` for it.
- `trace_id` (string, optional): chain trace id.
- `run_id` (string, optional): a run id instead of `trace_id`. It resolves to the chain of the latest chain turn of the run.

### hop
Read one hop with its full `text` and `context`, and both ends (`from`, `to`) with run and turn.
- `id` (string, required): hop id.

### stop
Stop a chain. Open hops stop, the live runs of the chain end, and each agent that waits for a reply gets one turn to finish with what it has. A later ask in the chain returns `chain_stopped`. The reply is `{"stopped", "killed_runs", "cancelled_hops", "unwinding"}`. An agent run cannot stop a chain.
- `trace_id` (string, required): chain trace id.
- `reason` (string, optional): why the chain stops. The chain keeps it in `stop_reason`.
- `confirm` (boolean, required): ask the user first. Without it the call returns `validation` with `details.confirm_required` and a message that names what stops.

## Related guides

`agents`

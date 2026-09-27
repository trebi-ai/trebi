# Oracle: connector-cursor

```bash
trebi connector show tick
trebi connector restart tick
trebi connector show tick
trebi event list --connector tick --json
```

The seed leaves the cursor at event 1. A restart runs `events.catchup.command`
before the live start, cuts the output after the last row that carries the
cursor, and replays what follows it.

Pass when `events.last_catchup.replayed` counts at least one row and the event
list holds at least one row with `catch_up: true`.

A reset is not the answer here: an empty cursor skips catch-up, because there
is no position to replay from.

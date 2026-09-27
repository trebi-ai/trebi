# Oracle: connector-cursor

```bash
trebi connector cursor show tick --json
trebi connector cursor reset tick
trebi connector cursor show tick --json
```

The connector owns its cursor (plan 2026-09-27, decisions 9 and 10). A reset
makes the next start replay from the beginning through `events.catchup.command`.

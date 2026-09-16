# Claude fixture generator (dev/CI only)

Records real `claude` stream-json NDJSON sessions into
`internal/runtime/cli/testdata/claude/<sdk>-<cli>/` so the Go driver is tested
against actual wire frames, not invented ones (plan Part L.1).

- `generate.mjs` spawns the operator's `claude` exactly like the Go driver
  (same argv, same control protocol) and records the raw NDJSON. Scenario
  frames are sanitized: cwd → `/tmp/ws`, session ids → a stable fixture UUID,
  and the giant `system/init` tool list is trimmed to its protocol fields.
  Everything else is byte-for-byte CLI output.
- `conformance.mjs` runs the same prompt through the official TypeScript
  Agent SDK as an optional oracle. It is never a Trebi runtime dependency.

This directory is a build/test-time dependency only: Trebi never ships or
spawns Node (plan Part P).

```sh
npm ci
node generate.mjs                 # all scenarios (spends a little quota)
node generate.mjs basic           # one scenario
node conformance.mjs              # optional SDK oracle
```

Requirements: `claude` on PATH with a working shared login. The script never
reads, copies, or intermediates vendor tokens; a missing login fails the run
and is fixed with `trebi harness auth login claude` (or `claude auth login`).

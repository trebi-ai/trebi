# Screen fixtures

Frame sequences of `tmux capture-pane -p` output at known lifecycle moments,
captured once on a dev machine and replayed offline by
`tool_fixtures_test.go` — they drive `Tool.IsReady`, `Tool.HandleStartup`,
`Tool.ParseSessionID`, and the `waitReadyLoop` state machine.

## Layout

```
screens/
  <tool>/                 # claude, grok, opencode, codex, copilot, gemini, cursor, pi
    <scenario>/           # startup, idle, resume-picker, mid-session, ...
      01-boot.txt         # numbered frames in pane order: <NN>-<state>.txt
      02-ready.txt
      info.txt            # cli version, date, geometry, exact command
  _capture/               # raw capture-tui output, pruned before promotion
```

A single-frame fixture is just a one-frame scenario.

Session ids never appear on any captured screen, so there is nothing to
assert about them here. Each tool has its own channel: claude/grok/pi get a
`--session-id` launch flag, opencode delivers the id in hook payloads
(persisted by the daemon), and codex/copilot/gemini keep ids on disk only —
their `PrepareResume` fails loudly.

## State vocabulary (filename = expectation)

| State | Asserts |
|-------|---------|
| `boot` | not ready; no actions |
| `trust-folder` | not ready; `{Key: "enter"}` |
| `bypass-permissions` | not ready; `{Text: "2"}` |
| `hooks-trust` | not ready; per-tool expectation (see `hooksTrustActions`) |
| `setup` | not ready; per-tool expectation (see `setupActions`) — first-run offers that are neither trust nor bypass, e.g. copilot's VS Code keybinding dialog |
| `picker` | not ready; no actions |
| `ready` | **ready**; no actions |
| `busy` | classifier: busy (mid-session, hook-less tools only) |
| `idle` | classifier: idle (mid-session, hook-less tools only) |

An unrecognized state name fails the test. `busy`/`idle` frames are skipped by
the readiness walker (the pane there contains arbitrary model output) and
assert the coarse screen classifier instead.

## Capturing

```
./scripts/capture-tui <tool> <scenario> [--max-secs 120] -- <command...>
```

- Fixed 120x40 geometry — wrapping is part of the fixture.
- Runs in a temp dir with `~/.jobi/bin` stripped from PATH: fixtures reflect
  the stock TUI, not the jobi shim (hooks are Track B).
- A new frame is written whenever the pane content changes; interact with the
  TUI normally (`tmux attach -t <session>`) — splash, dialogs, and ready all
  land in one recording.
- Afterwards: prune mid-render/duplicate frames, rename survivors to
  `<NN>-<state>.txt`, review for secrets, move the directory here.

## Rules

- Frames stay byte-exact as captured — no normalization. `capture-pane -p`
  output has no ANSI escapes; box-drawing characters are literal text and the
  substring predicates handle them.
- claude's `bypass-permissions` dialog only appears before the first global
  accept, which this capture machine has already done — the dialog predicate
  stays covered by the synthetic case in `tool_test.go` until someone captures
  it from a fresh profile.
- When a CLI update breaks tests, re-capture that tool's scenarios together —
  never loosen a predicate without new goldens. `info.txt` records versions.
- Capture first-run dialogs from a fresh temp dir/profile when possible.
- Synthetic cases stay in `tool_test.go`; fixtures are for realism.

## Live smoke (`-tags live_tui`)

`go test -tags live_tui -run TestLiveTUISmoke ./internal/tmux/` launches every
harness found on `PATH` in a throwaway tmux session and requires ready within
60s (startup dialogs included); missing binaries skip. Not part of default
`go test ./...` — the tag keeps it out of CI.

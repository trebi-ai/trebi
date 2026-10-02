Fake wire profiles for `TestHarnessResumeProfiles`. Each folder holds `start.sh` (turn 1) and `resume.sh` (the reopened turn) for `agentwire.FakeDriver`. A profile without `resume.sh` returns `ErrResumeUnsupported` on resume.

- `claude-same-id`: the resume keeps the session id, and each result has usage.
- `codex-adopt-id`: the first turn reports its id only on the result, and the resume reports the same thread id.
- `fork-id`: the resume reports a new session id.
- `no-resume`: the harness cannot resume.

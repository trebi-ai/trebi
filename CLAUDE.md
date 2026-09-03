# CLAUDE.md

Instructions for humans and coding agents working in this repository.

Prefer types that own behavior. Prefer fewer, denser files. Prefer explicit wiring over package-level state. Do not invent `util` packages, one-function files, or interfaces that have a single implementation.

## Repository structure

Trebi is one binary (`trebi`) that is both the daemon (`trebi service daemon`) and the CLI. The design contract is `../docs/trebi/`: read `backend-0808.md` first (objects, data model, API, event bus), then `backend.md` (runtime layer, scheduler, logging, CLI parity, cloudlink) and `backend-review-0809.md` (open action items). `backend-0808.md` supersedes `backend.md` §4.3 and §4.5.

```text
cmd/trebi/           # thin main: cli.NewApp().Run(os.Args)
cmd/trebi-eval/      # dev-only eval runner; built by scripts/eval.py, never shipped
internal/api/        # /v1 HTTP + SSE server + /mcp MCP endpoint; one file per route area
internal/cli/        # the trebi command surface; a thin API client
internal/daemon/     # ControlPlane: background workers (queue, schedule, watch, prune)
internal/eval/       # live LLM-driven CLI eval harness
internal/guides/     # embedded workflow cheat sheets (leaf; go:embed)
internal/request/    # pure /v1 request builders (HTTPRequest); no I/O
internal/store/      # SQLite open, migrations, event bus, event-name registry
internal/<domain>/   # one service per object family (runs, workspaces, alerts, …)
internal/tmux/       # harness drivers: one file per harness tool
internal/tray/       # macOS menu bar (build-tagged)
evals/               # tasks/, seeds/, oracles/, rubrics/, baseline.json
scripts/ testdata/   # build helpers, jobi fixtures, eval.py
```

Domain packages own one object family and are named after it: `runs`, `workspaces`, `alerts`, `skills`, `integrations`, `schedule`, `dispatch`, `job`, `secrets`, `harness`, `config`, `home`, `pricing`, `platform`. Each exports a `Service` (or an equivalent concrete type) built by `New(st *store.Store)`.

Dependency direction is one way:

```text
cli ──▶ api ──▶ <domain services> ──▶ store
daemon ──▶ <domain services> ──▶ store
cli ──▶ request ◀── api
cli ──▶ guides ◀── api
```

- `api` and `daemon` are peers. Both wire the same services; neither imports the other.
- `request` is a leaf: pure `/v1` request builders, no I/O. Both `cli` and `api` (MCP tools) import it. It imports nothing from this repo.
- `guides` is a leaf: embedded workflow markdown. Both `cli` (`trebi guide`, offline) and `api` (routes + MCP) import it. It imports nothing from this repo.
- Domain services never import `api`, `cli`, or `daemon`.
- `store` imports no domain package.
- `home` owns every `~/.trebi` path. Do not build paths with `filepath.Join(os.UserHomeDir(), ".trebi", …)` anywhere else.

When you add an object, add the whole vertical in one change: store table + migration, domain service, bus topic in the registry, API routes, CLI command, and tests. A route with no CLI command is an incomplete change (see the parity rule below). A new workflow area ships its guide (or extends one) alongside the route, command, skill entry, and MCP action.

## Types, methods, functions

- If several functions share the same first argument of type `T` / `*T`, they belong on `T` as methods.
- If the shared first argument is an interface (e.g. `Connection`), do not force a method. Put shared behavior on the embedded base struct (e.g. `BaseConn`), or keep the function package-level.
- Put related state on a struct. Inject dependencies in `NewT(...)`.
- Unexported fields by default. Export a field only when it is part of the API and needs no invariant.
- Consistent receivers per type: all pointer or all value. Use a pointer if the method mutates, contains a mutex, or the struct is not tiny.
- Package-level functions are OK for:
  - `NewT` / `MustT` constructors
  - pure conversions that do not belong to one type
  - orchestration across two or more types
- Do not extract a one-off function unless it is reused or it names a real concept. Duplicate-then-extract only after the second use.

```go
// avoid
func ValidateUser(u *User) error { ... }
func SaveUser(db *sql.DB, u *User) error { ... }

// prefer
type UserStore struct{ db *sql.DB }

func NewUserStore(db *sql.DB) *UserStore {
    return &UserStore{db: db}
}

func (s *UserStore) Save(ctx context.Context, u User) error { ... }
func (u User) Validate() error { ... }
```

## Files

Prefer fewer, denser files. One file per type or per topic, not one file per function.

- A new function goes in the existing file for that type or topic.
- Do not create a new file unless you are introducing a new type, a new backend/connector, or a second named concern that does not belong in the current file.
- Forbidden: `helpers.go`, `utils.go`, `misc.go`, and files named after a single function (`get_user.go`, `validate.go`).
- HTTP: construction and `Listen` live in `server.go`; handlers are split by route area in `routes_<area>.go`, with each file's route table at its top (see Backend design patterns → API). Keep route registration next to the handlers.
- Tests: `foo_test.go` next to `foo.go`. Do not make a new test file per test function.
- Same type, same file. `T`, `NewT`, and methods on `T` stay together.
- A second implementation (`PostgresStore`, `MemoryStore`) gets its own file, not its own package, until it has real weight.

Line count is a weak signal. Use seams:

1. Same actor, same file (all `Server` HTTP methods together).
2. Split on a named concern: lifecycle vs protocol vs persistence; public API vs admin; one connector vs another.
3. Soft cap ~800–1200 lines is a prompt to look for a seam. If you cannot name one, leave the file alone.
4. Constrain function size (`funlen` / `gocyclo`), not file size. A large file of small methods is fine.

If a change would add a file under ~80 lines that is not a test, generated output, or a new type, fold it back into the existing topic file.

## Interfaces

- Accept interfaces, return concrete structs.
- Factories that select an implementation may return an interface (e.g. `NewConn` returns `Connection`).
- Keep interfaces small (often one method) and defined by the consumer, not the implementor.
- Do not define an interface “for mocking.” Introduce a seam only where a test or a second implementation already needs one.
- Verify implementations at compile time when useful:

```go
var _ Connection = (*SQLConn)(nil)
```

## Constructors, wiring, state

- `main` / `cmd/...` owns process wiring: config, clients, stores, servers. Domain packages do not open connections in `init()`.
- No package-level mutable state (`var db *sql.DB`, `var defaultClient`).
- Allowed package-level vars:
  - sentinel errors (`var ErrNotFound = errors.New("not found")`)
  - `regexp.MustCompile`
  - `//go:embed` contents
  - `version`
- Anything else that must stay package-level needs `//nolint:gochecknoglobals` and a reason on the same line.
- No `init()` unless a plugin/registry architecture truly requires it. Prefer an explicit `Register` called from `main` or the owning package.
- Config is a struct loaded once at startup and injected. Do not reread env vars deep in the call stack.
- `NewT` returns `*T` or `(T, error)`. `MustT` panics; use only from `main` or tests.

## Context, errors, concurrency

- `context.Context` is the first argument of any function that does I/O, waits, or calls something that does.
- Do not store request-scoped `Context` on a struct. The exception is a process-lifetime context cancelled on shutdown.
- Wrap errors with useful context:

```go
return fmt.Errorf("save user %s: %w", id, err)
```

- Sentinel errors at package level; compare with `errors.Is` / `errors.As`.
- Return `error` as the last result.
- Do not panic in library code except `Must*` constructors used at process init.
- Do not ignore errors with `_ =` unless the reason is on that line.
- No goroutine without a documented stop condition (cancel, `WaitGroup`, or `errgroup`). The starter owns lifetime.

## API and naming

- MixedCaps. No `GetUser` for a field read; use `User()` only when the field should stay unexported and the accessor is the API.
- Prefer `users, err := store.List(ctx)` over boolean or status out-params.
- Do not stutter: `user.User` is fine; `user.UserUser` is not. Package name plus type name should read well at the call site.
- Exported identifiers need a comment only when the name does not already say what it is. Do not write `// UserStore stores users`.

## Tests

- Table-driven tests next to the code they cover.
- Use `t.Parallel()` when the test has no shared mutable state.
- Prefer fakes injected via constructors over monkey-patching or package-level setters.
- Assert behavior, not log text, unless the log line is the API.
- External test package (`package foo_test`) only when testing the public API as a client.
- Race-detect packages that start goroutines: `go test -race ./...`.

## Evals

Live LLM-driven CLI evals: skill → client LLM (`claude` / `codex`) → `trebi` CLI → `/v1` → a per-trial daemon. Grade outcome state, not wording. Always go through `scripts/eval.py` (uv script; builds `trebi` + `trebi-eval`, tees timestamped `out/eval-live.log`, sweeps leftover trial daemons on exit). `scripts/eval.sh` is the legacy wrapper, left for in-flight runs.

```bash
# full suite, one client (25 tasks × 3 trials)
scripts/eval.py run --client codex
scripts/eval.py run --client claude
scripts/eval.py run --client codex --surface mcp
scripts/eval.py run --surface both --parallel 4
scripts/eval.py run --parallel 1        # serial
scripts/eval.py run --client codex --judge codex
scripts/eval.py run --judge codex:gpt-5

# smoke one task
scripts/eval.py run --client codex --trials 1 --task onboarding-health

# one tier
scripts/eval.py run --client codex --tier debug

# default client via env if you omit --client
EVAL_CLIENT=codex scripts/eval.py run --trials 1 --task onboarding-health

# replay the reference script (no LLM); a task without a green oracle is broken
scripts/eval.py oracle onboarding-health

# sample judge verdicts for human labels
scripts/eval.py review --run out/<runid>

# analyse a finished (or partial) run — writes analysis.json / analysis.md
scripts/eval.py ls
scripts/eval.py report                  # latest out/<runid>
scripts/eval.py report 20260829T184851Z --fail-only
scripts/eval.py why --task not-found-honesty --commands
scripts/eval.py compare RUN_A RUN_B
```

`--task`, `--tier`, `--client` (repeatable), `--judge` (`claude` default, or `codex` / `name:model`), `--trials`, `--budget` (abort at cumulative `cost_usd`), `--baseline`, `--surface` (`cli` default, `mcp`, or `both`), `--parallel` (trials in flight, default 2, or `EVAL_PARALLEL`; use `--parallel 1` for serial). Trials are hermetic, so the pool spans tasks, clients, surfaces, and trials at once. `--no-build` skips the Go rebuild. `EVAL_JUDGE` fills `--judge` when the flag is omitted, same as `EVAL_CLIENT`. Server-side jobs default to harness `codex` (the one that probes `ok` on this Mac). Each trial is isolated with `TREBI_HOME` under `out/<runid>/`. MCP trials also get a scratch client home: credentials are copied in (`auth.json` for codex; `.claude.json` onboarding seed for claude); model selection (`model`, `model_provider`, `[model_providers.*]`) is merged from the real `config.toml` so mcp and cli evaluate the same model; skills, `AGENTS.md`, and extra MCP servers stay out. After a scratch codex launch, a newer `auth.json` `last_refresh` is written back so token rotation is not discarded. The MCP isolation tripwire flags `trebi` or `curl` at command position only, not in echoed prose. Before the first trial the runner preflights each (client, surface) and aborts on model 401 or a missing `mcp__trebi*` handshake. A client that produces zero assistant steps is `infra-failure` (no checks, no judge). Checks that are already green on seed state also abort the trial as infra so vacuous passes cannot land. An oracle replay skips every `commands_jq` check, so a task whose checks are all `commands_jq` passes its oracle on script exit alone; its `commands_jq` is proven instead by the fixture under `evals/fixtures/<id>.<surface>.json`. Ctrl-C cancels the suite, writes a partial `report.md` / `report.json`, and kills trial daemons. Do not `exec` `trebi-eval` or redirect its stdout without the wrapper: Go fully buffers piped stdout, and a killed runner used to leak `trebi service daemon --listen 127.0.0.1:0`.

## Logging and observability

Do not stamp a process-wide field in `main`. A key that appears on every line cannot be filtered. Each subservice sets `log` in `NewT` with `slog.Default().With(...)`. Do not pass a logger into the constructor.

```go
func NewScheduler() *Scheduler {
    return &Scheduler{log: slog.Default().With("logger", "scheduler")}
}

func NewHTTPServer() *HTTPServer {
    return &HTTPServer{log: slog.Default().With("logger", "http_server")}
}

func NewNATS() *NATS {
    return &NATS{log: slog.Default().With("logger", "nats")}
}

func NewNotifier(kind string) *Notifier {
    return &Notifier{log: slog.Default().With("logger", "notifier", "notifier", kind)}
}

func NewJob(id, kind string) *Job {
    return &Job{log: slog.Default().With("logger", "job", "job_id", id, "job_kind", kind)}
}
```

```go
sched := NewScheduler()
httpSrv := NewHTTPServer()
note := NewNotifier("slack")

sched.log.Info("tick")                      // source=scheduler
httpSrv.log.Info("listening", "addr", addr) // source=http_server
note.log.Info("sent")                       // source=notifier notifier=slack
```

One-shot facts (`addr`) stay on that event. Request/step ids go on a child logger; they do not replace `source`:

```go
func (s *HTTPServer) handleGet(w http.ResponseWriter, r *http.Request) {
    log := s.log.With("req_id", reqID(r), "method", r.Method, "path", r.URL.Path)
    log.InfoContext(r.Context(), "request started")
}
```

Keys: `source` (subservice; one snake_case word), plus instance ids (`job_id`, `job_kind`, `req_id`, `conn_id`, `notifier`). Use `err` on failures.

- No `log.Printf`, no package-level logger, no `fmt.Println` for ops.
- Libraries return errors. Log at the edge (handler, `Run`, worker).
- Prefer `InfoContext` / `ErrorContext` when a `ctx` is in hand.
- Do not store request `context.Context` on the struct.
- Configure the handler once in `main` with `slog.SetDefault` if you  want JSON or a non-default level.

## Backend design patterns

These patterns are the contract, not a preference. Keep a new feature inside them; if you must break one, say why in the change.

### The four invariants

1. **API-first, single writer.** Every mutation goes through the `trebid` HTTP API. The CLI, tray, and UI are clients of the same routes. Do not add a second write path that reaches the store or the filesystem behind the daemon.
2. **Everything injected, nothing global.** One `Store`, built in `main`, passed to every service. No package-level DB handle, no function-pointer seams, no mutable config global. `home` holds the only allowed override (`TREBI_HOME`, for tests).
3. **`context.Context` flows through every operation.** Any function that does I/O, waits, or launches a run takes `ctx` first.
4. **Typed edges.** Statuses, reasons, outcomes, alert kinds, and bus topics are closed enums declared once. Do not compare error strings and do not spell an enum value inline.

### Services

One service per object family. Same shape every time:

```go
// Package runs owns run CRUD, timeline, launch path, and agent state.
package runs

type Service struct {
    st  *store.Store
    log *slog.Logger
}

func New(st *store.Store) *Service {
    return &Service{st: st, log: slog.Default().With("source", "runs")}
}
```

- The wire struct (`Workspace`, `Run`) lives in the service package with its `json` tags. The API package does not redeclare it.
- List methods take a `ListOpts` struct, not eight positional arguments.
- The service returns typed errors (`sql.ErrNoRows`, package sentinels). The HTTP layer maps them to status codes.
- Long-running work belongs to `daemon.ControlPlane`, not to the service constructor. A service never starts a goroutine in `New`.

### API

`internal/api` is a thin, dumb layer: decode, validate, call a service, write JSON. Business rules live in the service.

- **One file per route area**, named `routes_<area>.go`, each exporting one `func (s *Server) route<Area>(mux *http.ServeMux)` that lists its routes at the top of the file. `Server.Handler()` in `server.go` composes them. Add a route to the existing area file; do not add a file per handler. Keep registration next to the handlers.
- Use Go 1.22 method-and-pattern routes (`"POST /v1/runs/{id}/kill"`) and `r.PathValue("id")`. No router library.
- Route shape: plural nouns; non-CRUD actions are `POST /{resource}/{id}/{verb}`; sub-resources nest one level only.
- Every response goes through `writeJSON` / `writeErr`. The error envelope is `{"error":{"code","message","details"}}` with a stable machine code (`not_found`, `conflict`, `ambiguous`, `validation`, `draining`, `precondition_failed`, `unauthorized`, `internal`). Never write a bare string body.
- Handlers stay short. Pull request parsing and option building into small helpers in the same file (`runSpec`, `launchOpts`), not into a `helpers.go`.
- **Cross-cutting concerns are middleware**, wrapped once in `Handler()`: bearer auth, then audit. A handler never checks the token itself.
- **File-backed documents** (`JOB.md`, `TREBI.md`, `MEMORY.md`): `PUT` replaces, `PATCH` patches, both through `ifMatchOK`. An absent `If-Match` proceeds; a stale one returns 412 `precondition_failed`.
- **DB-backed objects** carry a monotonic `revision`, bumped on every write and carried on the bus frame.
- **List routes** are cursor-paginated (`?limit=&cursor=`). ULIDs double as cursors; a malformed cursor is a 400, not a silent full scan.
- **Drain** blocks automatic producers and returns 503 `draining`. Manual launches still pass.
- The daemon owns the shared service instances; `BindControlPlane` swaps them into the server. Do not let the API build a second `runs.Service` at request time.
- Times are RFC3339 UTC. Durations use an `_ms` suffix. Keys are snake_case. The server computes derived fields (`duration_ms`, `cost_usd`, `billed`); clients never re-derive money or duration.

### Event bus

One append-only log in `event_log`, one registry, one wire frame.

- `store.AllEvents()` is the single source of truth for every event name. **Add the name there first.** `Bus: true` means it also rides `/v1/stream`; `Log: false` means ops-log only. `AppendInTx` rejects an unregistered topic, so an unlisted name fails at runtime.
- The same name is used on the bus, in the ops log, and in the audit stream. Do not invent a second spelling for one event.
- Write the entity change and the bus row **in one transaction** via `store.AppendEntity`. A crash must not advance an entity past what the bus recorded.
- Bus appends are serialized (`LockBus`) so `seq` is gapless in commit order. Keep that rule for any future Postgres port; do not append outside `Append`/`AppendEntity`.
- **Frames are thin.** Entity events carry `entity`, `id`, `action`, `revision` — never the payload. The client refetches. Domain events may carry a small `attrs` object.
- **Every mutation path emits.** An API write and a file-watcher reload both emit the same `job.updated`; a client cannot tell them apart and must not need to.
- `/v1/stream` is the bus. `/v1/events` is the trigger pipeline. Never conflate the two names.
- Three cursors, three jobs — always name the subject: `event_log.seq` (bus cursor), `integration_cursors.last_event_id` (integration cursor), `schedules.next_due_at`. The bare word `cursor` is banned in identifiers.
- Retention is a single 7-day prune. Anything that must survive longer reads from a durable table, not from the bus.

### CLI

The CLI is a thin API client. It holds no business logic and never touches the store or spawns a session.

- **Parity rule: the CLI covers 100% of the control-plane API.** A new route ships with its `trebi` command, its skill entry, and its MCP action (or `mcpExcluded` entry in `internal/api/mcp_parity_test.go`) in the same change. The assistant drives trebi through this CLI, so an endpoint with no command is unreachable.
- Command tree: one file per command *area*, named `commands_<area>.go`, registered in `NewApp()` in `app.go`. Tiny sibling commands may live in the larger sibling's file (`queue`+`events` in `commands_integration.go`, `alerts` in `commands_inbox.go`). Split subcommands into named groups (`jobManageSubcommands`) when a list grows past a screen; do not split into more files.
- Startup context is built once in `Before` and stored as `appState` in `App.Metadata`. Actions read it through `appStateFor(c)` / `clientFor(c)` / `workspaceFor(c)`. Do not reload config, sticky context, or the API client inside an action.
- Request building is pure and separately testable: `Build*` returns an `HTTPRequest{Method, Path, Body}`, and `client.DoHTTP` executes it. Test the shape without a daemon.
- Client resolution order is fixed: linked remote workspace → remote sticky context → `TREBI_URL`/`TREBI_TOKEN` → local `~/.trebi/api.json`. Remote is workspace context, not a `--remote` flag.
- Local-only commands (`service install|daemon`) fail fast when the sticky context is remote.
- Every read command takes `--json` and passes the API payload through unchanged. Human output uses the shared `newTable`; do not hand-format columns.
- The offline subset must work with the daemon down: `service install|start|stop|status`, `doctor`, `config validate`, `guide`. Everything else fails with a clear "daemon not running" message.

### Store

- `internal/store` owns `Open`, migrations, the bus, and low-level scan helpers. Domain services own their own SQL.
- SQL stays Postgres-portable: no SQLite-only syntax. Placeholders are `?`.
- Migrations are the ordered slice in `schema.go` (index i = version i+1) with `SchemaVersion` at the head. Append only. Never edit an applied migration.
- Wrap multi-statement writes in `WithTx`. Use `AppendEntity` whenever the write has a bus topic.
- Map `""` to SQL NULL with `store.NullStr`. Keep one copy of that helper.

### Daemon workers

- `ControlPlane` owns every background goroutine. Each is started in `Start(ctx)` under the `WaitGroup` and stops on `ctx.Done()`; `Stop` cancels and waits. No goroutine without a stop condition.
- `Options.Shadow` skips producers (integrations, schedule fire, queue drain). Keep new workers honest about shadow and drain.
- Tick intervals are `Options` fields with defaults, so tests can drive them fast. Do not hard-code a sleep in a worker.

### Harness drivers

- A harness is a `tmux.Tool` implementation, one file per tool (`tool_claude.go`, `tool_codex.go`). Adding a harness means adding a file, never editing a switch in the launch path.
- Screen behavior is proven by golden fixtures under `internal/tmux/testdata/screens/<tool>/<scenario>/`. A new tool ships with its captures.
- Keep planning pure: `PrepareLaunch` / `PrepareResume` return a `LaunchPlan`; the caller executes it.
- `fake` (`tool_fake.go`, binary in `cmd/fake`) is the eval stand-in. It prints a device login screen, accepts any code, and writes a marker file under `harness/fake/`, so `harness-login` and `harness-logout-confirm` never touch a vendor account. It has no TUI, so it ships a `login` fixture only.

### Logging

Every service sets `log` in `New` with `slog.Default().With("source", "<service>")`. Log lines use the registered event name as the message (`s.log.Info("api.request", …)`), so bus, log, and audit share one vocabulary. Libraries return errors; the edge logs them.

## Lint and format

New code must pass:

```bash
golangci-lint run --new-from-merge-base=main ./...
```

See `.golangci.yaml`.

- Format with the project formatters (`gofmt` / `goimports`). Do not hand-format imports.
- After edits, run the linter on the packages you touched.
- Do not sprinkle `//nolint` without a reason on the same line.

```go
var defaultTimeout = 5 * time.Second //nolint:gochecknoglobals // process-wide default, never mutated
```

## What not to do

- Do not add a package-level helper “for cleanliness” when the logic is used once and belongs on an existing type.
- Do not introduce a new interface for a single implementation.
- Do not add getters/setters with no invariant.
- Do not use `init()` to hide dependency wiring.
- Do not create `utils.go` to make a file shorter.
- Do not add `handlers/`, `models/`, or `services/` directories that only exist to sort file types.
- Do not extract a file per HTTP verb or per SQL query.
- Do not store `context.Context` on a request-scoped struct.
- Do not add a global client, DB handle, or logger.

When unsure: put the new code on the existing type, in the existing file, with dependencies passed into `NewT`. Split only when you can name the second concern in a few words.

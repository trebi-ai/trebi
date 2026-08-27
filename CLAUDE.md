# CLAUDE.md

Instructions for humans and coding agents working in this repository.

Prefer types that own behavior. Prefer fewer, denser files. Prefer explicit wiring over package-level state. Do not invent `util` packages, one-function files, or interfaces that have a single implementation.

## Layout

```text
cmd/<name>/          # binaries only: parse flags, load config, wire deps, Run
internal/            # private application code
internal/<domain>/   # one concept per package (user, invoice, conn)
```

- Put binaries in `cmd/<name>/`. Keep `main` thin.
- Private code lives in `internal/`.
- Name packages after what they provide (`user`, `invoice`), not what they contain (`models`, `types`, `interfaces`, `utils`, `common`, `helpers`).
- One concept per package. Adapters stay at the edge (`internal/http`, `internal/postgres`); domain code points inward.
- Do not add a directory to hold two or three files.

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
- HTTP: construction and `Listen` live in `server.go`. All handlers for one server live in `handler.go`. Split only by area (`handler_admin.go`) after that file has a clear second topic. Keep route registration next to the handlers.
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

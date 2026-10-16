# Coding Standards

How Go code in Spillway is written.
The hard rules in `AGENTS.md` come first; this file covers everything below them.
Anything `make lint` already enforces is not repeated here.

## Packages and design

- A package owns one concept and is named for it (`quota`, `redact`), never `util`, `common` or `helpers`.
- Prefer deep packages: a small exported surface with real work behind it. Unexport until something outside needs it.
- Accept interfaces, return concrete types. Define an interface in the package that consumes it, with only the methods that consumer calls. `providers.Provider` is the one shared interface.
- No global mutable state and no `init()` side effects. Wire dependencies in `internal/cli` and pass them through constructors.
- Constructors validate their inputs and return an error; a constructed value is always usable.
- Zero values should be useful or impossible to misuse. Use `New...` when they are not.
- Give domain concepts their own types (`ModelID`, `redact.Secret`) instead of bare strings when mixing them up would be a bug.

## Errors

- Wrap with context: `fmt.Errorf("fetch groq models: %w", err)`. The message says what was being done, lowercase, no trailing punctuation.
- Compare with `errors.Is` and `errors.As`, never `==` on wrapped errors or string matching.
- Sentinels (`var ErrRateLimited = errors.New(...)`) live in the package that owns the condition. Use a typed error when callers need data, such as `RetryAfter`.
- Handle each error once: either return it or log it, not both.
- Never put a secret, a full upstream body, or a full request in an error. Scrub upstream text with `internal/redact` first.
- No `panic` outside `main` and truly impossible states. Request handlers never panic.

## Context, timeouts and cancellation

- Every function that does I/O or may block takes `ctx context.Context` first, and passes it on.
- Every outbound HTTP request is built with `http.NewRequestWithContext`. The shared client has explicit timeouts; streaming uses a context deadline per request, not `Client.Timeout`.
- Client disconnect cancels the request context, which must cancel the upstream call and stop any goroutine serving it.
- Never store a context in a struct.

## Concurrency

- Every goroutine has an owner and a way to stop: a context, a closed channel, or a returned `Close`. If you can't say who stops it, don't start it.
- Prefer pull-based APIs (`Recv()`/`Next()`) over handing out channels; they make backpressure and errors explicit.
- Protect shared state with a mutex next to the data it guards, or confine it to one goroutine. No unsynchronised maps.
- Always `defer resp.Body.Close()` and drain or cancel before returning.
- Bound everything: request bodies (`http.MaxBytesReader`), SSE line buffers, queues, retries, concurrent upstream calls.

## HTTP

- Routes use the standard `http.ServeMux` method patterns (`POST /v1/chat/completions`).
- The server sets `ReadHeaderTimeout`, `IdleTimeout` and `MaxHeaderBytes`. Streaming responses must not be cut by a global `WriteTimeout`; use `http.ResponseController` for per-write deadlines.
- Errors on `/v1/*` use the OpenAI shape: `{"error":{"message":"...","type":"...","code":"..."}}` with the matching status code.
- SSE: `Content-Type: text/event-stream`, one `data: <json>\n\n` per chunk, flush after each, end with `data: [DONE]\n\n`.
- Never forward inbound headers upstream wholesale. Build upstream requests from an allowlist.

## Logging

- `log/slog` only, with a logger passed in, never the package default inside libraries.
- Log stable keys (`provider`, `model`, `status`, `latency_ms`), not formatted sentences.
- Secrets are `redact.Secret` so they log as `[REDACTED]` even by mistake.
- Request and response bodies are never logged; configuration has no body-logging option.

## Testing

- Table-driven tests named `TestThing_Behavior`, with `t.Run` subtests and `t.Parallel()` where safe.
- Tests live under `tests/`, mirroring the package they cover, as `package foo_test` (ADR 0004). They test through the package's exported API only.
- HTTP adapters: `httptest.Server` replaying fixtures from `tests/<package>/testdata/`. Handlers: `httptest.NewRecorder` or a real `httptest.Server` for streaming.
- Time-dependent code (timeouts, retry-after, circuit breakers) is tested with `testing/synctest`, never `time.Sleep`.
- Use `t.Context()` for contexts in tests and `t.TempDir()`/`t.Setenv()` for files and env.
- Compare structs with `go-cmp` only if the standard library gets painful; plain `if got != want` first.
- Expected values come from fixtures or worked examples, never recomputed the way the code computes them.
- Every security property has a test: key absent from errors, logs, `check` output and responses.

## Dependencies

- Standard library first. A new module needs a reason in the commit body, an active maintainer, and a pinned version at least two weeks old.
- Dev tools are pinned with the `go.mod` `tool` directive and run via `go tool`.
- No cgo, ever, in anything the binary imports.

## Naming and docs

- Short, clear names; receivers are one or two letters and consistent.
- No stutter: `providers.Provider` is fine, `config.ConfigLoader` is not.
- Every exported identifier has a doc comment starting with its name. Comments explain why, not what.

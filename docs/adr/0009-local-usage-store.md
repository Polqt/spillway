# 9. Usage metadata in an opt-in local SQLite store

Date: 2026-10-15
Status: accepted

## Context

v0.4 adds usage statistics, routing history, cost estimates and budgets, and all of them need data that survives a restart.
Spillway must stay a single binary with no cgo and no external service, and must not store prompts or responses by default.
v0.3 configs run without creating any file, and that must not change on upgrade.
The access log already records request metadata, but a log is not queryable and has no retention.

## Decision

- Usage metadata is stored in one SQLite file through `modernc.org/sqlite`, the pure-Go driver the hard rules name, pinned in `go.mod`.
- The store is opt-in: it exists only when `store.path` is set. Without it, no file is created and nothing is recorded.
- One row per chat request and one per provider attempt, holding only metadata: time, client app, requested model (bounded), route reason, skips, tier, provider, model, status, a closed-set error kind, stream and cache flags, latency, token counts, cost and their sources.
- The store never holds a prompt, a response, a header value, a provider key, a local key or provider error text.
- Rows older than `store.retention` (default 90 days) are pruned at startup and hourly by a goroutine the store owns.
- The schema is versioned with `PRAGMA user_version` and embedded forward-only migrations; a file with a newer version than the binary is refused.
- The file is created with mode 0600, opened in WAL mode with one writing connection, and a failure to open or migrate it stops `serve` at startup.
- Records are written synchronously after the response completes, on a context detached from the request; a write failure is logged and counted, and serving continues.
- One Spillway process per database file; sharing one file is unsupported.

## Consequences

- Usage, history and budgets survive restarts with no external database.
- The binary and module graph grow by the size of `modernc.org/sqlite`; the first store commit records the measured cost.
- The file is still sensitive: it reveals which apps used which models, when, and at what cost. The docs say so.
- Storing any prompt or response content, making the store on by default, or adding a second storage backend needs a new ADR.

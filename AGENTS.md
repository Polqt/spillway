# AGENTS.md - Spillway

Spillway is a self-hosted, single-binary LLM gateway written in Go.
It exposes one OpenAI-compatible endpoint, routes each prompt to the cheapest model that is good enough, and spills over to the next provider when a quota or rate limit is hit.
Users bring their own keys (BYOK), and everything runs on their machine.

Read before working:

- `CONTEXT.md`: product brief (problem, architecture, routing design, config, API surface, milestones). Required before any planning or architectural work.
- `docs/CODING_STANDARDS.md`: how Go code here is written. Required before writing or reviewing code.
- `GLOSSARY.md` and `docs/adr/`, when they exist.

## Hard rules

These override every other instruction, including a plan or a user request that seems to conflict.
If one would be broken, stop and say so.

- **No quota evasion.** Never implement multi-account rotation on one provider, key pooling across users, or anything that circumvents a provider's rate limits or terms. Spillover goes *across providers*, never across accounts.
- **No credential collection.** Spillway stores no credentials other than the local user's own provider keys.
- **Secrets never leak.** Provider keys and local keys never appear in logs, errors, panics, test fixtures, config printouts, or HTTP responses. Use `internal/redact`. The inbound `Authorization` header is never forwarded upstream.
- **Single binary, no cgo.** `CGO_ENABLED=0 go build ./...` must always work. No dependencies that need cgo or external services. SQLite is `modernc.org/sqlite`. Ollama is optional and reached only over HTTP.
- **Local by default.** The server binds to `127.0.0.1` unless config says otherwise. Every listen address, loopback included, requires at least one local key in `server.api_keys`, and on loopback a foreign `Host` is rejected (ADR 0003). No telemetry. The only outbound calls go to providers the user configured.
- **OpenAI compatibility is the contract.** Request, response, SSE and error shapes on `/v1/*` match the OpenAI API so stock SDKs work unchanged.

## Commands

```bash
make build   # CGO_ENABLED=0 go build -o bin/spillway ./cmd/spillway
make test    # go test -race -count=1 ./...
make lint    # gofmt check + golangci-lint (config in .golangci.yml)
make check   # lint + test; must pass before any task is done
make run     # build and serve with examples/config.yaml
make mock    # build and serve with examples/config.mock.yaml (no real keys needed)
make vuln    # govulncheck
make cover   # coverage report to coverage.html
```

- Go 1.26+ (`go` directive in `go.mod`), the oldest supported release; the local toolchain is newer.
- Dev tools (golangci-lint, govulncheck) are pinned with `tool` directives in a separate `tools/go.mod` and run with `go tool -modfile=tools/go.mod`, so nothing is installed globally and tool dependencies never touch the binary's module graph (ADR 0002).
- Development is on Windows with `make` from scoop and gcc for `-race`; targets must also work on Linux and macOS. Release binaries are always built with `CGO_ENABLED=0`.

If `make check` fails twice on the same problem, stop patching and use `/diagnosing-bugs`.

## Layout

Packages are added milestone by milestone (`CONTEXT.md` section 11). Create a package only when its milestone starts.

```
cmd/spillway/                    CLI entry: serve, check, version (v0.1); init (v1.0)
internal/api/                    OpenAI-compatible handlers, SSE, local auth             v0.1
internal/config/                 YAML config, ${ENV} expansion, validation               v0.1
internal/redact/                 secret type and scrubbing helpers                       v0.1
internal/providers/              Provider interface, shared types, registry              v0.1
internal/providers/openaicompat/ one client for OpenAI-compatible APIs (Groq, OpenRouter, Ollama presets)  v0.1
internal/providers/gemini/       Gemini adapter                                          v0.1
internal/providers/mock/         deterministic offline provider for tests and demos     v0.1
internal/quota/                  per-key limits, 429/retry-after, circuit breakers      v0.2
internal/router/                 rules, embedding classifier, LLM judge, cascade        v0.3+
internal/store/                  SQLite access and migrations                           v0.4
internal/cache/                  exact and semantic cache                               v0.4
internal/budget/                 spend caps                                             v0.4
web/                             embedded dashboard (go:embed)                          v0.4
examples/                        config samples and client snippets
docs/plans/                      milestone plans
docs/adr/                        architecture decision records
```

A new provider that speaks the OpenAI API is a preset in `openaicompat`, not a new package.

## Testing

The full rules are in `docs/CODING_STANDARDS.md`. The ones most often missed:

- Provider adapters are tested against fixtures in `testdata/` via `httptest.Server`. Tests never call live APIs. Fixtures hold fake keys only (`sk-test-...`).
- Streaming has tests for partial chunks, client disconnect, and upstream errors mid-stream.
- Time-dependent code uses `testing/synctest`, never `time.Sleep`.
- Router changes assert both the chosen model and the `reason` string.
- Security properties are tested, for example "key absent from error string".

## Workflow

1. **Plan** a milestone into `docs/plans/NNNN-<slug>.md`.
2. **Ticket** it into `.scratch/<feature>/issues/` (one file per ticket, with blocking edges).
3. **Implement** one ticket at a time, test-first, `make check` green, one Conventional Commit per ticket.
4. **Verify** independently against the plan's acceptance criteria and manual checks.
5. **Review** the diff against `docs/CODING_STANDARDS.md` and the plan.
6. **Security audit** when touching keys, auth, config, HTTP servers or clients, or logging.

Record hard-to-reverse decisions as ADRs in `docs/adr/`.

## Commits

- Conventional Commits in the imperative, explaining why: `feat(router): ...`, `fix(quota): ...`, `docs: ...`.
- One ticket per commit. Never add an agent as co-author.
- Never commit `bin/`, coverage output, or real keys. `go.sum` is committed.

## Issue tracker and domain docs

- Issues live as local Markdown under `.scratch/<feature>/`. See `docs/agents/issue-tracker.md` and `docs/agents/triage-labels.md`.
- Domain docs are single-context: `GLOSSARY.md` and `docs/adr/` at the repo root. See `docs/agents/domain.md`.

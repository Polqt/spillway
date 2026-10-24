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

- **No quota evasion.** Multiple provider instances are allowed only for legitimate workload isolation using independently authorized projects, accounts, or credentials, and only when the provider's terms permit that use. A new key, project, account, organization, or instance is never proof of independent capacity or permission to exceed any shared provider limit. Never use or coordinate instances, keys, projects, accounts, or organizations to evade provider rate limits, quotas, free-tier caps, or terms; never pool credentials across users. Automatic spillover is allowed only across distinct provider services and only within their applicable limits. Never rotate or automatically fail over between instances of the same provider service to escape a quota or restriction.
- **No credential collection.** Spillway stores no credentials other than the local user's own provider keys.
- **Secrets never leak.** Provider keys and local keys never appear in logs, errors, panics, test fixtures, config printouts, or HTTP responses. Use `internal/redact`. The inbound `Authorization` header is never forwarded upstream.
- **Single binary, no cgo.** `CGO_ENABLED=0 go build ./...` must always work. No dependencies that need cgo or external services. SQLite is `modernc.org/sqlite`. Ollama is optional and reached only over HTTP.
- **Local by default.** The server binds to `127.0.0.1` unless config says otherwise. Every listen address, loopback included, requires at least one local key in `server.api_keys`, and on loopback a foreign `Host` is rejected (ADR 0003). No telemetry. The only outbound calls go to providers the user configured.
- **OpenAI compatibility is the contract.** Request, response, SSE and error shapes on `/v1/*` match the OpenAI API so stock SDKs work unchanged.

## Commands

```bash
make build   # CGO_ENABLED=0 go build -o bin/spillway ./apps/spillway
make test    # go test -race -count=1 ./...
make lint    # gofmt check + golangci-lint (config in .golangci.yml)
make check   # lint + test; must pass before any task is done
make run     # build and serve with examples/config.yaml
make mock    # build and serve with examples/config.mock.yaml (no real keys needed)
make vuln    # govulncheck
make cover   # coverage report to coverage.html (-coverpkg=./..., since tests sit outside their packages)
```

- Go 1.26+ (`go` directive in `go.mod`), the oldest supported release; the local toolchain is newer.
- Dev tools (golangci-lint, govulncheck) are pinned with `tool` directives in a separate `tools/go.mod` and run with `go tool -modfile=tools/go.mod`, so nothing is installed globally and tool dependencies never touch the binary's module graph (ADR 0002).
- Development is on Windows with `make` from scoop and gcc for `-race`; targets must also work on Linux and macOS. Release binaries are always built with `CGO_ENABLED=0`.

If `make check` fails twice on the same problem, stop patching and use `/diagnosing-bugs`.

## Layout

Packages are added milestone by milestone (`CONTEXT.md` section 11). Create a package only when its milestone starts.
Deliverables live under `apps/<name>/` and all tests under `tests/` (ADR 0004).

```
apps/spillway/                   binary entry point; main.go only calls cli.Run          v0.1
apps/spillway/web/               embedded dashboard package (go:embed), part of the binary v0.4
internal/cli/                    Cobra composition, shared options, and tightly coupled handlers
internal/cli/eval/               experimental evaluation command family
internal/cli/initui/             interactive init wizard
internal/cli/provideraddui/      interactive provider-add wizard
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
examples/                        config samples and client snippets
docs/plans/                      milestone plans
tests/<package>/                 tests for internal/<package>, fixtures in testdata/     v0.1
tests/e2e/                       cross-package end-to-end tests                          v0.1
docs/adr/                        architecture decision records
```

A new provider that speaks the OpenAI API is a preset in `openaicompat`, not a new package.
A new deliverable is a directory under `apps/` named for the binary it builds; anything it shares with other apps goes in `internal/`.

### Go package layout

- A directory is a Go package boundary; files in different directories cannot share unexported identifiers.
- Name folders for one cohesive responsibility with a short, lowercase Go package name, such as `eval`, `initui` or `provideraddui`.
- Keep `internal/cli` as the command composition layer for Cobra wiring and handlers that share CLI-private behavior.
- Extract a child package when its behavior has a stable responsibility and a small API; child packages must not import their parent package.
- Follow dependencies inward from the CLI toward domain packages, and do not introduce import cycles.
- Do not create folders only to reduce file counts, one folder per command wrapper, or generic `common`, `helpers` and `utils` packages.
- Before moving files, inspect unexported references and preserve public entry points with a small facade when needed.

## Comments

- Keep code comments brief and precise: one short sentence where one will do, saying why, not what.
- No restating the code, no narrating history or the ticket, no filler.

## Senior engineering principles

- **KISS:** Choose the simplest design that fully solves the user-visible problem.
- **DRY:** Keep each rule and decision in one place; abstract repeated behavior only when it shares one meaning and changes together.
- **YAGNI:** Build the requested behavior and its necessary safeguards; avoid speculative options, frameworks, and extension points.
- **Clean code:** Use names that explain intent, keep functions cohesive, make control flow explicit, and remove dead or misleading code.
- **Engineering judgment:** Inspect existing contracts before changing them, weigh compatibility, security, accessibility, and maintenance costs, and explain material tradeoffs.
- **Evidence:** Test behavior through public interfaces, add regression coverage for fixes, and report only checks that actually ran.

## Testing

The full rules are in `docs/CODING_STANDARDS.md`. The ones most often missed:

- Every test lives under `tests/`, mirroring the package it covers (`internal/config` is tested in `tests/config/`). No `_test.go` file goes next to production code (ADR 0004).
- Provider adapters are tested against fixtures in `tests/<package>/testdata/` via `httptest.Server`. Tests never call live APIs. Fixtures hold fake keys only (`sk-test-...`).
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
7. **Checklist** After verification, tests, fixes, review and any required security audit, revisit the scratch ticket's acceptance checklist. Check only items supported by evidence, record any unmet items, and report the ticket complete only when every acceptance item is met.

Do not include the ticket Markdown file in a commit.

Record hard-to-reverse decisions as ADRs in `docs/adr/`.

## Commits

- Conventional Commits in the imperative, explaining why: `feat(router): ...`, `fix(quota): ...`, `docs: ...`.
- One ticket per commit. Never add an agent as co-author.
- Never commit `bin/`, coverage output, or real keys. `go.sum` is committed.
- Apply these commit rules on the current working branch, including branches other than `main`; do not switch branches just to create a ticket commit.
- Before staging, inspect `git status` and stage only deliverables required by the ticket or plan.
- Remove disposable generated artifacts when they are no longer needed; preserve source, audit evidence and user data unless the user explicitly asks to delete them.
- Keep `.scratch/` local. It contains issue files and private run data, including evaluation answers and blind keys, and must not be committed.

## Feature branches and pull requests

- Create a short-lived branch for each feature from its intended target branch, using `feat/` or `fix/` followed by a hyphenated two- or three-word phrase, such as `feat/config-dx-fixtures`.
- Keep unrelated feature work out of release-named branches.
- Before opening a pull request, rebase the private feature branch onto the latest target branch, resolve conflicts, and rerun `make check`.
- Rebase only the feature branch owned by the task; do not rewrite shared branch history, and ask before force-pushing.
- Before pushing any branch, including `main`, confirm that all CI checks for the changes have passed.
- Prepare a concise pull request title in imperative Conventional Commit style, with a scope when it helps, and describe the user-visible outcome.
- Write the pull request description for reviewers with the context, key changes, verification commands and results, and any material risks or rollout notes.
- Link the issue or plan when available, and do not claim checks that were not run.
- After verification and review, push the feature branch and open the pull request against its intended target branch.
- After the pull request is merged, delete the remote feature branch and its local branch.

## Release documentation

- When publishing a new version tag, update `README.md` and `docs/install.md` to use the newest published tag in archive, `go install`, and Docker examples.
- Keep the README's download link pointed at the current published release page, and do not describe an unpublished stable version as available.
- Do not add a verification-status table to `README.md`; keep changing audit and test evidence in release notes or the local ticket instead.

## Issue tracker and domain docs

- Issues live as local Markdown under `.scratch/<feature>/`. See `docs/agents/issue-tracker.md` and `docs/agents/triage-labels.md`.
- Domain docs are single-context: `GLOSSARY.md` and `docs/adr/` at the repo root. See `docs/agents/domain.md`.

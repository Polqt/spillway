# 4. Apps under apps/, all tests under tests/

Date: 2026-10-14
Status: accepted

## Context

The v0.1 plan used the standard Go layout: the binary in `cmd/spillway/` and each package's `_test.go` files next to its code.
The owner wants deliverables grouped under `apps/` and tests kept apart from production code in one `tests/` tree.
Go only lets a test see a package's internals from the package's own directory, and `package main` cannot be imported at all.
`go install` names a binary after its directory.

## Decision

- The binary's entry point is `apps/spillway/main.go`, so `go install` still produces `spillway`. It only calls `cli.Run`.
- Subcommand dispatch and the wiring from config to providers and the server live in `internal/cli`, which tests can import.
- The build-time version is injected with `-X github.com/Polqt/spillway/internal/cli.version=...`.
- The embedded dashboard (v0.4) lives in `apps/spillway/web/` as a Go package that embeds its assets and exports an `http.Handler`. It is compiled into the one binary, never shipped as a separate app.
- Every test lives under `tests/`, mirroring the package it covers: `internal/config` is tested in `tests/config/`, `internal/providers/openaicompat` in `tests/providers/openaicompat/`.
- Fixtures sit next to the tests that read them, in `tests/<package>/testdata/`.
- Cross-package end-to-end tests live in `tests/e2e/`.
- No `_test.go` file exists outside `tests/`.

## Consequences

- Tests exercise only exported APIs, which the coding standards already preferred. Behaviour that needs a test must be reachable through an exported identifier.
- Coverage is attributed with `-coverpkg=./...`; `make cover` does this. A plain `go test -cover` on one package reports nothing.
- `go test ./internal/config/` runs no tests; run `go test ./tests/config/` or `make test`.
- Production packages show `[no test files]` in `go test` output, which is expected.

# 24. Version identity across install channels

Date: 2026-10-20
Status: accepted

## Context

A v1.0 release archive printed `spillway 1.0.0`, while `go install ...@v1.0.0` printed `spillway v1.0.0`, so the same release looked like two versions.
A local `go build` reported a Go pseudo-version such as `v1.0.1-0.20261020022547-61fb99757afd+dirty`, which names no release that exists.
During onboarding a user ran an older or different copy of `spillway` and a server with a different config, and nothing showed which executable, config or dashboard assets were in use.

## Decision

Every channel reports a `v`-prefixed version in `spillway version`.
GoReleaser stamps `-X github.com/Polqt/spillway/internal/cli.version=v{{ .Version }}`, so archives and images print `vX.Y.Z` and snapshots print `vX.Y.Z-SNAPSHOT-<sha>`.
This amends the ldflags bullet of ADR 0016 only in that prefix; the other build flags and reproducibility rules are unchanged.
`make build` stamps `git describe --tags --always --dirty`, which already carries the prefix; without git it stamps `dev`.
`go install` keeps reporting its module version, and a build with neither stamp nor module version reports `dev`.

The default `spillway version` line keeps its fields and order for scripts.
`spillway version -verbose` adds one `key: value` line each for the executable path, the module path and version, each `vcs.*` build setting, and `dashboard_assets` (`embedded` or `fallback`).
`serve` logs `spillway starting` with the version, executable path and absolute config path before it listens.
An authenticated `GET /api/gateway` returns only `version` and `dashboard_assets`.
It follows every `/api/*` rule: dashboard key, Host check, `no-store`, OpenAI-shaped errors, and no secrets or config values.

## Consequences

- Archive, image and `go install` users see the same version string for one release.
- Local builds name the nearest tag and commit instead of a fictional pseudo-version.
- A user can tell from the CLI, the server log or the dashboard which copy is running and whether it serves the embedded or fallback dashboard.
- Docker image tags keep their unprefixed form, such as `1.1.0`; only the binary's output changes.

## Rejected alternatives

Appending the executable path to the default `version` line would change a line that scripts split.
Leaving the prefix off `go install` output would require rewriting a module version the Go toolchain owns.

## What needs a new ADR

Changing the default `version` line format, adding fields to `GET /api/gateway`, or exposing any config value through version output requires a new ADR.

# 18. Separate release tooling from Go 1.26 checks

Date: 2026-10-16
Status: accepted

## Context

Spillway supports Go 1.26, and its CI plan requires `make check` on that minimum version.
The Go 1.27.2 release toolchain is pinned for release builds.
GoReleaser v2.18.2 requires Go 1.27.1, so placing it in `tools/go.mod` raises that module's minimum Go version and makes `go tool -modfile tools/go.mod` fail under Go 1.26.

## Decision

Keep `golangci-lint` and `govulncheck` pinned in `tools/go.mod`, whose minimum remains Go 1.26.0.
Pin GoReleaser v2.18.2 in `tools/release/go.mod`, which uses Go 1.27.2.
Make the CI matrix run `make check RACE=-race` on every OS and Go version.
Run release configuration and snapshot commands with the Go 1.27.2 release toolchain and the separate release-tools module.

This supersedes ADR 0016 only for GoReleaser's module location.
The release tool version and artifact configuration are unchanged.

## Consequences

- Go 1.26 can run the complete check suite without loading a tool module that requires a newer Go version.
- Release tools remain pinned outside the shipped binary's module graph.
- The release-tools module duplicates some checksums from `tools/go.sum` because Go modules maintain independent dependency graphs.

## Rejected alternative

Running only build and tests on Go 1.26 would leave the approved full-check criterion unmet.
Using an unpinned GoReleaser binary would weaken reproducibility.

## What needs a new ADR

Changing the minimum Go version, the release Go toolchain, or the pinned GoReleaser version requires a new ADR.

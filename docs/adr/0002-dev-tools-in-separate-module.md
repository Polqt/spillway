# 2. Pin dev tools in a separate tools/go.mod

Date: 2026-10-14
Status: accepted

## Context

Dev tools (golangci-lint, govulncheck) are pinned with the Go `tool` directive so nothing is installed globally.
Putting `tool` lines in the root `go.mod` merges the tools' requirements into the binary's module graph.
golangci-lint alone brings about 226 requirements, and minimal version selection can then raise the versions of modules the binary links, such as `go.yaml.in/yaml/v3` or `golang.org/x/sys`.

## Decision

The `tool` directives live in `tools/go.mod` (module `github.com/Polqt/spillway/tools`).
Tools run with `go tool -modfile=tools/go.mod <tool>`, wrapped by Makefile targets.
The root `go.mod` contains only what the binary needs.

## Consequences

- Upgrading a tool never changes a dependency of the shipped binary, and `go.sum` stays small and reviewable.
- Tool versions are still pinned and checksummed, in `tools/go.sum`.
- Contributors run tools through `make`, not bare `go tool`, which without `-modfile` would not find them.

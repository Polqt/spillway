# 01: Project skeleton

**What to build:** A contributor clones the repo and runs `make check`, `make build` and `spillway version`, and all three work on Windows, Linux and macOS with nothing installed beyond Go and make.
The module is `github.com/Polqt/spillway`; dev tools are pinned in their own module (ADR 0002), and the binary builds without cgo.
Design: plan `docs/plans/0001-v0.1-proxy.md`, "Toolchain and module", "Makefile", task T1.

**Blocked by:** None (can start immediately).

**Status:** ready-for-agent

- [x] `make check` passes: gofmt check, golangci-lint with the committed config, and `go test -race`.
- [x] `make lint` fails when a `.go` file is unformatted (proved by a test or a scripted check, then reverted).
- [x] `make build` produces the binary with `CGO_ENABLED=0`.
- [x] `spillway version` prints the version injected at build time, and `dev` when none was injected.
- [x] `make vuln` runs the pinned govulncheck.
- [x] The root `go.mod` contains no tool requirements; tools run through `make`.
- [x] Line endings are normalised so lint results do not depend on the platform.

## Comments

- 2026-10-14: Implemented. All checklist items verified on Windows (PowerShell, Git Bash, cmd.exe). Linux and macOS not verified: WSL has no Go toolchain. The gofmt failure was proved manually with a throwaway misformatted file (LF and CRLF), then removed.

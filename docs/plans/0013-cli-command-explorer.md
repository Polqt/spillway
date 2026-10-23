# 1.3 CLI command explorer and interactive command browsers

## Goal

Make interactive command discovery and read-only management flows feel as deliberate as `spillway init` while preserving the stable behavior of commands in scripts and non-interactive terminals.

## Design

- In an interactive terminal, bare `spillway` and root help open a Bubble Tea command explorer.
- The explorer lets users filter commands, inspect concise help, and copy or type the displayed invocation themselves.
- It never executes a selected command, starts networking, or changes files.
- Interactive config, provider, model and tier browsing use the same filter, detail and navigation patterns.
- Interactive `check` and `doctor` show their reports in a scrollable detail view; secrets remain redacted.
- One-shot output, server processes, edits and external actions keep their existing command contracts.
- `--no-tui`, non-interactive streams, and command-specific help use readable linear output.
- Shared semantic styles keep help and status output consistent and honor `NO_COLOR`.
- `AGENTS.md` and `CLAUDE.md` document KISS, DRY, YAGNI, clean code, and senior engineering expectations.

## Acceptance criteria

- Command discovery works with keyboard only, terminal resize, and narrow terminal widths.
- Every root command is discoverable with its group and useful summary.
- Details include usage, examples, flags, and a plain mode that remains suitable for pipes and screen readers.
- Help does not execute commands or expose configuration values.
- Non-TTY output keeps its existing exit codes and stream behavior.
- CLI tests cover interactive filtering, safe detail views, report redaction and linear help contracts.
- `make check` passes.

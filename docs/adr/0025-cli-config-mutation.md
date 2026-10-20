# 25. Safe CLI provider configuration edits

Date: 2026-10-20
Status: accepted

## Context

`provider add` must update a valid config without replacing local API keys, comments, quoting, or user formatting.
It must leave a recoverable previous version and must not expose provider key values.
`init` already creates files exclusively and must keep that behavior.
The accepted CLI framework decision in ADR 0020 limits Huh prompts to `init` unless a later ADR approves another use.

## Decision

- Config edits locate YAML nodes and splice only the inserted block-style lines into the original bytes.
- The editor rejects flow-style targets, YAML anchors, aliases, merge keys, and multiple documents with a paste-ready snippet.
- Every proposed result is validated with a unique placeholder for each environment reference and compared against the intended in-memory tree change.
- The edit path never marshals a loaded `Config` and never accepts a provider key value.
- A commit rechecks the source SHA-256 digest, writes an exclusive timestamped UTC backup, writes and syncs a same-directory temporary file, and atomically replaces the resolved target.
- Unix replacements use rename followed by directory sync, and the replacement and backup files use mode 0600.
- Windows replacements use `ReplaceFileW` loaded through `windows.NewLazySystemDLL`, preserving the target security descriptor, with bounded retries for sharing and lock violations.
- The five newest backups matching the exact timestamp pattern are kept, and unrelated files are never pruned.
- If the source changed after it was read, the edit aborts and asks the user to rerun the command.
- Read-only files and bind-mounted targets are reported with instructions to edit the file on the host.
- `provider add` is experimental for v1.1.x; `init` continues to refuse overwriting an existing file.
- This ADR approves Huh accessible prompts for provider selection, non-secret configuration choices, and preview confirmation in experimental `provider add` only; it does not approve prompts for provider key values.

## Consequences

- Existing config bytes outside inserted lines remain unchanged.
- Backups contain the prior config, including local keys, and their paths never contain key material.
- A concurrent editor can still write in the small interval between the digest check and atomic replacement.
- The command does not signal a running server; the user restarts it to load the new config.
- `provider add` may change its flags in a later minor release while it remains experimental.

## Rejected alternatives

Re-encoding the YAML tree can change comments, whitespace, ordering, and quoting.
Marshalling a loaded `Config` can replace secret values with redaction markers or write expanded secrets.
Regex editing cannot reliably locate YAML containers or prove that unrelated values stayed unchanged.
Lock files can remain stale after a crash and do not coordinate with ordinary editors.

## What needs a new ADR

Changing the edit and backup guarantees, allowing key values through the edit API, or making config mutation stable requires a new ADR.

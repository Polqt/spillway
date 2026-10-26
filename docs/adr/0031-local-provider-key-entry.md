# 31. Hidden local provider key entry

Date: 2026-10-25
Status: accepted

## Context

Some users prefer not to manage provider keys through shell environment variables.
Passing a key as a command-line argument can expose it through shell history and process inspection.
The CLI config editor already preserves unrelated YAML and creates a recoverable private backup.

## Decision

- Add `spillway provider key set <instance> -config <path>` with one asterisk per entered byte.
- Never accept provider key values as command-line arguments or echo them in output.
- Store the user's own provider key in that provider's `api_key` config field; environment references remain supported.
- Redact literal keys from config output and config-edit previews.
- Use the existing atomic config edit and backup flow, and tell users to keep the config and backups private.
- Do not rotate or fail over between instances of the same provider service to evade limits.

## Consequences

The config file and its backups contain the provider key in plaintext and must be protected by the user.
The command requires an interactive terminal for secret entry.
Existing environment-based configs continue to work unchanged.

## Rejected alternatives

Command-line key arguments can be recorded in shell history or exposed to process inspection.
Adding an operating-system credential store would add platform-specific behavior and dependencies.

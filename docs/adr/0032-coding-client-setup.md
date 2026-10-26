# 32. Coding client setup uses local keys through client-native mechanisms

Date: 2026-10-25
Status: accepted

## Context

OpenCode, Claude Code, and Codex use different API protocols and credential mechanisms.
Spillway keeps the user's local gateway key in its own config and must not print it during setup.
OpenCode supports file substitution for provider API keys, while Claude Code and Codex support commands that return a bearer token.

## Decision

- `spillway setup opencode`, `spillway setup claude`, and `spillway setup codex` write global, user-level client configuration.
- OpenCode reads the existing local `default` key from a separate `spillway.key` file in its private configuration directory.
- Claude Code and Codex call `spillway key -config <path> default` when they need the key.
- Codex uses an isolated `spillway.config.toml` profile under `$CODEX_HOME` (default `~/.codex`), selected with `codex --profile spillway`, so its main config is not rewritten.
- Setup supports only loopback gateway endpoints and reuses the existing local key; it creates no provider credentials.
- Existing JSON client settings are merged and backed up before updates.

## Consequences

- OpenCode has an additional private copy of the user's local gateway key.
- Setup never prints any local or provider key.
- Key helper commands resolve the latest local key from the selected Spillway config.
- The three clients share Spillway routing and authentication while receiving their respective wire protocols.

## Rejected alternatives

- Requiring users to paste the local key into each client would add manual steps and increase accidental exposure.
- Reusing Chat Completions directly would not work for clients that require Anthropic Messages or OpenAI Responses.

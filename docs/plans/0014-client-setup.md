# Spillway coding client setup

## Objective

Make Spillway usable from OpenCode, Claude Code, and Codex with one global setup command per client, while preserving existing client settings and keeping local keys out of command output.

## Scope

- Add `spillway setup opencode`, `spillway setup claude`, and `spillway setup codex`.
- Add the minimum Anthropic Messages and OpenAI Responses API compatibility required for coding clients, including text, tool calls, non-streaming responses, and streaming responses.
- Reuse the existing router, provider adapters, local authentication, and usage tracking.
- Resolve the user's existing Spillway config and local `default` key without printing the key.
- Make setup repeatable, global per user, and safe when client config files already contain settings.
- Document supported request features and any unsupported features honestly.

## Non-goals

- Full parity with every Anthropic or OpenAI Responses API feature.
- Adding a mock provider to the shipped binary.
- Making live provider calls in tests.
- Modifying existing client configs without an explicit command.

## Decisions

- Use public API handlers as the compatibility seam, backed by the existing Chat Completions execution path.
- Configure clients to request `auto` by default and use the existing local default key.
- For OpenCode, use its supported custom OpenAI-compatible provider config.
- For Claude Code, use its Anthropic gateway settings and key helper.
- For Codex, use a Responses provider and command-backed authentication that invokes `spillway key`.
- Preserve unrelated JSON/TOML settings, make writes atomic, and create a backup before changing an existing file.

## Tickets

1. Add protocol translation for Anthropic Messages and OpenAI Responses.
2. Add the three global client setup commands.
3. Add binary-level end-to-end coverage and client documentation.

## Acceptance

- Each configured client uses only the existing loopback Spillway API and a local key.
- All three protocol endpoints route through Spillway and return their respective wire formats for text and tool calls, including streaming.
- End-to-end tests launch the built binary with a local fake upstream and make no external provider calls.
- Setup preserves existing client settings, is idempotent, and never prints local or provider keys.
- `make check` passes.

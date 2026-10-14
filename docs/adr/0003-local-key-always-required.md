# 3. Require a local key even on loopback

Date: 2026-10-14
Status: accepted

## Context

Spillway binds to `127.0.0.1` by default, and the v0.1 plan made `server.api_keys` optional on loopback.
Loopback is not a trust boundary against the browser.
Any web page the user visits can send a "simple" `text/plain` POST to `http://localhost:8080`, and a page using DNS rebinding can make full requests that the browser treats as same-origin.
Without a local key, either one spends the user's provider keys and can read model output.

## Decision

`server.api_keys` must list at least one local key for every listen address, loopback included; `serve` and `check` refuse a config without one.
As a second layer, when Spillway listens on loopback it rejects requests whose `Host` is not `localhost`, `127.0.0.1`, `[::1]` or the configured listen address, with or without the port.
`/healthz` stays unauthenticated and returns no configuration or provider details.

## Consequences

- Drive-by and rebinding requests fail: the page does not know a local key, and a rebound `Host` is rejected before auth.
- No extra setup for real clients: OpenAI SDKs already require an API key string, so users set one either way.
- `examples/config.mock.yaml` keeps its literal fake key, so `make mock` still needs no environment.
- Changing this back would reopen the hole, so it needs a new ADR, not an edit.

# 02: Mock chat end to end

**What to build:** The tracer bullet.
A user runs `make mock`, then sends a non-streaming chat completion for the model reference `mock/echo` with their local key, and gets back an OpenAI `chat.completion` object.
Everything a request crosses exists in its smallest real form: a minimal config (listen address, local keys, the mock provider), the secret type so local keys cannot leak, the `Provider` interface, the mock provider, local key auth, the loopback `Host` check (ADR 0003), the OpenAI error shape, and `spillway serve`.
Design: plan sections "internal/redact", "internal/providers", "internal/providers/mock", "internal/api"; ADR 0003; GLOSSARY.md.

**Blocked by:** 01: Project skeleton.

**Status:** ready-for-agent

- [x] `make mock` serves on `127.0.0.1:8080` using `examples/config.mock.yaml`, which contains only a clearly fake local key.
- [x] `POST /v1/chat/completions` with `model: mock/echo` and a valid local key returns 200 and a `chat.completion` the official OpenAI SDK parses.
- [x] The response carries `x-spillway-provider` and `x-spillway-model` (the upstream model).
- [x] A missing or wrong local key returns 401 in the OpenAI error shape, and the response never echoes the key.
- [x] On a loopback listen, a request whose `Host` is not `localhost`, `127.0.0.1`, `[::1]` or the listen address is rejected before auth.
- [x] A config with no local key is refused at startup, on loopback too.
- [x] Local keys accept both the plain and the named form; logs carry `client_app=<name>`, never the key.
- [x] `GET /healthz` returns `ok` without a local key and reveals nothing else.
- [x] Key comparison is constant-time and does not reveal which key matched.
- [x] Tests prove a local key never appears via `fmt` verbs, slog output or JSON.
- [x] `make check` passes.

## Comments

2026-10-14: Implemented in 3562106, with review and audit fixes in the follow-up commit.
Verifier: PASS on all 11 boxes, with evidence from `make check`, `make mock` + curl, and the official OpenAI Python SDK (openai 2.54.0).
Security audit: PASS with no Critical or High findings.
Both Medium findings are fixed with regression tests: a local key written as a YAML mapping key no longer appears in config errors, and `serve` now logs a WARN on non-loopback listens.
Open Low finding, not yet scheduled: local keys have no minimum length.
Deferred to later tickets: provider error mapping and `context.Canceled` (03, 06), lossless `Extra` pass-through (06), log scrubbing including panic values (05).

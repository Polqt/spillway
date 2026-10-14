# 06: Groq end to end

**What to build:** A user with a Groq provider key adds Groq to their config, and `groq/<upstream model>` works for chat, streaming and model listing exactly as `mock/echo` does.
This introduces the one shared client for OpenAI-compatible providers, configured by a Groq preset, with provider errors mapped to the shared error kinds (rate limited with retry-after, unauthorized, unavailable, bad request, model not found).
Request fields Spillway does not know pass through to the provider unchanged, and responses are relayed verbatim.
Design: plan "internal/providers/openaicompat", "internal/providers" (errors, SSE reader), "Examples".

**Blocked by:** 03: Streaming end to end on the mock; 04: Model listing and model-reference errors; 05: Full config and `spillway check`.

**Status:** ready-for-agent

- [x] Chat, streaming and model listing for Groq pass against recorded fixtures served by `httptest.Server`; no test calls a live API, and fixtures hold only `sk-test-...` keys.
- [x] Outbound requests carry only allowlisted headers; the client's `Authorization` is never forwarded, and the provider key is sent only to the configured base URL.
- [x] Redirects are not followed.
- [x] Upstream 429 maps to rate limited with retry-after, 401 and 403 to unauthorized, 5xx to unavailable, each returned to the client in the OpenAI error shape.
- [x] Upstream error text is scrubbed of the provider key before it reaches logs or the client; a test proves it.
- [x] Unknown request fields (`tools`, `response_format`, future fields) reach the provider byte-for-byte.
- [x] `examples/config.yaml` exists with env placeholders only.
- [x] `make check` passes.

# 03: Streaming end to end on the mock

**What to build:** A client sends `stream: true` for a mock model and receives server-sent events exactly as the OpenAI API sends them, ending in `data: [DONE]`.
The `Stream` is pull-based and owns no goroutine.
A failure after the first chunk has reached the client ends the stream with an error event and no `[DONE]`, because spillover is no longer possible once the client has any of the response (GLOSSARY.md, "Spillover").
A client that disconnects cancels the work behind its request.
Design: plan "internal/providers" (Stream), "internal/providers/mock", "internal/api" streaming steps.

**Blocked by:** 02: Mock chat end to end.

**Status:** ready-for-agent

- [x] `stream: true` returns `Content-Type: text/event-stream`, one `data: <chunk>` event per chunk, each flushed, then `data: [DONE]`.
- [x] The official OpenAI SDK consumes the stream unchanged.
- [x] A mock model that fails mid-stream produces its chunks, then `data: {"error":{...}}`, and no `[DONE]`.
- [x] Client disconnect cancels the provider stream; a test proves the stream is closed and nothing keeps running.
- [x] Tests cover chunks split across reads (partial chunks), client disconnect, and upstream error mid-stream.
- [x] No `time.Sleep` in tests.
- [x] `make check` passes.

## Comments

- 2026-10-14: Implemented.
  Verified by `make check`, curl against `make mock`, and the official OpenAI Python SDK (openai 3.31.0): the echo stream yields `You said: hello spillway` plus usage, and `mock/stream-error` raises `APIError` after two chunks.
  SSE headers are sent with the first chunk, so a failure before it is still a plain HTTP error (matches GLOSSARY "Spillover").
  Partial chunks are covered client-side (`TestStream_PartialReads`); the upstream variant (`TestStream_PartialUpstreamChunks`) needs the SSE reader and is owed by 06.
  Per-chunk write deadlines remain with 09.

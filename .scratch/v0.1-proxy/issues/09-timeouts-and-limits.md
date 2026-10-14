# 09: Timeouts and limits

**What to build:** Spillway stays healthy against slow or hostile clients and slow providers, while long streams still finish.
Request bodies over 10 MiB are refused, idle and slow-header connections are closed, each streamed chunk gets its own write deadline instead of a global one that would cut long streams, and provider calls time out.
An in-memory network lets these be tested with `testing/synctest`, so no test waits on a real clock.
Design: plan "internal/fakenet", "internal/api" (server timeouts, body limit), decision 8; `docs/CODING_STANDARDS.md` HTTP section.

**Blocked by:** 06: Groq end to end.

**Status:** complete

- [x] A body over 10 MiB returns 413 in the OpenAI error shape without reading the rest.
- [x] The server sets read-header and idle timeouts and a header size limit.
- [x] A stream longer than any server-wide timeout completes; a client that stops reading is cut off by the per-chunk write deadline.
- [x] A provider that stalls before responding, or mid-stream, times out and the client gets an error (an error event if streaming had started).
- [x] Every timeout test runs under `testing/synctest` over the in-memory network; no `time.Sleep`.
- [x] `make check` passes.

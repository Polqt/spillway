# 08: Gemini

**What to build:** A user adds a Gemini provider key, and `gemini/<upstream model>` works for chat, streaming and model listing through Gemini's OpenAI-compatible endpoint (ADR 0001).
Gemini's differences are handled: model ids listed with a `models/` prefix appear without it, and errors that arrive as a one-element JSON array with status `RESOURCE_EXHAUSTED` map to rate limited.
Design: plan "internal/providers/gemini"; ADR 0001.

**Blocked by:** 06: Groq end to end.

**Status:** ready-for-agent

- [x] Chat, streaming and model listing pass against recorded Gemini fixtures.
- [x] `/v1/models` shows `gemini/<id>` without the `models/` prefix.
- [x] An array-wrapped `RESOURCE_EXHAUSTED` error maps to rate limited (429) in the OpenAI error shape.
- [x] The provider key travels only in the `Authorization` header, never in a URL; a test proves transport errors cannot contain it.
- [x] `examples/config.yaml` shows Gemini.
- [x] `make check` passes.

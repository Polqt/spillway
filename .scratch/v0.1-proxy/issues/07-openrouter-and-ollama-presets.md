# 07: OpenRouter and Ollama presets

**What to build:** A user adds OpenRouter and a local Ollama to their config, and model references such as `openrouter/meta-llama/llama-3.1-8b-instruct:free` and `ollama/llama3.2:3b` work for chat, streaming and model listing.
Both are presets of the shared OpenAI-compatible client, not new packages.
Ollama needs no provider key and may use plain `http` because it is on loopback.
Design: plan "internal/providers/openaicompat" (presets).

**Blocked by:** 06: Groq end to end.

**Status:** complete

- [x] OpenRouter and Ollama chat, streaming and model listing pass against recorded fixtures.
- [x] Upstream models containing `/` and `:` round-trip unchanged.
- [x] Ollama works with no provider key and sends no `Authorization` header.
- [x] A provider key configured for Ollama is rejected by config validation.
- [x] `examples/config.yaml` shows both providers.
- [x] `make check` passes.

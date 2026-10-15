# 7. Model capabilities come from explicit declarations

Date: 2026-10-15
Status: accepted

## Context

v0.3 adds `model: "auto"`, where Spillway chooses the model, and capability filtering for `spillway/<tier>` requests.
A request can need tools, image input, JSON mode or structured outputs, and a model that lacks one answers with a 400 that does not spill (ADR 0006).
Spillway must never claim a model supports something without a reliable source.
Provider catalogs are uneven: OpenRouter and Ollama publish capability fields, Groq publishes only a context window, and Gemini's OpenAI-compatible model list publishes none.
Fetching catalogs would add startup network calls, failure modes and staleness.
v0.2 tiers send every request to every candidate in order, and that behaviour must not change for existing configs.

## Decision

- In v0.3 the only source of a model's capabilities is the user's `models:` config section.
  Each entry names a model reference, a complete `capabilities` list from the closed set `tools`, `vision`, `json_mode`, `structured_outputs` (an empty list means text only), and an optional `context_window`.
- Spillway never infers a capability from a model name, a provider or a past success.
- A request's capability requirements are detected from its body, for `auto` and `spillway/<tier>` requests only.
- A request with no capability requirement may reach every candidate, declared or not.
- A request with at least one requirement reaches a declared candidate only if it declares all of them.
  An undeclared candidate is still tried in a `spillway/<tier>` request, because the user chose that tier explicitly, and is skipped in an `auto` request, because Spillway is choosing and must not guess.
- A candidate with a declared `context_window` is skipped only when a conservative estimate of the request size clearly exceeds it; an undeclared window never filters.
- Direct model references are never inspected or filtered.

## Consequences

- No capability is ever claimed on Spillway's own authority, and v0.3 makes no new outbound calls.
- Users declare capabilities by hand; a wrong declaration either skips a working model or reaches one that answers 400.
- Configs without `models:` behave exactly as in v0.2.
- `auto` and tiers treat undeclared models differently only for requests with requirements; skip reasons say which rule applied.
- Reading capabilities from provider catalogs (planned with v0.4 pricing) would add a source alongside declarations, with declarations taking precedence; doing so, or inferring capabilities any other way, needs a new ADR.

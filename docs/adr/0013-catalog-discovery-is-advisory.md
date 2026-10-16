# 13. Provider catalog discovery is advisory

Date: 2026-10-15
Status: accepted

## Context

ADR 0007 made the user's `models:` declarations the only capability source and left catalog discovery to v0.4.
ADR 0005 deferred checking OpenRouter catalog pricing to v0.4.
Provider catalogs are uneven: OpenRouter publishes pricing, supported parameters, input modalities and context length; Ollama's `/api/show` publishes capabilities and context length; Groq publishes only a context window; Gemini's OpenAI-compatible list publishes none of these.
Using a catalog at runtime would change which model `auto` picks for existing configs, add network calls at startup, and make routing depend on a remote list that can change at any time.

## Decision

- Discovery is an explicit `spillway catalog` subcommand. It loads the config, contacts only configured providers, prints suggested `models:` and `pricing:` YAML with each value's source and a diff against current declarations, and writes nothing.
- Sources are OpenRouter, Ollama and Groq as listed above; Gemini and mock are skipped with a note.
- It warns when a configured OpenRouter `:free` model has a non-zero catalog price.
- Routing capabilities still come only from `models:` (ADR 0007), and prices only from `pricing:` or provider-reported cost (ADR 0010).
- Responses are size-bounded, every request has a timeout, each provider key goes only to its own provider, and output contains no key.

## Consequences

- Routing stays deterministic and offline, and v0.3 configs route exactly as before.
- Users review catalog data before it affects anything; it can go stale between runs, as declarations can.
- The ADR 0005 follow-up is a warning, not a runtime eligibility check.
- The command is optional within v0.4; deferring it changes no other decision.
- Reading catalog data at runtime, refreshing it on a schedule, or letting it override declarations needs a new ADR.

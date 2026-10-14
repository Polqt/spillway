# 1. Reach Gemini through its OpenAI-compatible endpoint

Date: 2026-10-14
Status: accepted

## Context

Spillway's contract on `/v1/*` is the OpenAI API, and Groq, OpenRouter and Ollama are already served by one `openaicompat` client.
Gemini offers two APIs: an OpenAI-compatible endpoint at `https://generativelanguage.googleapis.com/v1beta/openai`, and the native `generateContent` API.

## Decision

The `gemini` adapter wraps `openaicompat.Client` and talks to the OpenAI-compatible endpoint.
It adds only what differs: the `models/` prefix on model ids, and parsing errors that arrive as a one-element JSON array with status `RESOURCE_EXHAUSTED`.
The key is sent in the `Authorization` header, never in the URL.

## Consequences

- Chat, streaming, tools, JSON mode and model listing work with no translation layer, and requests pass through verbatim.
- Transport errors (`*url.Error`) cannot contain the key, because it is never in the URL. The native API puts it in `x-goog-api-key` or `?key=`.
- Gemini-only features the compatibility endpoint lacks are unavailable. Revisit with a native adapter only when a user needs one of them; that is a new ADR, not a change to this one.

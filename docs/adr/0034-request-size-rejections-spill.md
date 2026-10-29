# 34. A provider's request-size rejection spills without penalizing the provider

Date: 2026-10-29
Status: proposed

## Context

ADR 0006 does not spill on 400, 413 or 422, because the request itself is the likely cause and every candidate would reject it.
That holds for malformed requests but not for size.
Providers differ in context windows and per-minute token limits, so a request one provider service rejects as too large is often accepted by another.
On 2026-10-29 a ~35k-token OpenCode request to a free tier of Gemini, Groq and Ollama failed: Gemini returned 503 and spilled, then Groq returned 413 because its free-tier tokens-per-minute limit is 8000, and the 413 ended the request before Ollama was tried.
Groq's 413 body for that case carries `"code":"rate_limit_exceeded"`, but waiting never helps a request larger than the limit itself, and the same provider still serves smaller requests.
OpenAI reports input beyond a context window as 400 with code `context_length_exceeded`; OpenRouter's `error.code` is numeric, and Gemini reports over-long input with the generic 400 `INVALID_ARGUMENT` or a 500.

## Decision

This amends ADR 0006's list of non-spilling statuses for request-size rejections only.

- An adapter reports `providers.ErrRequestTooLarge` for an upstream HTTP 413 with any body, for an upstream HTTP 400 whose JSON `error.code` is the string `context_length_exceeded`, and for an OpenAI-shaped SSE error event whose `error.code` is `413` or `context_length_exceeded`.
  The HTTP status decides before any body code, and message text is never used to classify.
- For `auto` and `spillway/<tier>`, `ErrRequestTooLarge` spills to the next eligible candidate under every other ADR 0006 rule: the commit point, tier order, at most one attempt per candidate, and, by ADR 0029, never to another instance of the same provider service.
- A request-size rejection is a property of the request, not of the provider: it sets no cooldown, counts no circuit-breaker failure, resets no breaker or backoff, ignores `Retry-After` and rate-limit headers on the response, and leaves the candidate available to every other request.
- Direct model references never spill; a direct request-size rejection answers 400 `invalid_request_error` with code `context_length_exceeded`.
- When every eligible candidate rejected the request for size, the client gets 400 `invalid_request_error` with code `context_length_exceeded` and no `Retry-After`.
  Otherwise request-size rejections are left out of ADR 0006's exhaustion rule: 429 with the earliest `Retry-After` when every other eligible candidate has a known resume time, else 503 `no_candidate_available`.
- The attempt outcome and log `error_kind` is `request_too_large`, the exhaustion message reason is `request too large`, and neither contains provider error text, prompt text or keys.
- Every other 400, and every 422, still does not spill.

## Consequences

- Large coding requests on free tiers reach a configured provider whose limits fit, instead of failing at the first provider with a smaller limit.
- A provider that always rejects a request of that size is tried again on every such request, costing one round trip; Spillway does not learn size limits from failures, in line with ADR 0007, and users can declare `context_window` or reorder tiers to avoid the attempt.
- Each spill sends the whole prompt to another configured provider, as every spill already does.
- ADR 0010 charges failed paid attempts their estimated prompt cost, so a large request that spills across paid candidates can overcount estimated spend more than before.
- Clients see 400 `context_length_exceeded`, a code Spillway already returns under ADR 0008, in more cases; mixed exhaustion now answers 429 or 503 where it used to answer 400 `provider_bad_request`, and a direct upstream 413 keeps status 400 but changes code from `provider_bad_request` to `context_length_exceeded`.
- Spillway reads ADR 0015's "default routing" as `router.mode` and classifier defaults, so this ships in a minor release; it changes only requests that fail today and never which candidate serves a request that succeeds today.
- Gemini's and OpenRouter's over-long-input 400s have no distinct structured code and still do not spill; Gemini's 500 for long input already spills as unavailable and counts a breaker strike.
- Ollama may truncate rather than reject a long prompt, so a spill to a small local model can succeed on a truncated prompt unless its `context_window` is declared.
- For direct references the local access log keeps the scrubbed upstream message, which for Groq names the organization, as it already does for every upstream error.

## Rejected alternatives

- Classifying Groq's 413 as a rate limit would cool every Groq candidate down for requests that fit, and waiting cannot help the rejected request.
- Classifying it as a provider failure would count breaker strikes, so three large requests would stop Groq for everyone.
- Spilling on every 400 would send malformed requests to every provider, spending quota and exposing the request to more providers for no chance of success.
- Matching message text such as "Request too large" or "maximum context length" breaks when providers reword messages and contradicts the coding standard against string matching.
- Answering 413 `request_too_large` would reuse the code for Spillway's own 10 MiB body limit and point users at the wrong fix; answering 429 would make SDKs retry a request that cannot succeed.
- Remembering a per-candidate size ceiling from past rejections would add state, go stale, and infer limits from past results, which ADR 0007 rules out.
- An opt-in config field would add permanent stable surface for behaviour every user of a virtual model wants.

## What needs a new ADR

Classifying any other 400 or 422 as spillable, classifying by message text, letting a request-size rejection change quota or breaker state, or spilling a direct reference.

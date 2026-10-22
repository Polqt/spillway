# 6. Spillover policy: which failures spill, and when a response is committed

Date: 2026-10-14
Status: accepted

## Context

A `spillway/<tier>` request may be tried on several candidates in tier order.
Each extra attempt spends a request from the user's quota on another provider, and once any byte of a response has reached the client, a second provider's answer can no longer replace it.
OpenAI SDKs already retry 429 and 5xx with backoff, honouring `Retry-After`.

## Decision

- A request spills over to its next candidate on: rate limit (429), payment required (402), provider failure (5xx), transport errors, timeouts, a rejected provider key (401, 403), and a missing model (404).
- It does not spill on 400, 413 or 422, because the request itself is the likely cause and every candidate would reject it; nor after the client cancels.
- The commit point is the first byte written to the client. For streams that is the first chunk; a failure in `ChatStream`, or an error from the first `Recv`, including an error event as the first SSE event, still spills. After the commit point a failure ends the stream with an error event and no `[DONE]`.
- A candidate is tried at most once per request, and never while it is cooling down; a cooldown is never shorter than the provider's `Retry-After`.
- Candidate order is the tier's list order minus unavailable candidates. There is no randomness, weighting or latency-based ordering.
- When no candidate can serve, the client gets 429 with `Retry-After` set to the earliest resume time if every candidate has a known resume time, and otherwise 503 `no_candidate_available`. The message lists each candidate and why it was skipped, never provider error text.
- Direct model references (`provider/model`) never spill.

## Consequences

- Routing is reproducible from config, clock and failure history, which makes it testable with `testing/synctest`.
- A request with a capability some candidates lack fails with a 400 that does not spill, until capability detection in v0.3.
- Spillover across providers respects each provider's limits and never rotates accounts, as the hard rules require.
- Spilling on 400, retrying a candidate within one request, or spilling after the commit point needs a new ADR.

## Relationship to provider instances (ADRs 0028-0030)

ADRs 0028 and 0029 supersede two assumptions from this ADR while preserving its failure and response-commit rules.

- “Provider” as a candidate identity is now “provider instance”; the provider service type is a separate policy identity.
- A virtual route may contain repeated model references or models from the same instance, but it may not contain two distinct instances of one provider service type.
- Direct references still never spill and now select exactly one configured instance.

The spill-triggering failure list, candidate order, one-attempt-per-candidate rule, retry-after handling, OpenAI error behavior, and stream commit point remain in force.
The no-quota-evasion rule continues to prohibit automatic same-service instance rotation.

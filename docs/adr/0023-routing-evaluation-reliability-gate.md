# 23. Request-success reliability gate for routing evaluation

Date: 2026-10-19
Status: proposed

## Context

ADR 0014 defines cost, latency, quality, severe-failure, and known-cost-coverage gates.

The v1.0 evaluation results show why cost and latency alone cannot support a routing decision.

The evaluator must also distinguish successful requests from HTTP errors, transport failures, exhausted candidates, and incomplete streams.

Spillway's definition of spillover is sending the request to the next candidate before response bytes reach the client.

## Proposed decision

- Preserve every existing ADR 0014 gate and its statistical method.
- Add request success as a non-inferiority gate.
- A successful trial must produce a complete valid OpenAI-compatible response; a stream must reach `[DONE]`.
- An HTTP error, timeout, transport error, exhausted route, or incomplete stream counts as an unsuccessful request.
- The lower paired 95% prompt-cluster bootstrap confidence bound for a candidate policy's request-success difference versus current `auto` must be at least -2 percentage points.
- Report HTTP status classes, timeout counts, incomplete-stream counts, spillover selections, successful spillovers, and exhausted requests separately.
- Report provider-reported request cost separately from declared-price and token-estimated cost.
- Provider account billing is reconciled from provider-side usage evidence and is never inferred from request-reported cost.
- A failed or inconclusive gate leaves the candidate policy opt-in.
- Passing every evaluation gate produces a recommendation only.
- Runtime routing defaults remain unchanged until the owner explicitly approves a separate change.
- Pair and resample by prompt, keep repeats together, and record the bootstrap seed and replicate count.

## Consequences

- Reliability regressions become visible even when a policy lowers estimated cost.
- Spillover outcomes can be evaluated without changing route-reason grammar or the OpenAI-compatible API shapes.
- Small or incomplete samples may produce inconclusive reliability intervals.
- Existing cost, latency, quality, severe-failure, and cost-coverage thresholds remain stable.

## Rejected alternatives

- Treating every HTTP 200 as successful would hide malformed or incomplete streamed responses.
- Replacing the ADR 0014 gates would make the v1.0 and v1.2 results harder to compare.
- Automatically promoting a policy after the gates pass would violate the v1 compatibility promise that default routing does not change in a minor release.

## What needs a new ADR

Changing the -2 percentage-point reliability threshold, changing an ADR 0014 gate, changing route-reason grammar, or enabling a routing mode by default requires a new ADR.

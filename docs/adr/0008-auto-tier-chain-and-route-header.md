# 8. `auto` is an ordered chain of tiers, explained by `x-spillway-route`

Date: 2026-10-15
Status: accepted

## Context

v0.3 lets a client send `model: "auto"` and leave model choice to Spillway.
Selection must be deterministic, free-first, and keep ADR 0005's rule that spending is always an explicit config choice.
v0.3 has no classifier, so the only per-request signal is what the request needs (ADR 0007).
Clients and, later, the dashboard need to see why a model was chosen without reading server logs.

## Decision

- `router.auto` is an ordered list of configured tier names.
  `auto` resolves to their candidates in that order, keeping the first occurrence of a model that appears in several tiers, then filters them by ADR 0007 and spills over by ADR 0006.
- Every zero-spend tier in `router.auto` must come before every `allow_spend` tier; config that breaks this is refused.
  `auto` spends only through a listed `allow_spend` tier, and only after every zero-spend candidate was skipped or failed.
- The de-duplicated chain has at most 16 candidates, the same per-request attempt bound as one tier.
- Without `router.auto`, `auto` answers 404 and says how to configure it.
- When filtering leaves no candidate, the client gets 400 `invalid_request_error` with code `context_length_exceeded` if every skip was for size, otherwise `no_capable_model`, and a message listing each candidate and its skip reason.
- Every chat response carries `x-spillway-route`, a reason built only from fixed words, tier names and capability names, at most 256 bytes:
  `direct`; `tier <name>[; needs <caps>]; candidate <n>/<m>`; `auto[; needs <caps>]; tier <name>; candidate <n>/<m>`; and `...; no capable model` or `...; exhausted (<m> eligible)` on errors.
  `n/m` is the serving candidate's position among the eligible candidates.

## Consequences

- Routing for `auto` is reproducible from config, request body and failure history.
- Users control spend and priority with the tiers they already have; no rule language is introduced.
- The header format is a public contract that clients may parse; changing its grammar, putting request content or provider text in it, or dropping it from any chat response needs a new ADR.
- User-written routing rules, classifier tiers and cascades (v0.5) extend how the chain is chosen; they need their own ADRs.

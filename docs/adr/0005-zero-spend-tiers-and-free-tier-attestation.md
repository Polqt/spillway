# 5. Zero-spend tiers by default, with free-tier attestation

Date: 2026-10-14
Status: accepted

## Context

Spillway v0.2 routes `spillway/<tier>` requests through user-named tiers, and its purpose is to keep users on free tiers.
Whether a request costs money is only partly visible to Spillway.
Ollama and the mock provider are local and always free.
OpenRouter marks free variants with a `:free` model id suffix, and those are never billed.
Groq and Gemini expose no API field for the plan: Groq free versus Developer is account state, and a Gemini key moves to paid rates as soon as billing is enabled on its Google Cloud project, with no change to the key or the responses.

## Decision

Every tier is zero-spend unless it sets `allow_spend: true`.
In a zero-spend tier, every model must be eligible by one of these rules, checked by `serve` and `check` from config alone:

- the provider is `ollama` or `mock`;
- the provider is `openrouter` and the upstream model ends in `:free`;
- the provider is `groq` or `gemini` and sets `free_tier: true`, the user's attestation that the account or project has no paid plan or billing.

`free_tier` is accepted only on `groq` and `gemini`.
`serve` logs the attested providers at startup.
A config that breaks these rules is refused with an error naming the tier, the model and both ways to fix it.

## Consequences

- Spending is always an explicit choice in config, never a side effect of spillover.
- Groq and Gemini eligibility is only as true as the attestation; Spillway cannot detect a project with billing enabled, and the docs say so.
- Eligibility is static and offline, so `check` stays offline and routing stays deterministic.
- Checking OpenRouter's catalog pricing at runtime is deferred to v0.4 with budgets and pricing; it would be an added check that can only remove eligibility, not a change to this decision.
- Making spending the default, or treating a provider as free without a stated rule, needs a new ADR.

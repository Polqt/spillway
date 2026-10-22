# Provider instance identity and additive configuration

Date: 2026-10-21
Status: accepted

## Context

The existing `providers` map is keyed by preset names, which also appear in model references and route metadata.
Users need multiple independently authorized instances of a provider service without breaking existing configurations or creating parallel registries.

## Decision

Keep one `providers` registry and use each map key as the stable instance name.
Existing preset-name entries without `type` continue resolving to the same service and retain their current model-reference meaning.
Additional instances require an explicit `type` that resolves through the built-in provider-service registry; the implementation adapter may be shared by several service types.
Environment-referenced credentials, endpoints, model declarations, prices, free-tier attestations, quota-domain labels, and optional instance budgets belong to the named instance and never inherit from another instance.
Instance names are constrained safe slugs and are not places for credentials, email addresses, or provider account identifiers.
An explicit `instance/model` reference selects only that configured instance and preserves the existing direct-reference pass-through semantics.
The `/v1/*` request, response, SSE, and error contracts remain unchanged.
New fields are additive; older binaries may reject them under strict unknown-field validation, and downgrade guidance must require restoring an old-compatible config.

## Consequences

Existing configs and references remain valid while new instances have unambiguous identities.
Instance identity expresses routing and accounting ownership only; it does not assert independent provider capacity or authorize use outside provider terms.

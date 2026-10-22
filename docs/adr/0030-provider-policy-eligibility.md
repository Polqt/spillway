# Provider policy eligibility for multiple instances

Date: 2026-10-21
Status: accepted

## Context

Provider terms and quota scopes differ and change over time.
Allowing multiple configurations must not imply that extra credentials or accounts may be used to bypass provider limits.

## Decision

Before a provider service supports multiple instances, review dated official provider documentation for applicable terms and the scope of relevant rate, quota, and spending limits.
Record separately the scopes that are documented, the scopes that remain unknown, and the resulting shared-state defaults.
Support legitimate workload isolation only where the provider's terms permit it; never use instances to exceed shared provider limits, free-tier caps, or other restrictions.
If policy eligibility or quota scope is unclear, fail closed for automatic cross-instance selection and apply provider-service-wide shared cooldown state.
Review official sources again before implementation and release, and refresh the documented review date when provider terms or limit scopes change.
No live or paid provider calls are needed to perform policy review or config validation.

## Consequences

Provider support is based on verifiable public policy and quota documentation rather than assumptions encoded in credentials.
Providers with undocumented scopes may have more conservative routing until authoritative documentation resolves the uncertainty.

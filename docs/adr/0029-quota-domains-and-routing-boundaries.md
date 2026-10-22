# Quota domains and provider-instance routing boundaries

Date: 2026-10-21
Status: accepted

## Context

Provider credentials, configured instances, and actual quota domains are not one-to-one.
Providers may enforce limits at project, model, organization, account, billing-account, workspace, key, or upstream-service scope.
A new credential is not evidence of independent capacity.

## Decision

Keep per-instance credential rejection and circuit-breaker state separate, because credentials and endpoints can fail independently.
Associate quota cooldowns with the provider-documented scope and model where known, using operator-defined opaque shared-domain labels to join instances subject to the same limit.
When a scope or association is uncertain, apply a conservative provider-service-wide cooldown; never infer independent capacity from a different key, project, account, organization, or instance name.
Shared-domain labels organize local cooldown state only and do not authorize quota aggregation or same-service fallback.
Treat quota-domain labels as potentially identifying metadata and redact their values from diagnostics and printed config output.
Reject a virtual `auto` or `spillway/<tier>` route if its resolved candidate list contains more than one instance of a provider service type.
Direct `instance/model` references select that instance alone and never spill.
Automatic spillover remains available across distinct provider services, subject to each service's applicable limits and terms.
Optional per-instance budgets may restrict spend for an instance, while existing process-wide daily and monthly budgets remain the absolute configured aggregate eligibility ceiling across every instance and attempt.
Enforcement retains ADR 0010's existing soft-cap limitation: concurrent in-flight requests may overshoot the configured budget.

## Consequences

The routing rule cannot use a sibling credential to escape a rate limit, exhausted quota, or other provider restriction.
Conservative defaults may temporarily suppress otherwise independent projects until their verified quota domains are configured.
Spillway's quota tracking and spending estimates remain local observations and cannot replace provider-side limits or billing records.

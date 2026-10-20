# Dashboard onboarding, provider status and request playground

Date: 2026-10-18
Status: accepted

## Context

The v1.0 dashboard reports usage and routing history but does not help a first-time user understand setup or exercise a request. Its shell and APIs are protected by the browser security boundary in ADR 0012, and usage storage is optional under ADR 0009. A generic “provider status” label could incorrectly imply live reachability even though Spillway only observes provider behavior when a request is made.

## Decision

Extend the dashboard with onboarding, provider status and a request playground. Dashboard access continues to require a local key marked `dashboard: true`; `store:` remains opt-in and enables history rather than being a prerequisite for onboarding or the playground. Provider status reports only configuration/credential presence and observed runtime signals, never live health or reachability; the dashboard does not probe providers. The playground uses the existing `/v1/models` and `/v1/chat/completions` behavior, allows any configured model, and submits only after an explicit user action with a notice that the request reaches the configured provider and may incur cost. Prompts and responses are not logged or persisted by the dashboard. Existing same-origin bearer authentication, `sessionStorage` key handling, CSP, `no-store`, Host checks and secret-redaction rules remain in force.

## Consequences

The dashboard can help users before usage history exists, but history remains unavailable until they opt into `store:`. Provider status is intentionally observational and can be stale. Any new status API must be authenticated, bounded and omit key values, environment values, provider URLs and authorization headers.

# Spillway

A self-hosted gateway that gives client apps one OpenAI-compatible endpoint and sends each request on to a provider the user has configured.

## Language

### Providers and models

**Provider**:
One configured upstream service that serves models, such as Groq, Gemini or a local Ollama.
_Avoid_: vendor, backend, upstream

**Provider status**:
The dashboard summary of whether a provider is configured, whether its required credential is present, and any observed cooldown or failure signals.
It does not establish current reachability or guarantee that a request will succeed.
_Avoid_: provider health, connection status

**Model reference**:
How a client names a model: `provider/model`, for example `groq/llama-3.1-8b-instant`.
_Avoid_: model name, model id

**Upstream model**:
The model id a provider itself understands: a model reference without its provider prefix, such as `llama-3.1-8b-instant`.
_Avoid_: raw model

**Virtual model**:
A model reference Spillway resolves itself instead of passing to a provider: `auto` or `spillway/<tier>`.
_Avoid_: alias, meta-model

**Tier**:
A user-named, ordered list of model references, such as `cheap`, that a client can request as `spillway/<tier>`.
_Avoid_: class, bucket

**Capability**:
A supported request feature a model can handle: `tools`, `vision`, `json_mode` or `structured_outputs`.
_Avoid_: feature flag

**Capability declaration**:
The user's complete `models:` config entry for a model, listing its supported capabilities; `[]` means text only.
Spillway does not infer capabilities from model names, providers or past requests (ADR 0007).
_Avoid_: capability discovery

**Requirement**:
A capability or context size detected from a virtual-model request body and used to filter candidates.
Direct model references are passed through without requirement detection.
_Avoid_: constraint

**Auto chain**:
The ordered, de-duplicated candidates from the tiers in `router.auto`, with zero-spend tiers before tiers that allow spending.
_Avoid_: fallback chain

**Difficulty**:
One of the closed labels `simple`, `moderate` or `hard` that selects a user's configured tier list for `auto` routing.
Rules choose the label deterministically; an optional local classifier can choose it when explicitly configured (ADR 0014).
_Avoid_: intelligence level

**Local difficulty classifier**:
The explicitly configured, pinned Ollama model that may classify a bounded request into a difficulty label.
It is opt-in, never chooses a provider model, and falls back to deterministic rules on errors or uncertain output.
_Avoid_: AI router

**Route reason**:
The short explanation in `x-spillway-route` describing direct routing, a tier or auto candidate, and any detected requirements.
_Avoid_: routing trace

**Candidate**:
A model in the requested tier that is still eligible for one request after quota, budget and health are considered.
_Avoid_: option, target

### Keys

**Provider key**:
The user's own credential for one provider.
_Avoid_: API key, upstream key

**Local key**:
The named credential a client app presents to Spillway; its name identifies that client app.
_Avoid_: Spillway key, server key, API key

**Client app**:
A program that sends requests to Spillway, identified by the name of the local key it uses.
_Avoid_: project, consumer, caller

**Client example**:
A copyable snippet that configures a client app with the local endpoint and a local key placeholder.
It never contains a real key.
_Avoid_: SDK sample, integration

### Delivery

**Spillover**:
Sending a request to its next candidate, in tier order, before any part of the response has reached the client.
Once the client has received any of the response, a failure ends it with an error instead.
_Avoid_: fallback, failover, retry, chain

**Cooldown**:
A period during which a candidate is skipped after a rate limit or another temporary failure.
The provider's retry time is honored when it is available.
_Avoid_: backoff, pause

**Circuit breaker**:
A temporary stop on a candidate after repeated upstream failures.
The candidate becomes eligible again when the breaker's cooldown ends.
_Avoid_: breaker state

**Zero-spend tier**:
A tier that only accepts models Spillway can identify as free, unless `allow_spend: true` explicitly opts into paid models.
For Groq and Gemini, eligibility depends on the user's `free_tier: true` attestation.
_Avoid_: free tier

**Free-tier attestation**:
The user's `free_tier: true` config claim that their Groq account has no paid plan or their Gemini project has no billing.
Spillway cannot verify the claim or detect billing changes.
_Avoid_: verified free tier

**Exhaustion**:
The state where every candidate in a tier is unavailable or failed for a request.
Spillway reports each candidate and its reason in an OpenAI-shaped error.
_Avoid_: tier failure

### v0.4 observability and cost control

**Usage record**:
Metadata for one chat request and its provider attempts, including the route, token counts, cost source and cache outcome.
It never contains prompt or response text, keys, header values or provider error text (ADR 0009).
_Avoid_: request log

**Paid model**:
A model that ADR 0005 cannot identify as zero-spend eligible from local config.
Paid models can be priced and stopped by a budget; free models are never blocked (ADR 0010).
_Avoid_: billable model

**Cost estimate**:
An approximate USD cost in integer micro-USD, based on reported or estimated token usage and configured prices, or provider-reported cost.
It may differ from provider billing (ADR 0010).
_Avoid_: actual cost

**Budget**:
A local daily or monthly soft cap on estimated paid-model spend recorded by one Spillway process.
It can overshoot for in-flight requests and does not include spend outside that process (ADR 0010).
_Avoid_: hard cap

**Cache hit**:
A response served from the opt-in, in-memory exact cache for the same client app and byte-identical request body.
It costs zero and disappears when Spillway restarts (ADR 0011).
_Avoid_: cached completion

**Dashboard key**:
A local key marked `dashboard: true` that grants read access to `/api/*` usage data.
Chat-only keys do not grant dashboard access (ADR 0012).
_Avoid_: admin key

**Request playground**:
A dashboard interface where a user explicitly submits a chat request through Spillway and inspects the response and route reason.
It does not retain prompt or response text.
_Avoid_: test chat, provider console

**Fallback dashboard**:
The checked-in dashboard served by a binary built without the embedded Preact assets, such as one from `go install`.
It is a supported channel with the same content and security rules as the embedded dashboard (ADR 0021).
_Avoid_: legacy dashboard

**Catalog**:
Provider-published model metadata printed by the explicit `spillway catalog` command as a suggestion for the user's config.
It is advisory and never changes runtime routing or pricing by itself (ADR 0013).
_Avoid_: discovered model config

### v1.0 compatibility and release

**Compatibility promise**:
The guarantees Spillway makes to users about which configuration, CLI, API, header and store behaviors remain compatible across v1 minor releases (ADR 0015).
_Avoid_: backward compatibility guarantee

**Stable surface**:
The user-facing fields, commands, API shapes, headers and store schema covered by the compatibility promise (ADR 0015).
_Avoid_: public API (too narrow)

**Release candidate**:
A versioned pre-release build, tagged `vX.Y.Z-rc.N`, used to verify release artifacts on supported platforms before the final version (ADR 0016).
_Avoid_: preview release

**Provider preset**:
A built-in provider definition that supplies its default endpoint, key requirements and suggested model for configuration and setup (v1.0 plan).
_Avoid_: provider template

**Known limitation**:
A documented behavior or unsupported capability of the current release that users should account for, such as the absence of an embeddings endpoint or the lack of Windows/arm64 run testing.
_Avoid_: bug (unless behavior is unintended)

### v1.1 CLI management

**Provider inspection**:
The `spillway provider list` and `spillway provider show` views of configured presets and credential-variable presence; they do not reveal key values or contact providers.
_Avoid_: provider test

**Experimental provider add**:
The v1.1.x `spillway provider add` command that previews, backs up and atomically adds a provider preset to an existing valid config.
It accepts an environment-variable name, never a provider key value, and rejects duplicate provider instances.
_Avoid_: provider setup wizard

**Doctor probe**:
The optional-by-flag, unauthenticated local HTTP and bind checks `spillway doctor` makes against the configured listen address; it never contacts configured providers or sends a key.
_Avoid_: health check (too broad)

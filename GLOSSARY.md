# Spillway

A self-hosted gateway that gives client apps one OpenAI-compatible endpoint and sends each request on to a provider the user has configured.

## Language

### Providers and models

**Provider**:
One configured upstream endpoint that serves models, such as Groq, Gemini or a local Ollama.
_Avoid_: vendor, backend, upstream

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

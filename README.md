# Spillway

Spillway is a self-hosted gateway that exposes an OpenAI-compatible API and routes requests through configured providers.

## Run the mock setup

Run `make mock` to start the server with the offline mock provider.
The local demo key is `sk-test-spillway-mock` and the configured virtual model is `spillway/demo`.

```shell
curl http://127.0.0.1:8080/v1/chat/completions \
  -H "Authorization: Bearer sk-test-spillway-mock" \
  -H "Content-Type: application/json" \
  -d '{"model":"spillway/demo","messages":[{"role":"user","content":"Hello"}]}'
```

## Configure tiers

Tiers list candidate models in priority order.
Requests for `spillway/<tier>` try eligible candidates in that order and spill over when a provider fails before any response reaches the client.

Tiers are zero-spend by default.
Ollama and the mock provider are local, and OpenRouter models ending in `:free` are identified by their model id.
Groq and Gemini do not expose plan or billing status to Spillway, so their provider entries require `free_tier: true` for a zero-spend tier.
That setting is your attestation that the Groq account has no paid plan or the Gemini project has no billing.
Spillway checks the setting but cannot verify the claim or detect later billing changes.
Use `allow_spend: true` on a tier only when you intend to let that tier include paid models.

## Declare model capabilities and use `auto`

`models:` is the only capability source in v0.3.
Declare a complete capability list for each model that `auto` may choose for requests with capability requirements; use `[]` for text-only models.
Spillway does not infer support from model names or provider catalogs.

```yaml
models:
  groq/llama-3.3-70b-versatile:
    capabilities: [tools, json_mode]
    context_window: 131072
  ollama/llama3.2:3b:
    capabilities: []

router:
  auto: [free, smart]
```

The accepted capabilities are `tools`, `vision`, `json_mode` and `structured_outputs`.
`context_window` is optional; Spillway skips a candidate only when its conservative size estimate clearly exceeds the declaration.
`router.auto` lists configured tiers in order, with every zero-spend tier before any tier that sets `allow_spend: true`.
For requests that need capabilities, `auto` skips undeclared models; an explicit `spillway/<tier>` request can still try them.
Direct model references pass through without capability checks.
The `x-spillway-route` response header explains which route and candidate served the request.

## Usage, cost, budgets and dashboard

SQLite usage history is opt-in through `store.path`; it records request metadata, not prompts, responses, keys, headers or provider error text.
The database still reveals usage patterns and should be treated as sensitive; only one Spillway process may use a database file at a time.

Cost figures are estimates, not provider invoices.
Spillway uses provider-reported usage when available and otherwise uses conservative token estimates with prices from `pricing:`.
Prices can become stale, and cached-input discounts, reasoning tokens, image and audio pricing, per-request fees, minimum charges, taxes and currency conversion are not modelled.
Provider billing, balances and plan status are invisible to Spillway.
Groq and Gemini models configured with `free_tier: true` are counted as free based on the user's attestation, even if billing later becomes enabled.
Providers may ignore injected stream usage requests, and disconnects or mid-stream failures can leave usage estimated.
Failed paid attempts without reported usage are charged the estimated prompt cost; this can overcount a request rejected before processing or undercount hidden output generated before failure.

Daily and monthly budgets are soft caps on paid-model spend observed by this Spillway process.
Requests already in flight can overshoot a cap; spend made by other tools, machines or Spillway processes is not counted, and deleting the database resets the recorded spend.
Budgets never block free models.
The in-memory exact cache is opt-in, per client app and byte-exact; it ignores temperature, can return a stale or non-deterministic answer until expiry, and is cleared on restart.
Catalog suggestions are advisory, may be incomplete or stale, and never affect routing or pricing until reviewed and copied into config.
Gemini has no supported catalog fields, and Groq catalog data includes only context length.

The mock setup enables all v0.4 features for local exploration.
Mock models are always treated as free, so the sample price is illustrative and cannot exhaust its budget.
Paid-budget behavior is covered by the offline cross-feature test with a fake paid provider.

Use the `dashboard` key from `examples/config.mock.yaml` at `http://127.0.0.1:8080/dashboard`.
The `demo` key can call chat endpoints but cannot read dashboard APIs.

```shell
curl http://127.0.0.1:8080/v1/chat/completions \
  -H "Authorization: Bearer sk-test-spillway-mock" \
  -H "Content-Type: application/json" \
  -d '{"model":"mock/echo","messages":[{"role":"user","content":"Cache me"}]}'
```

Repeat the identical curl request to see `x-spillway-cache: hit`.
For streaming usage capture, send `{"model":"mock/echo","stream":true,"messages":[{"role":"user","content":"Stream this"}]}` and inspect `/api/decisions` with the dashboard key.
Run `spillway catalog -config examples/config.mock.yaml` to see the mock-provider skip note.
The database at `./spillway-mock.db` retains usage and budget totals after restart; the response cache does not.
For a full curl and OpenAI Python SDK walkthrough, including a local paid-model fixture and budget persistence across restart, see the [v0.4 manual verification](docs/v0.4-manual-verification.md).

## Provider data terms

Free pricing does not guarantee that a provider will not use prompts to improve or train its models.
Gemini's free tier and some OpenRouter free endpoints may use submitted prompts under their provider terms.
Review the current terms for each model before routing sensitive data through a free tier.
Spillway sends requests to the providers in your configuration and cannot change their data handling policies.

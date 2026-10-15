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

## Provider data terms

Free pricing does not guarantee that a provider will not use prompts to improve or train its models.
Gemini's free tier and some OpenRouter free endpoints may use submitted prompts under their provider terms.
Review the current terms for each model before routing sensitive data through a free tier.
Spillway sends requests to the providers in your configuration and cannot change their data handling policies.

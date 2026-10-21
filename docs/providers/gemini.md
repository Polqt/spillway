# Gemini

Gemini uses Google's OpenAI-compatible endpoint (ADR 0001).

- Key: set `GEMINI_API_KEY` and reference it as `${GEMINI_API_KEY}` in `providers.gemini.api_key`.
- Free tier: Google Cloud billing state is not exposed to Spillway. Use `free_tier: true` only to attest that the key's project has no billing; this is the user's claim, not a verification by Spillway (ADR 0005). Otherwise enable spending on the tier and configure prices for budgets.
- This provider-level attestation does not confirm that an individual model has a free quota. Check the current [Gemini pricing](https://ai.google.dev/gemini-api/docs/pricing) and [rate limits](https://ai.google.dev/gemini-api/docs/rate-limits) before adding a model to a zero-spend tier; quotas vary by model and project.
- Data terms: prompts and outputs are sent to Google when a Gemini model is selected. Review Google's current Gemini API privacy, retention and service terms before sending sensitive data; local Spillway storage settings do not control provider retention.
- Base URL defaults to `https://generativelanguage.googleapis.com/v1beta/openai`; key is required.

```yaml spillway-config
server:
  api_keys:
    - name: my-app
      key: "${SPILLWAY_KEY}"
providers:
  gemini:
    api_key: "${GEMINI_API_KEY}"
    free_tier: true
tiers:
  free:
    models: [gemini/gemini-2.5-flash]
router:
  auto: [free]
```

Verified: 2026-10-16, offline adapter tests using `httptest` fixtures; no live project billing or quota was checked.

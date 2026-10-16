# Groq

Groq uses the built-in OpenAI-compatible provider preset.

- Key: set `GROQ_API_KEY` and reference it as `${GROQ_API_KEY}` in `providers.groq.api_key`.
- Free tier: Groq account limits and plan terms can change. A model is eligible in a zero-spend tier only with `free_tier: true` and the attestation that this account has no paid plan. Spillway cannot inspect billing state (ADR 0005). Otherwise use `allow_spend: true` and configure prices when budgets are enabled.
- Data terms: prompts and outputs are sent to Groq when a Groq model is selected. Review Groq's current privacy, retention and service terms before sending sensitive data; local Spillway storage settings do not control provider retention.
- Base URL defaults to `https://api.groq.com/openai/v1`; key is required.

```yaml spillway-config
server:
  api_keys:
    - name: my-app
      key: "${SPILLWAY_KEY}"
providers:
  groq:
    api_key: "${GROQ_API_KEY}"
    free_tier: true
tiers:
  free:
    models: [groq/llama-3.3-70b-versatile]
router:
  auto: [free]
```

Verified: 2026-10-16, offline provider adapter tests using `httptest` fixtures; no live account or current free quota was checked.

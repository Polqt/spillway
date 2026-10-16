# OpenRouter

OpenRouter uses the built-in OpenAI-compatible provider preset.

- Key: set `OPENROUTER_API_KEY` and reference it as `${OPENROUTER_API_KEY}` in `providers.openrouter.api_key`.
- Free tier: only an upstream model reference ending in `:free` is zero-spend eligible by config. Eligibility comes from the model identifier, not an account attestation. Free model availability, limits and terms can change; `spillway catalog` is advisory and does not change routing or pricing (ADR 0005, 0013).
- Data terms: requests go to OpenRouter and then the selected upstream model provider. Review both OpenRouter's and that provider's current privacy, retention and service terms before sending sensitive data.
- Base URL defaults to `https://openrouter.ai/api/v1`; key is required.

```yaml spillway-config
server:
  api_keys:
    - name: my-app
      key: "${SPILLWAY_KEY}"
providers:
  openrouter:
    api_key: "${OPENROUTER_API_KEY}"
tiers:
  free:
    models: [openrouter/meta-llama/llama-3.3-70b-instruct:free]
router:
  auto: [free]
```

Verified: 2026-10-16, offline provider adapter tests using `httptest` fixtures; no live catalog, account or quota was checked.

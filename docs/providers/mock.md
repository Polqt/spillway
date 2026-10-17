# Mock

Mock is a deterministic offline provider for development and examples.

- Key: none; `api_key` is not accepted.
- Free tier: local deterministic responses have no provider usage charge and are always zero-spend eligible (ADR 0005). A configured price for a mock model does not make its requests paid.
- Data terms: request text is processed by the local Spillway process and returned as a deterministic mock response. It is not sent to an external provider unless you separately configure and route to one.
- The suggested model is `mock/echo`.

```yaml spillway-config
server:
  api_keys:
    - name: my-app
      key: "${SPILLWAY_KEY}"
providers:
  mock: {}
tiers:
  local:
    models: [mock/echo]
router:
  auto: [local]
```

Verified: 2026-10-16, offline mock provider and end-to-end tests; no external service is needed.

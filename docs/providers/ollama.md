# Ollama

Ollama runs models on a service you operate. The default URL is `http://localhost:11434`.

- Key: no API key is accepted or required. Pull a model with Ollama separately and use its exact local model tag in `tiers`.
- Free tier: local inference has no provider account or usage bill; hardware, electricity and any model license terms still apply. Do not set `free_tier` for Ollama; local providers are always zero-spend eligible under ADR 0005.
- Data terms: prompts are sent to the configured Ollama service. With a local service they stay on infrastructure you control, but review the model license and any remote service or logging configured in your environment.
- Docker (D4): when Spillway runs in a container and Ollama runs on the Linux host, use `--network host` so `localhost:11434` reaches the host service. Publish the Spillway port only on host loopback (for example `-p 127.0.0.1:8080:8080`) and keep `server.api_keys` configured. This host-network note applies to Linux; do not substitute a non-loopback plaintext Ollama URL.

```yaml spillway-config
server:
  api_keys:
    - name: my-app
      key: "${SPILLWAY_KEY}"
providers:
  ollama: {}
tiers:
  local:
    models: [ollama/llama3.2:3b]
router:
  auto: [local]
```

Verified: 2026-10-16, offline OpenAI-compatible adapter tests using `httptest` fixtures; no live Ollama daemon was checked.

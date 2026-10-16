# Network destinations

Spillway has no telemetry and makes no background network requests.
Network calls happen only when a configured provider is used, when the operator runs `spillway catalog` or `spillway eval`, or when the optional local classifier is enabled.
When no providers are configured, `serve` makes no provider requests.
The `mock` provider is offline.

## Provider destinations

The following preset URLs are the built-in destinations used by `serve` when the matching provider is configured without a `base_url` override.
The configured provider URL replaces the preset URL when `base_url` is set.

| Provider | Preset destination | Feature that contacts it |
| --- | --- | --- |
| Gemini | `https://generativelanguage.googleapis.com/v1beta/openai` | `serve` chat and model requests |
| Groq | `https://api.groq.com/openai/v1` | `serve` chat and model requests; `catalog` requests `/models` |
| OpenRouter | `https://openrouter.ai/api/v1` | `serve` chat and model requests; `catalog` requests `/models` |
| Ollama | `http://localhost:11434` | `serve` chat and model requests; `catalog` requests `/api/show` for configured models |

`spillway catalog` is explicit and contacts only configured OpenRouter, Groq and Ollama providers.
It skips Gemini and mock.
The command writes suggestions to standard output and does not change routing or configuration.

## Local classifier

The optional `router.classifier` feature sends a bounded request summary to Ollama's `/api/generate` endpoint.
It is disabled unless `router.mode: rules`, `router.classifier.model` and `providers.ollama` are configured.
The configured Ollama URL must use a loopback host, and the classifier sends no request when disabled.

## Evaluation command

`spillway eval` sends evaluation prompts to the explicitly selected Spillway-compatible endpoint.
By default that endpoint is `http://127.0.0.1:8080`; `-endpoint` and per-arm `@endpoint` values can select another destination.
The command sends no requests unless it is run with a dataset and arms.

## HTTP proxies

The default Go HTTP transport honors `HTTP_PROXY`, `HTTPS_PROXY` and `NO_PROXY` (including their lowercase forms).
These settings apply to provider, catalog, classifier and evaluation requests.
Set `NO_PROXY` for loopback destinations when local Ollama or a local Spillway endpoint must bypass a configured proxy.

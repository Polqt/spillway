# Troubleshooting

## `spillway check` reports a missing variable

Every `${NAME}` reference must resolve to a non-empty environment variable before `check` or `serve` starts. Set provider and local keys in the same environment that launches Spillway, then rerun `spillway check`. For example, in PowerShell use `$env:GROQ_API_KEY = "..."`; in POSIX shells use `export GROQ_API_KEY="..."`. Do not paste provider key values into config files or share command output that contains credentials.

## HTTP 401 versus 403

- **401 Unauthorized** means the request has no valid local Spillway key. Send `Authorization: Bearer <local-key>` for a key listed in `server.api_keys`.
- **403 Forbidden** means the key is valid but lacks permission for that endpoint. Dashboard data under `/api/*` requires a key marked `dashboard: true`; use the separate dashboard key created by `spillway init -store`.

The inbound Authorization header is not forwarded to a provider. Provider credentials are read from configured environment variables.

## Host header rejected

On a loopback listener, Spillway rejects a request whose `Host` is not a loopback address or the configured loopback host and port (ADR 0003). Use the local address from `server.listen`, such as `http://127.0.0.1:8080`, and check whether a proxy or client is rewriting `Host`. Do not disable this protection by changing network exposure casually.

## `no_capable_model`

The selected virtual model (`auto` or `spillway/<tier>`) has no eligible candidate for the request's detected requirements. Check `x-spillway-route`, the ordered tiers, and each candidate's `models.<provider/model>.capabilities` declaration. Declare the model's supported `tools`, `vision`, `json_mode` or `structured_outputs` capabilities accurately, or select a model that supports the request. Direct model references bypass capability filtering.

## HTTP 429 and budget errors

A provider quota/rate-limit response and a local budget cap both use HTTP 429, but have different causes. A provider's quota error can include a provider name and `Retry-After`; configure another provider candidate if you want cross-provider spillover. A local budget rejection uses error code `insufficient_quota` and `Retry-After` until the next budget period. Check `/api/budget` with a dashboard key. Budgets are soft caps on estimated spend recorded by this process; they can overshoot for in-flight requests and do not include external usage.

## Cache headers

When exact caching is enabled, `x-spillway-cache` reports `hit`, `miss` or `bypass`. A hit is an in-memory response for the same client app and byte-identical request body; it disappears on restart. Streaming requests bypass the cache. To bypass it for a non-streaming request, send request header `X-Spillway-Cache: bypass`. If caching is disabled, the response header is absent.

## Classifier fallback

The optional difficulty classifier needs rules mode and a configured loopback Ollama model with an explicit pinned tag. If classification errors, returns an invalid label or is uncertain, Spillway falls back to deterministic rules; the `x-spillway-route` header includes `classifier fallback`. Confirm Ollama is running and the model is available. The classifier is disabled unless configured.

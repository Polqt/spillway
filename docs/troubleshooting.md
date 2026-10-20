# Troubleshooting

## `spillway init` says the config already exists

If `spillway init -config <path>` reports that the config already exists, remember that init never overwrites a config.
Validate it with `spillway check -config <path>`, or create a separate config with `spillway init -config <new-path>`.

## `spillway check` reports a missing variable

Every `${NAME}` reference must resolve to a non-empty environment variable before `check` or `serve` starts.
Use the shell-specific command printed by `spillway check` or `spillway init` to set it in this shell, then rerun the command.
PowerShell prompts without putting the value in shell history: `$env:GROQ_API_KEY = [Net.NetworkCredential]::new('', (Read-Host -AsSecureString 'provider key')).Password`.
In bash or zsh, type the key into the terminal and press Enter; input is hidden: `read -rs GROQ_API_KEY && export GROQ_API_KEY`.
Do not paste provider key values into config files or share command output that contains credentials.

## `spillway serve` cannot use the configured address

If another process is already using the address, Spillway reports `Another process (possibly an older Spillway) is using this address` and suggests an available loopback example.
Change `server.listen` in the config, for example to `127.0.0.1:8787`, then rerun `spillway serve`.
Update client apps to use the same port.
The process may be an older Spillway server that is still running; check its startup log and stop it cleanly if appropriate.

## The store schema is newer than this Spillway binary

The database schema is newer than this binary supports.
The error reports the found schema version, the supported schema version and this binary's version.
Run the newer Spillway binary that wrote this database, or point `store.path` at a new file in the config.
Keep the existing database; do not delete it to work around the version mismatch.

## `spillway check` succeeds

After `config OK`, run the suggested command, `spillway serve -config <path>` (using the path printed by `check`), to start the gateway.
Keep the server running while client apps use it.

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

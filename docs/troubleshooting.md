# Troubleshooting

## `spillway init` says the config already exists

If `spillway init -config <path>` reports that the config already exists, remember that init never overwrites a config.
Validate it with `spillway check -config <path>`, or create a separate config with `spillway init -config <new-path>`.
Use `spillway provider list -config <path>` to inspect configured providers.
`spillway provider add <name> -config <path>` is experimental and prints a preview before saving.

## Add a provider without replacing local keys

For a valid existing config, use `spillway provider add <name> -config <path>` instead of generating a second config and swapping files.
Review its diff and backup message, then run `spillway check -config <path>`.
The command preserves the existing `server.api_keys` entries.

If `spillway provider add <name>` says that provider is already configured, Spillway allows one config entry per provider.
It never rotates accounts to evade a rate limit.
Use another provider, upgrade the provider plan, or change the configured environment variable to the key for the same account.

## `spillway check` reports a missing variable

Every `${NAME}` reference must resolve to a non-empty environment variable before `check` or `serve` starts.
Use the shell-specific command printed by `spillway check` or `spillway init` to set it in this shell, then rerun the command.
PowerShell prompts without putting the value in shell history: `$env:GROQ_API_KEY = [Net.NetworkCredential]::new('', (Read-Host -AsSecureString 'provider key')).Password`.
In bash or zsh, type the key into the terminal and press Enter; input is hidden: `read -rs GROQ_API_KEY && export GROQ_API_KEY`.
Do not paste provider key values into config files or share command output that contains credentials.
`spillway doctor -config <path>` reports which variable is missing and prints shell-specific recovery guidance without showing values.

## A variable exists in Windows settings but Spillway still reports it missing

A process inherits environment variables when its terminal starts.
If `spillway doctor` says a variable is set for your Windows user or machine but not in this terminal, open a new terminal and run `spillway doctor -config <path>` again.

## Spillway appears to be using the wrong config or provider list

`spillway doctor -config <path>` prints the absolute config path and provider names loaded from that file.
Compare them with the `config` and `providers` fields in the `spillway serve` startup log.
Stop the server and rerun `spillway serve -config <path>` when they differ.

## You cannot find or use a local key

Run `spillway doctor -config <path>` to see each local key entry's name, access, source and config line.
It never prints the key.
Client apps use the `default` entry; dashboard data requires an entry marked `dashboard: true`.

## The configured listen address is occupied

Run `spillway doctor -config <path>` to check whether the address answers like Spillway, belongs to another service, or has no listener.
It gives an OS-specific port-owner command and suggests a loopback port.
If the result is `answers like Spillway`, compare the config's provider names with the `providers` list in the server startup log to find an older process or different config.
Use `spillway doctor --no-probe -config <path>` to skip network checks.

## Gemini key shape looks wrong

`spillway doctor -config <path>` can recognize a few certain credential mistakes and may give a format advisory based on the documented Gemini prefix.
It never prints key characters and does not verify a key with Google.
Check the value in your secret manager and set `GEMINI_API_KEY` in the current shell.

## A client requires the Responses API

Spillway serves Chat Completions at `/v1/chat/completions` and model listing at `/v1/models`; it does not serve `/v1/responses`.
`spillway doctor` prints this limitation.
Configure the client to use Chat Completions or choose a client that supports it.

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

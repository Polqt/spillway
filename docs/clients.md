# Connect a client app

Run `spillway key default` to print the client key. Use `spillway key dashboard` for the dashboard. Keep both private, and never put a real key in source control or a shared example.
If you are using the downloaded Windows archive from its extracted folder, run `.\spillway.exe key default` or `.\spillway.exe key dashboard`.
If the config is elsewhere, add `-config <path>` before the key name, for example `spillway key -config config.yaml default`.

Useful config commands:

```text
spillway model list
spillway model list -provider groq
spillway tier list
spillway model replace gemini/old-model gemini/new-model -config config.yaml -dry-run
```

`model replace` shows a preview and saves a backup before writing. Restart the server after a change.
`model list` reports zero-spend eligibility from your config; Gemini model quotas must be checked against Google's current pricing and rate limits.

Spillway supports OpenAI Chat Completions at `/v1/chat/completions` and model listing at `/v1/models`.
It does not implement the OpenAI Responses API at `/v1/responses`; use a client configured for Chat Completions.

The local OpenAI-compatible endpoint is `http://127.0.0.1:8080/v1`. Examples request model `auto`, so Spillway can spill over across eligible configured providers.
Only models configured in tiers are listed or accepted as direct model requests.

## curl (POSIX)

See [`curl.sh`](../examples/clients/curl.sh):

```sh
curl http://127.0.0.1:8080/v1/chat/completions \
  -H 'Authorization: Bearer <LOCAL_APP_KEY>' \
  -H 'Content-Type: application/json' \
  -d '{"model":"auto","messages":[{"role":"user","content":"Hello from Spillway"}]}'
```

## curl (PowerShell)

See [`curl.ps1`](../examples/clients/curl.ps1):

```powershell
curl.exe http://127.0.0.1:8080/v1/chat/completions `
  -H 'Authorization: Bearer <LOCAL_APP_KEY>' `
  -H 'Content-Type: application/json' `
  -d '{"model":"auto","messages":[{"role":"user","content":"Hello from Spillway"}]}'
```

## Python

Install the OpenAI SDK with `uv run --with openai python examples/clients/python.py`. Set `OPENAI_API_KEY` to your `default` local key first. The source file uses the endpoint and model `auto` directly.

## Node.js

Install the OpenAI SDK in your project with `npm install openai`, set `OPENAI_API_KEY` to your `default` local key, then run `node examples/clients/node.mjs`.

## SDK environment variables

Set these in your shell, replacing the placeholder locally:

```sh
export OPENAI_BASE_URL=http://127.0.0.1:8080/v1
export OPENAI_API_KEY=<LOCAL_APP_KEY>
```

PowerShell:

```powershell
$env:OPENAI_BASE_URL = 'http://127.0.0.1:8080/v1'
$env:OPENAI_API_KEY = '<LOCAL_APP_KEY>'
```

## See spillover offline

Run the shipped mock config, which needs no provider account or provider key:

```sh
spillway serve -config examples/config.mock.yaml
```

The mock config includes a local `demo` key for loopback-only testing. In another terminal, send the same `auto` request from any example with `sk-test-spillway-mock` as the temporary local key. The first two mock candidates simulate rate limiting and unavailability; the third answers. The response header is:

```text
x-spillway-route: auto; tier demo; candidate 3/3
```

This fake key is only for the checked-in mock config. Do not use it with another config or expose it on a network. Python and Node examples can be run unchanged by setting `OPENAI_API_KEY=sk-test-spillway-mock` in the environment before invoking them.

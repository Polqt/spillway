# Connect a client app

Use the `default` local key from your private `config.yaml` for client apps. Do not use the `dashboard` key: it grants access to local usage data. Replace `<LOCAL_APP_KEY>` with the `default` key value when you run a command; never put a real key in source control or a shared example.

The local OpenAI-compatible endpoint is `http://127.0.0.1:8080/v1`. Examples request model `auto`, so Spillway can spill over across eligible configured providers.

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

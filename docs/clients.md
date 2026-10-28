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

Spillway supports OpenAI Chat Completions at `/v1/chat/completions`, OpenAI Responses at `/v1/responses`, Anthropic Messages at `/v1/messages`, and model listing at `/v1/models`.
Text, images, tool calls, and streaming are translated to the existing Chat Completions routing path.
Codex namespace functions are flattened for the provider and restored in Responses tool calls; provider-hosted tools such as `web_search` are omitted because Chat Completions has no equivalent.
The Responses `store`, `include`, `client_metadata`, `prompt_cache_key`, `service_tier`, `reasoning.summary`, `text.verbosity`, input-item `id`, and replayed `output_text` `annotations` and `logprobs` fields are accepted for client compatibility. Spillway does not retain Responses objects, return provider-specific included fields, or apply client metadata, cache, service-tier, summary, or verbosity hints; input-item IDs are ignored by the stateless chat adapter.
Background Responses, `previous_response_id`, computer use, and other protocol features that have no Chat Completions equivalent are not supported.

### Claude Code Messages compatibility

Claude Code `thinking`, `output_config.effort`, `context_management`, `metadata`, and `cache_control` fields are accepted as hints and are not forwarded to providers.
The accepted thinking types are `adaptive`, `disabled`, `between_tools`, and `enabled`; enabled thinking requires a `budget_tokens` value of at least 1024 and below `max_tokens`.
The accepted effort values are `low`, `medium`, `high`, `xhigh`, and `max`, and cache control accepts `ephemeral` with an optional `5m` or `1h` TTL.
These hints do not enable provider features or change model behavior.
Unknown or malformed fields are rejected with an Anthropic-shaped request error.
Server tools, background tasks, and other Anthropic features without a Chat Completions equivalent are unsupported.
Thinking and redacted-thinking blocks in prior assistant messages are ignored during translation.
Image blocks inside `tool_result` are rejected with a clear request error; send images as user content instead.

### Gemini thought signatures in tool loops

Gemini thought signatures are carried in opaque signed tool-call IDs on Responses and Messages so clients can echo them on the next turn.
Spillway restores the provider call ID and signature before forwarding replayed tool history to Gemini.
Unsigned tool history, including history that came from another provider during spillover, receives Google's `skip_thought_signature_validator` placeholder on the first tool call in an assistant step.
The placeholder lets Gemini validate history without a real signature; it does not reproduce the original reasoning state.
Signatures larger than 64 KiB are not embedded in IDs, so the placeholder fallback applies when that unsigned history is sent to Gemini.
The OpenAI Chat Completions endpoint preserves `extra_content.google.thought_signature` as provider-specific extension data.

## One-command coding client setup

Start with the config that contains your local `default` key.
If `config.yaml` is in the current directory or your home directory, setup finds it automatically.
Otherwise, pass `-config <path>`.

```powershell
spillway setup opencode
spillway setup claude
spillway setup codex
```

Each setup command updates the client's global configuration and preserves unrelated settings.
Existing files are backed up before an update.
Then start the gateway with `spillway serve -config <path>` and open the client.
For example, Codex uses its isolated profile with `codex --profile spillway`.
The profile is saved under `CODEX_HOME`, or `~/.codex` when that variable is unset.

OpenCode reads its local key from a separate `spillway.key` file in its private config directory.
Claude Code and Codex invoke `spillway key` when they need the local key.
Keep the OpenCode key file and the Spillway config private.

The `setup` command supports loopback servers only.
It maps wildcard listen addresses to `127.0.0.1` for the coding client.

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

## Use a local Ollama provider

Start Ollama, configure it with `spillway init -providers ollama`, then run the generated config.
Ollama needs no provider key, but it must be installed and running locally.
Use `spillway key default` to print the generated local Spillway key for the client examples.

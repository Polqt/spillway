# Spillway

Spillway is a self-hosted LLM gateway with one OpenAI-compatible API. It routes requests to configured providers, spills over across providers when an upstream is temporarily unavailable, and can record local usage metadata, estimate costs and enforce local budgets.

Spillway is a single Go binary. It has no telemetry and sends requests only to providers you configure. You bring your own provider keys; Spillway stores no provider credentials for you.

## Install

The current published release is `v1.0.0-rc.2`, a release candidate for evaluation; stable `v1.0.0` is not published. Download the archive for Linux, macOS or Windows from the [v1.0.0-rc.2 release page](https://github.com/Polqt/spillway/releases/tag/v1.0.0-rc.2), then verify it before extracting it. See [install and verify](docs/install.md) for SHA-256 and GitHub artifact attestation steps.

Install the current release candidate with Go:

```sh
go install github.com/Polqt/spillway/apps/spillway@v1.0.0-rc.2
```

Or pull the current release candidate Docker image:

```sh
docker pull ghcr.io/polqt/spillway:1.0.0-rc.2
```

Docker users should read [the Docker guide](docs/docker.md) for the required config mount and loopback-only port publishing.

## Five-minute offline quickstart

This starts a mock provider that needs no provider account, provider key, network access or Docker. The mock answers with the user's message, so it is useful for checking the API wiring but does not call a language model.

```sh
spillway init -yes -providers mock -store
spillway check
spillway serve
```

The generated config contains a local app key and, because `-store` was selected, a separate dashboard key. Keep `config.yaml` private and copy the generated app key from it into your client. The CLI never prints either key. In another terminal, replace `<LOCAL_APP_KEY>` with that value:

```powershell
curl.exe http://127.0.0.1:8080/v1/chat/completions `
  -H 'Authorization: Bearer <LOCAL_APP_KEY>' `
  -H 'Content-Type: application/json' `
  -d '{"model":"mock/echo","messages":[{"role":"user","content":"Hello from Spillway"}]}'
```

Use `curl` with the same headers and a single-quoted JSON body on POSIX shells. The response contains `Hello from Spillway`.

To use an OpenAI SDK, set its base URL to `http://127.0.0.1:8080/v1` and its API key to the same local app key. See [configuration](docs/configuration.md) and [provider guides](docs/providers/) for setup details.

## Use a real provider

For example, configure Groq. Set the provider key in your environment, then create a config that refers to it:

```powershell
$env:GROQ_API_KEY = "your-provider-key"
```

```sh
spillway init -yes -providers groq -attest-free-tier groq
```

`init` generates a local key in `config.yaml`, but does not print it. Copy it from the private file for your client. Only use `-attest-free-tier groq` if the Groq account has no paid plan; otherwise omit the attestation and configure a spend-enabled tier and prices as described in the [Groq guide](docs/providers/groq.md). `GROQ_API_KEY` must be present before `spillway check` or `serve`.

Then validate and start Spillway:

```sh
spillway check
spillway serve
```

For local inference, use Ollama instead: install Ollama, pull a model such as `llama3.2:3b`, then run `spillway init -yes -providers ollama`. See the [Ollama guide](docs/providers/ollama.md), especially its Docker limitation.

## Operations and limitations

Read [operations](docs/operations.md) for shutdown, store backup, retention and upgrade guidance. Known limits are listed in [limitations](docs/limitations.md); common failures are covered in [troubleshooting](docs/troubleshooting.md).

## License

See [LICENSE](LICENSE).

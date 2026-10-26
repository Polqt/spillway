# Spillway

Spillway is a self-hosted LLM gateway with OpenAI Chat Completions, OpenAI Responses, and Anthropic Messages APIs. It routes requests to configured providers, spills over across providers when an upstream is temporarily unavailable, and can record local usage metadata, estimate costs and enforce local budgets.

Spillway is a single Go binary. It has no telemetry and sends requests only to providers you configure. You bring your own provider keys; Spillway stores no provider credentials for you.

## Install

The current stable release is `v1.1.0`. Download the archive for Linux, macOS or Windows from the [v1.1.0 release page](https://github.com/Polqt/spillway/releases/tag/v1.1.0), then verify it before extracting it. See [install and verify](docs/install.md) for SHA-256 and GitHub artifact attestation steps.

Install the current release from its source tag with Go:

```sh
git clone --branch v1.1.0 --depth 1 https://github.com/Polqt/spillway.git
cd spillway
go install ./apps/spillway
```

Or pull the current Docker image:

```sh
docker pull ghcr.io/polqt/spillway:1.1.0
```

Docker users should read [the Docker guide](docs/docker.md) for the required config mount and loopback-only port publishing.
For config fields and validation, see the [configuration reference](docs/configuration.md).

## Quickstart

Start the provider you intend to use. This example uses local Ollama and requires Ollama to be running.

```sh
spillway init -yes -providers ollama -store
spillway check
spillway serve
```

The generated config contains a local app key and, because `-store` was selected, a separate dashboard key. Keep `config.yaml` private. Run `spillway key default` to print the client key, then replace `<LOCAL_APP_KEY>` below. Use `spillway key dashboard` to print the dashboard key.

```powershell
curl.exe http://127.0.0.1:8080/v1/chat/completions `
  -H 'Authorization: Bearer <LOCAL_APP_KEY>' `
  -H 'Content-Type: application/json' `
  -d '{"model":"auto","messages":[{"role":"user","content":"Hello from Spillway"}]}'
```

Use `curl` with the same headers and a single-quoted JSON body on POSIX shells. For copyable client examples, see [Connect a client app](docs/clients.md).

### Set up the dashboard

The `-store` option creates a separate dashboard key and enables local usage history.
Start Spillway, then open `http://127.0.0.1:8080/dashboard` and enter that dashboard key from your private `config.yaml`.
Run `spillway key dashboard` to print the dashboard key when prompted. The command prints only the requested key to your terminal.
The dashboard sends the key only to this local Spillway instance; choose **Forget dashboard key** to clear it from the browser tab.
To watch the same data in your terminal, run `spillway dashboard` while the server is running; it reads the dashboard key from `config.yaml`.
To use the dashboard without storing history, set `dashboard: true` on a local key under `server.api_keys` and omit `store:`.

Usage history is optional.
Without `-store`, the dashboard still shows configured provider status and includes a request playground, but it does not retain request history.
Playground prompts are sent to the selected configured provider when you submit them, and paid providers may charge for requests.
Spillway does not send telemetry.

To use an OpenAI SDK, set its base URL to `http://127.0.0.1:8080/v1` and its API key to the same local app key. See [configuration](docs/configuration.md) and [provider guides](docs/providers/) for setup details.

## Use a real provider

For example, create a Groq config, then enter your key at the masked terminal prompt:

```sh
spillway init -yes -providers groq -attest-free-tier groq
spillway provider key set groq -config config.yaml
```

`init` generates a local key in `config.yaml`. Run `spillway key default` to print it for your client. Only use `-attest-free-tier groq` if the Groq account has no paid plan; otherwise omit the attestation and configure a spend-enabled tier and prices as described in the [Groq guide](docs/providers/groq.md).

Then validate and start Spillway:

```sh
spillway check
spillway serve
```

For local inference, use Ollama instead: install Ollama, pull a model such as `llama3.2:3b`, then run `spillway init -yes -providers ollama`. See the [Ollama guide](docs/providers/ollama.md), especially its Docker limitation.

For a guided first run, run `spillway` without arguments to discover commands, then use `spillway init`, `spillway check` and `spillway serve`.
The interactive setup writes local key values only to the private config file.
Use `spillway provider key set <instance>` to enter a provider key without shell environment variables.
See [configuration](docs/configuration.md) before enabling paid models, budgets or persistent history.

Create multiple configs with `spillway init -config NAME.yaml`.
Use `spillway config list` to find configs in the current directory and `spillway config open -config NAME.yaml` to open one in your text editor.
Pass `-config NAME.yaml` to `check`, `serve` or `dashboard` to choose which config that command uses.

To connect a coding client after starting Spillway, run `spillway setup opencode`, `spillway setup claude`, or `spillway setup codex`.
See [client setup](docs/clients.md#one-command-coding-client-setup) for the supported clients and Codex profile command.

## Operations and limitations

Read [operations](docs/operations.md) for shutdown, store backup, retention and upgrade guidance. Known limits are listed in [limitations](docs/limitations.md); common failures are covered in [troubleshooting](docs/troubleshooting.md).

For setup recovery, run `spillway doctor` to inspect the selected config, environment, local keys, store and listen address without contacting providers.
Use `spillway provider list` or `spillway provider show <name>` to inspect configured presets.
`spillway provider add <name>` is experimental; see the [configuration guide](docs/configuration.md) before changing an existing config.

## License

See [LICENSE](LICENSE).

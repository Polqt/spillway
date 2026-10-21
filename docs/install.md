# Install and verify

## Download a release archive

Choose the archive for your operating system and CPU architecture from [GitHub Releases](https://github.com/Polqt/spillway/releases). Linux and macOS archives use `.tar.gz`; Windows archives use `.zip`. Release archives contain the `spillway` binary, `LICENSE`, `README.md` and `examples/config.mock.yaml`.

Download `checksums.txt` from the same release and verify the archive before extracting it.

On Linux:

```sh
sha256sum -c checksums.txt --ignore-missing
```

macOS does not include `sha256sum` by default. Use `shasum -a 256 <archive>` and compare its output with the matching archive entry in `checksums.txt`.

On Windows PowerShell, compare the archive's hash with its line in `checksums.txt`:

```powershell
Get-FileHash .\spillway_1.0.0_windows_amd64.zip -Algorithm SHA256
```

The hash must match exactly. A checksum detects a damaged or substituted download only when `checksums.txt` itself came from a trusted release page.

GitHub CLI can verify the artifact's provenance attestation against this repository:

```sh
gh attestation verify spillway_1.0.0_linux_amd64.tar.gz -R Polqt/spillway
```

Use the actual archive filename for your platform. Verification requires a supported GitHub CLI version and network access. Release provenance attestations cover release archives and the image; v1.0 does not produce cosign signatures or SBOMs.

Extract the archive, place `spillway` on your `PATH`, and run `spillway version`.

## Install with Go

With Go 1.26 or later, clone the release tag and run `go install` from the repository root:

```sh
git clone --branch v1.0.0 --depth 1 https://github.com/Polqt/spillway.git
cd spillway
go install ./apps/spillway
```

The version-suffixed package form `go install github.com/Polqt/spillway/apps/spillway@v1.0.0` does not work with this repository layout because Go looks for a module and tag rooted at `apps/spillway`.
Cloning the root module tag avoids that lookup and installs the binary into `GOBIN` or `GOPATH/bin`.
Check the version with `spillway version`.
Because `go install` does not run the dashboard asset build, it serves the supported fallback dashboard included in the source.
The fallback dashboard supports onboarding, provider status, the playground and route explanations; see [known limitations](limitations.md) for the distinction from release archives.

## Install with Docker

The current stable image is `ghcr.io/polqt/spillway:1.0.0`; use a versioned image tag. See [Docker](docker.md) for loopback-only publishing, persistent data and container restrictions.

## Upgrade and find older copies

An older `spillway` earlier on your `PATH`, or a server you started before upgrading, can keep answering after you install a new version.
List every copy your shell can find, in the order it searches them:

```powershell
Get-Command spillway -All
```

```sh
which -a spillway
```

Then compare each copy with `spillway version -verbose`, which prints the running executable's path, its version and whether it serves the embedded or the fallback dashboard.
Remove or replace the copies you no longer use.
A running server logs `spillway starting` with its version, executable and config path, so restart `spillway serve` after an upgrade and check that line.

## First run

Run `spillway init -yes -providers mock`, then `spillway check` and `spillway serve`. The mock provider needs no provider key or outbound network access. The generated local key is written only to the config file. Run `spillway key default` to print it for your client. See the [README quickstart](../README.md#five-minute-offline-quickstart).
For config fields, environment variables and validation rules, see the [configuration reference](configuration.md).

After setup, `spillway doctor` checks the selected config, environment, local keys, store, installation path and configured listen address.
It makes only unauthenticated requests to the configured local listener; use `spillway doctor --no-probe` to skip the listener check.
`spillway provider list` and `spillway provider show <name>` inspect configured presets without contacting them.
`spillway provider add <name>` is experimental and edits the config after preview and backup; read [provider management](configuration.md#provider-management) first.

For the interactive path, run `spillway init`, review the provider and optional store choices, then follow the suggested `check` and `serve` commands.
The wizard creates a dashboard key when you enable the store.
To use the dashboard without storing history, set `dashboard: true` on a local key under `server.api_keys` and omit `store:`.
Keep `config.yaml` private, then open `http://127.0.0.1:8080/dashboard` after the server starts and enter that key.
Spillway does not print it.
Run `spillway key dashboard` to print the dashboard key when prompted. Dashboard use does not require a browser extension or a separate Node installation.

The dashboard works without `store:`.
Provider status and the request playground remain available, while usage and routing history require the optional local store.
Prompts are sent to the configured provider only after you submit a playground request.
Real providers may charge for requests; Spillway does not send telemetry.
See [operations](operations.md) and [configuration](configuration.md) for data retention, budgets and provider setup.

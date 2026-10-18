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
Get-FileHash .\spillway_1.0.0-rc.3_windows_amd64.zip -Algorithm SHA256
```

The hash must match exactly. A checksum detects a damaged or substituted download only when `checksums.txt` itself came from a trusted release page.

GitHub CLI can verify the artifact's provenance attestation against this repository:

```sh
gh attestation verify spillway_1.0.0_linux_amd64.tar.gz -R Polqt/spillway
```

Use the actual archive filename for your platform. Verification requires a supported GitHub CLI version and network access. Release provenance attestations cover release archives and the image; v1.0 does not produce cosign signatures or SBOMs.

Extract the archive, place `spillway` on your `PATH`, and run `spillway version`.

## Install with Go

With Go 1.26 or later:

```sh
go install github.com/Polqt/spillway/apps/spillway@v1.0.0-rc.3
```

`go install` fetches source through the Go module proxy rather than verifying a GitHub release archive. Check the version with `spillway version`.

## Install with Docker

The current published image is `ghcr.io/polqt/spillway:1.0.0-rc.3`; use a versioned image tag. See [Docker](docker.md) for loopback-only publishing, persistent data and container restrictions.

## First run

Run `spillway init -yes -providers mock`, then `spillway check` and `spillway serve`. The mock provider needs no provider key or outbound network access. The generated local key is written only to the config file. See the [README quickstart](../README.md#five-minute-offline-quickstart).

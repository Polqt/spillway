# 26. Offline local diagnostics

Date: 2026-10-20
Status: accepted

## Context

Users need to distinguish an invalid config, a missing environment variable, a misplaced local key, a store-version mismatch, and an unexpected installation channel.
Those checks must work offline and must not expose key material or send provider credentials to any listener.

## Decision

- `spillway doctor` is a stable, read-only command that reports config, provider, local-key, store, installation, and `PATH` checks.
- Config validation uses unique placeholders for missing environment references, while the output reports only variable names and whether each value is available in the current process.
- On Windows, persistent variable checks read HKCU and HKLM registry metadata with `GetValue(name, nil)` and never read the stored value.
- Shell guidance uses secure PowerShell input, hidden POSIX or fish input, and informational persistence instructions that contain no credential value.
- Provider key checks fail only on certain mistakes such as surrounding quotes, whitespace, a `Bearer` prefix, a literal environment reference, or a documented placeholder.
- Provider-specific public prefixes are advisory warnings only, because provider formats can change.
- Diagnostics never print a provider key character, a local key value, prefix, fingerprint, hash, or exact length.
- A local-key warning may say only that a value is shorter than 32 characters, without revealing its length.
- Store inspection opens SQLite with `mode=ro`, reads `PRAGMA user_version`, and never creates, migrates, or deletes a database.
- A newer store schema instructs users to run the newer binary or choose another store path, and explicitly says to keep the existing database.
- Installation-channel detection and upgrade instructions are offline; the releases page URL is printed text and is never requested.
- `spillway` on `PATH` is resolved by filesystem inspection only; the other executable is never run.

## Consequences

- Missing or malformed local state can be diagnosed without provider calls or network access.
- A provider-prefix warning reveals one bit about an environment value, but no part of the value itself.
- A listener check may identify a compatible local server as Spillway when it returns the same unauthenticated responses; no key is sent, so that classification is harmless.
- Shell persistence instructions are informational and leave the choice of secret manager or private profile file to the user.

## Rejected alternatives

Sending a local key to the configured address would disclose it to a process that may own the port.
Reading registry values or printing key fragments would expose credentials in diagnostics.
Opening a database through the normal store path would migrate it and change the file while a read-only command is running.
Checking for a newer release would add an outbound request contrary to the local-only contract.

## What needs a new ADR

Sending credentials during diagnostics, adding an outbound diagnostic request, changing store files, or exposing key material requires a new ADR.

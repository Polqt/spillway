# Local Config Discovery and Opening

Status: implementation complete on 2026-10-23; release remains separate.

## Goal

Let a user find, create, open, and explicitly select multiple local Spillway config files without changing the default config used by existing commands.

## Scope

- Add `spillway config list` to list Spillway config files in the current directory by path only.
- Detect valid Spillway YAML by its top-level `server` and `providers` mapping keys, including files without a `.yaml` extension.
- Add `spillway config open` with an optional `-config PATH` flag that defaults to `config.yaml` and launches the configured or platform-default text editor.
- Document `spillway init -config PATH` as the way to create another config and the existing `-config PATH` flags as the way to select one for each command.
- Do not persist a selected default config or change existing commands' path behavior.
- Preserve sample configs and historical YAML fixtures.
- Do not delete `jepoy` or `pol`; neither file exists in the current repository root.

## Acceptance

- `config list` prints sorted paths without parsing or printing secret values.
- `config list` includes valid extensionless and `.yaml` or `.yml` configs and skips non-config YAML and directories.
- `config open` opens only the selected existing config and returns a clear error when the path or editor is unavailable.
- Editor execution passes the config path as an argument without invoking a shell.
- Tests cover list filtering, ordering, no-config output, explicit paths, and safe editor errors.
- Docs explain how to create multiple configs, list them, open one, and use it with `check`, `serve`, and `dashboard`.
- `make check` passes.

## Release note

The requested `v1.1.0` publication remains a separate release step.
The current local checkout has uncommitted changes on a backup branch, and the remote has no `v1.1.0-rc.1` tag or `release/v1.1` branch.
A verified release candidate and clean release commit are required before publishing the stable tag.

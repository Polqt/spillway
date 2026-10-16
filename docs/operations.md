# Operations

## Shutdown

Send the process a normal termination signal (Ctrl+C in a foreground terminal, or the service manager's stop signal). Spillway stops accepting new connections and gives active requests up to `server.shutdown_timeout` to finish. The default is `10s`; allowed values are `1s` through `5m`. Requests still running after the grace period are cancelled. Set an orchestrator stop grace period at least as long as this value.

## Store backup and retention

The SQLite store is optional and contains request metadata such as model, route, token counts, estimated cost and status. It never stores prompts, responses, provider keys, header values or provider error text. Protect it as sensitive operational data. Only one Spillway process may use a store file at a time.

For a simple consistent backup, stop Spillway cleanly, copy the configured database file and then restart it. Keep backups access-controlled and test restoring them. Do not copy a live SQLite database file with ordinary file-copy tools.

`store.retention` defaults to `90d` and accepts `1d` through `3650d`. Spillway removes records older than the configured retention period. Keep retention long enough for configured budgets: at least two days for a daily budget and 35 days for a monthly budget.

## Upgrades within v1

Within v1, the documented config fields, stable CLI commands and flags, `/v1/*` and `/api/*` response shapes, `x-spillway-*` headers and store schema are compatibility surfaces (ADR 0015). Minor versions may add config fields, commands, flags and response fields. Read each release's notes before upgrading.

Back up the config and store, stop the old process, install the new version and run `spillway check` with the intended config before serving traffic. New config fields are additive, but config parsing is strict: an older binary rejects fields introduced by a newer release instead of ignoring them.

## Downgrade to v0.5

v1.0 makes no store schema change, so a v0.5 binary can open a store written by v1.0. Stop Spillway, retain a backup, restore the v0.5 binary and use the config saved before the v1 upgrade. If the config was edited to use newer fields, remove those fields from the downgrade copy before running `spillway check`; v0.5 rejects unknown fields. Do not let two versions open the same store concurrently.

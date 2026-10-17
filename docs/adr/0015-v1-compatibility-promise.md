# 15. v1.0 compatibility promise

Date: 2026-10-16
Status: accepted

## Context

v1.0 is Spillway's first public release, and users will pin it in scripts, containers and SDK configurations.
Until now each milestone could change behaviour freely, although every milestone so far kept older configs loading.
Spillway's surface has several parts: the config file, CLI subcommands and flags, exit codes, the OpenAI-compatible `/v1/*` API, dashboard `/api/*` endpoints, `x-spillway-*` headers, and the SQLite store schema.
Some commands, such as `eval` and `eval-score`, exist for maintainers' routing studies and are still changing.
The Go module proxy caches every published version permanently, so a released tag cannot be withdrawn, only retracted.

## Decision

- Spillway follows Semantic Versioning from `v1.0.0`.
  A breaking change to the stable surface needs a new major version and a new ADR.
- The stable surface is:
  - the config schema: every field, default, bound and validation rule;
  - the CLI subcommands `serve`, `check`, `init`, `version` and `catalog`, their flags, and the exit codes 0 (success), 1 (failure) and 2 (usage error);
  - request, response, SSE and error shapes on `/v1/*`, and `GET /healthz`;
  - the field names and meaning of `/api/*` responses;
  - the `x-spillway-provider`, `x-spillway-model`, `x-spillway-route` and `x-spillway-cache` header names, and the route grammar of ADRs 0008 and 0014;
  - the store schema, which only moves forward through embedded migrations (ADR 0009).
- `eval`, `eval-score` and `eval-report` are experimental: they are documented as such and may change in a minor release.
- Log lines, `check` output layout, dashboard page layout and error message wording are not stable; error `type` and `code` values on `/v1/*` and `/api/*` are.
- Every example config from the final commit of v0.1 through v0.5 must load unchanged; fixtures copied from those commits lock this in the test suite.
- Minor releases may add config fields, subcommands, flags and response fields.
  An older binary rejects a config that uses a newer field, because unknown fields are errors; release notes name every new field.
- `server.shutdown_timeout` is an additive config field with a 10s default and a supported range of 1s to 5m.
  It sets the graceful drain period before remaining request contexts are cancelled.
- v1.0 itself changes no store schema, so a database written by v1.0 still opens with v0.5.
- Default routing does not change in a minor release: configs without `router.mode` keep legacy `auto`, and the classifier stays disabled unless configured.
- A broken release is fixed forward with a patch release; it is never deleted or re-tagged, and a release unsafe to use is retracted in `go.mod`.

## Consequences

- Users can upgrade within v1 without editing configs or clients.
- Tests own the promise: historical config fixtures, the route-grammar tests and the OpenAI-shape tests fail if the surface breaks.
- Evaluation tooling can keep evolving without a major version.
- Strict unknown-field rejection makes downgrades fail loudly rather than silently ignore new settings; the upgrade notes say so.
- Removing anything from the stable surface, changing a default routing behaviour, or editing a released store migration needs a new major version and ADR.

## Rejected alternative

Keeping v1 releases free to change their interfaces would leave users unable to rely on configs, scripts or stored data after upgrading.
Treating all commands and output as stable would also freeze experimental evaluation tools and incidental presentation details.

## What needs a new ADR

Changing the stable surface or its compatibility guarantees, changing default routing behaviour, or editing a released store migration requires a new ADR.

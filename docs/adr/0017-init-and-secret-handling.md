# 17. `spillway init` writes key references, never provider keys

Date: 2026-10-16
Status: accepted

## Context

v1.0 adds `spillway init` so a new user can reach a working config in minutes.
A setup wizard is a natural place to ask for API keys, but a key typed into a prompt ends up in shell history, terminal scrollback or a plain config file.
The hard rules forbid collecting credentials other than the user's own provider keys, and forbid keys in logs, errors, config printouts and responses.
Spillway already reads provider keys from `${VAR}` references in `config.yaml`.
Every listen address needs at least one local key (ADR 0003), and generating one by hand is the slowest step of a first run.
Groq and Gemini are zero-spend only with an explicit `free_tier: true` attestation (ADR 0005).

## Decision

- `init` never asks for, reads, prints or writes a provider key value.
  It writes only `${VAR}` references, using each provider's default variable name, and reports for each variable only whether it is set.
- `init` generates one local key from `crypto/rand` (32 bytes, base64url, prefix `sk-spillway-`) and writes it as a literal into the new config file.
  The key is never printed; the next steps tell the user where it is stored.
- The config file is created exclusively (`O_CREATE|O_EXCL`) with mode 0600, and an existing file is never overwritten.
  On Windows the mode is not enforced, and the docs say so.
- Free-tier attestation for Groq and Gemini is a separate yes/no question that defaults to no.
  An unattested provider is configured but left out of the zero-spend tier, and `init` explains why.
- `init` writes one zero-spend `free` tier with one suggested model per eligible provider, `router.auto: [free]`, and optionally `store:` with a separate dashboard key.
  It never writes `allow_spend` tiers, pricing, budgets, routing rules or the classifier.
  Suggested model ids are labelled as suggestions that may be outdated.
- Every prompt has a flag, so `init` runs non-interactively; with `-yes`, an unanswered required choice is an error naming its flag.
- `init` validates the config it is about to write with the normal parser, supplying placeholders for referenced variables, and refuses to write an invalid file.
- `init` makes no network calls.

## Consequences

- Provider keys stay in the user's environment or secret manager; `init` cannot leak one because it never handles one.
- The local key lives in a file readable only by the owner on Unix; anyone who can read the file can call the gateway, as with any API key file.
- Spending stays an explicit edit: a config from `init` can never spend money.
- Generated configs are valid by construction and covered by tests for every provider combination.
- Prompting for provider keys, storing them encrypted, overwriting configs, or generating spending tiers needs a new ADR.

## Rejected alternative

Asking users to enter provider keys during setup would put secrets in terminal input and create another path for accidental disclosure.
Requiring users to invent and print a local key would add setup steps and could expose that key in shell history or terminal output.

## What needs a new ADR

Reading or storing provider key values, changing how the generated local key is stored or protected, overwriting an existing config, or generating paid tiers requires a new ADR.

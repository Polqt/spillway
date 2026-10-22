# Configuration reference

Spillway reads YAML from `config.yaml` by default. Unknown fields, duplicate fields and invalid values fail during `spillway check` and before serving starts.
Provider keys use `${NAME}` environment references; `spillway init` generates local API keys in the private config file, and config output redacts secrets.
After init, use the `default` key entry for client apps and the `dashboard` entry for dashboard data when init created one with `-store`.
See [install](install.md) for the first-run sequence and [client examples](clients.md) for SDK setup.

## Provider management

`spillway provider list -config <path>` shows configured instances, their provider service types and whether each required environment variable is set.
`spillway provider show <instance> -config <path>` shows one instance's safe configuration metadata.
Neither command prints provider key values or makes provider requests.

`spillway provider add <type> -config <path>` adds the canonical legacy instance.
`spillway provider add <type> --name <instance> -config <path>` adds a separately named instance, for example `spillway provider add gemini --name gemini-research --key-env GEMINI_RESEARCH_API_KEY`.
Named aliases must be short, lowercase, non-identifying slugs; do not use an email address or account, project, organization or person identifier.
When no `--key-env` is supplied, the command derives an environment-variable name from the alias, such as `GEMINI_RESEARCH_API_KEY`.
Each instance must use its own environment-variable reference; this does not prove that the credential has independent quota or authorization.
The experimental command validates the existing config, previews the provider entry and proposed tier changes, creates a private backup, then atomically saves the edit.
It does not accept provider key values; set the named environment variable in your shell.
Use `--dry-run` to inspect the preview without writing.
An instance addition does not change `auto` or any existing virtual route.
Use an explicit model reference such as `gemini-research/gemini-2.5-flash` to select it.
Virtual routes reject two distinct instances of one provider service, and no failure automatically rotates to another instance of that service.
The gateway reads configuration at startup; restart it after a config change.

## Named provider instances

Each `providers.<instance>` entry can use `type` to identify a supported provider service.
Legacy entries such as `providers.gemini` keep their existing meaning without a `type` field.
An instance has its own endpoint, environment reference, free-tier attestation, optional quota-domain labels and optional local budget.
Credentials, endpoints, attestations and instance budgets are never inherited from another entry.

```yaml spillway-config
server:
  api_keys:
    - name: example-app
      key: "${SPILLWAY_KEY}"
providers:
  gemini:
    api_key: "${GEMINI_API_KEY}"
    free_tier: true
  gemini-research:
    type: gemini
    api_key: "${GEMINI_RESEARCH_API_KEY}"
    free_tier: true
    quota_domains:
      project: research-project
      billing_account: google-billing
    budget:
      monthly_usd: 2.00
store:
  path: ./spillway.db
  retention: 35d
tiers:
  free:
    models: [gemini/gemini-2.5-flash]
router:
  auto: [free]
```

The aliases in `quota_domains` are opaque local labels to join instances that share a provider-documented quota scope.
Do not put actual project, account or organization identifiers there.
If the upstream response does not identify its quota scope, Spillway applies a provider-service-wide cooldown, even when multiple credentials are configured.
A documented model-scoped limit is shared for that service and model; other models remain eligible only when the provider's policy permits it.
The per-instance budget is an additional restriction; the process-wide daily and monthly budgets remain the aggregate spending ceiling across every instance and spill attempt.

An explicit reference such as `gemini-research/gemini-2.5-flash` selects only that instance and never spills to its sibling.
Existing `gemini/...`, `groq/...`, `auto` and `spillway/<tier>` meanings remain unchanged unless a virtual route is invalid under the same-service uniqueness rule.
OpenAI-compatible `/v1/*` request and response shapes are unchanged.

Run `spillway doctor -config <path>` to diagnose config selection, missing environment variables, local key entries, store schema, install channel and the configured listen address.
It probes only that listener with unauthenticated requests and does not contact providers.
`--no-probe` skips all listen checks.

## Complete example

This example uses the mock provider and needs no network access or provider credential.

```yaml spillway-config
server:
  listen: "127.0.0.1:8080"
  api_keys:
    - name: my-app
      key: "${SPILLWAY_KEY}"
logging:
  level: info
providers:
  mock: {}
tiers:
  free:
    models: [mock/echo]
router:
  auto: [free]
```

## Field reference

| Field | Type | Default | Bounds and validation | Error hint | Decision |
|---|---|---|---|---|---|
| `server.listen` | string `host:port` | `127.0.0.1:8080` | Port 0–65535; host must be an IP or valid hostname. Loopback listens also reject foreign `Host` values. | Use a valid `host:port`; loopback is safest for local use. | ADR 0003 |
| `server.api_keys` | list of strings or `{name, key, dashboard}` | none; required | At least one key on every listen address. A string entry gets the generated name `key-N`; named keys: `name` is 1–64 lowercase letters, digits or hyphens; key is non-empty and has no whitespace. Names and keys must be unique. `dashboard` is boolean and defaults false. | Add a long random local key even on loopback; give each app a different name and key. | ADR 0003, 0012 |
| `server.shutdown_timeout` | duration string | `10s` | `1s` to `5m`. | Use a duration in that range, for example `30s`; align Docker stop grace with it. | ADR 0015 |
| `logging.level` | enum string | `info` | `debug`, `info`, `warn` or `error`. | Choose one of the four supported levels. | v1.0 logging configuration |
| `providers.<instance>` | mapping | none; at least one provider required | Legacy preset names remain valid; additional aliases require an explicit supported `type`. At most 32 instances; `auto` and `spillway` are reserved. | Add a supported service; `mock: {}` is the offline option. | ADR 0001, 0028 |
| `providers.<instance>.type` | provider service type | inferred only for legacy preset keys | Built-ins: `gemini`, `groq`, `mock`, `ollama`, `openrouter`. DeepSeek adapter support is not included. | Use a supported service type and a safe alias. | ADR 0028, 0030 |
| `providers.<name>.api_key` | environment reference string | none | Required for Gemini, Groq and OpenRouter; those values must use `${VARIABLE}`. Not accepted for Mock or Ollama. | Set the named environment variable and use its exact reference in YAML. | ADR 0017 |
| `providers.<name>.base_url` | absolute URL string | provider preset URL | Only `http` or `https`; HTTP is allowed only for loopback. No userinfo, query or fragment. | Use the provider's HTTPS endpoint; local Ollama can use `http://localhost:11434`. | ADR 0001 |
| `providers.<name>.timeout` | duration string | `120s` | Greater than zero and at most `30m`. | Use a positive Go duration such as `30s`. | Baseline provider config |
| `providers.<instance>.free_tier` | boolean | `false` | Only supported for Groq and Gemini. Each instance's claim applies only to its associated account or project. | Without the attestation, put its model in an `allow_spend: true` tier. | ADR 0005 |
| `providers.<instance>.quota_domains` | scope-to-alias mapping | unknown scope defaults to service-wide cooldown | Gemini: `project`, `billing_account`; Groq: `organization`, `project`; OpenRouter: `account`, `workspace`, `member`, `key`. Alias labels are at most 32 safe characters and are hidden by diagnostics. | Use the same alias only when instances are known to share that provider-documented scope. | ADR 0029, 0030 |
| `providers.<instance>.budget.daily_usd` / `.monthly_usd` | positive decimal USD | unset | Additional per-instance soft cap; requires `store.path`, with `2d`/`35d` retention respectively. Cannot raise the process-wide cap. | Add the store and retain the full budget period. | ADR 0010, 0029 |
| `tiers.<name>` | mapping | none | Name matches `[a-z0-9-]{1,64}`; `auto` is reserved. | Use a short lowercase tier name. | ADR 0005, 0006 |
| `tiers.<name>.models` | list of `instance/model` strings | none; required | 1–16 distinct printable references; instance must be configured. Expanded virtual routes reject two distinct instances of one service type. | Use one instance per provider service in a virtual route; use explicit references for direct instance selection. | ADR 0005, 0006, 0029 |
| `tiers.<name>.allow_spend` | boolean | `false` | A true value explicitly permits paid models in this tier. | Keep omitted or false for a zero-spend tier. | ADR 0005 |
| `models.<instance/model>.capabilities` | list of strings | no declaration | Required for every entry; allowed values: `tools`, `vision`, `json_mode`, `structured_outputs`. No duplicates; `[]` means text only. Model must be in a tier. | Declare the complete known capability set, or use `[]`. | ADR 0007 |
| `models.<instance/model>.context_window` | integer tokens | unset | Optional, 1–10,000,000. Only filters virtual-model routing when the conservative request estimate exceeds it. | Use a positive token count from a trustworthy model specification. | ADR 0007 |
| `router.auto` | ordered list of tier names | unset | Required when using `auto`; at least one configured tier, no duplicates, no more than 16 resolved distinct candidates. Zero-spend tiers must precede spend-enabled tiers. | List only configured tiers, free first. | ADR 0008 |
| `router.mode` | enum string | legacy ordered auto chain | Only `rules` is accepted. | Remove the field or set `mode: rules`. | ADR 0014 |
| `router.difficulty` | mapping of labels to tier lists | unset | In rules mode all of `simple`, `moderate` and `hard` are required; each non-empty and lists only `router.auto` tiers without duplicates. | Add a non-empty ordered fallback list for every label. | ADR 0014 |
| `router.rules` | ordered list of rule mappings | empty | At most 32. First matching rule wins; each rule needs a non-empty `when` and a `difficulty` of `simple`, `moderate` or `hard`. | Keep rules bounded and use an allowed difficulty label. | ADR 0014 |
| `router.rules[].when` | mapping of conditions | none; required | At least one condition. Conditions are combined with AND; duplicate condition sets are rejected. | Add one or more supported condition fields. | ADR 0014 |
| `router.rules[].when.estimated_input_tokens_gte` | integer tokens | unset | 1–2,000,000. | Use a positive threshold within the limit. | ADR 0014 |
| `router.rules[].when.requested_output_tokens_gte` | integer tokens | unset | 1–2,000,000. | Use a positive threshold within the limit. | ADR 0014 |
| `router.rules[].when.tools_present` | boolean | unset | At least one condition is required per rule; conditions combine with AND. | Use `true` or `false`. | ADR 0014 |
| `router.rules[].difficulty` | enum string | none; required | `simple`, `moderate` or `hard`. | Use one of those labels and configure its tier list. | ADR 0014 |
| `router.classifier` | optional mapping | disabled | Only supported with `router.mode: rules`; requires an explicitly configured Ollama provider at a loopback URL. | Configure local Ollama and rules mode, or remove this section. | ADR 0014 |
| `router.classifier.model` | pinned Ollama model reference | unset | Optional; requires `router.mode: rules` and configured loopback Ollama. Must include a tag other than `latest`. | Use a local reference such as `ollama/llama3.2:3b`. | ADR 0014 |
| `store.path` | file path string | store disabled | Parent directory must exist; path must name a file, not a directory. One Spillway process per database. | Create the parent directory or choose an existing one. | ADR 0009 |
| `store.retention` | duration string or whole days | `90d` | `1d`–`3650d`. Must be at least `2d` with a daily budget and `35d` with a monthly budget. | Increase retention to cover the budget window. | ADR 0009, 0010 |
| `pricing.<provider/model>.input_usd_per_mtok` | decimal USD per million input tokens | none | Required with output price; 0–10000, at most 6 fractional digits; provider must be configured. | Set both input and output prices using decimal values, without exponent notation. | ADR 0010 |
| `pricing.<provider/model>.output_usd_per_mtok` | decimal USD per million output tokens | none | Required with input price; 0–10000, at most 6 fractional digits; provider must be configured. | Set both input and output prices using decimal values, without exponent notation. | ADR 0010 |
| `budgets.daily_usd` | positive decimal USD | unset | At most 6 fractional digits; maximum `9223372036854.775807`; requires `store.path` and at least `2d` retention. | Add a store and retain at least two days. | ADR 0010 |
| `budgets.monthly_usd` | positive decimal USD | unset | At most 6 fractional digits; maximum `9223372036854.775807`; requires `store.path` and at least `35d` retention. | Add a store and retain at least 35 days. | ADR 0010 |
| `cache.exact` | boolean | `false` | Enables the in-memory exact response cache. | Set true only if identical request bodies may reuse a prior answer. | ADR 0011 |
| `cache.ttl` | duration string | `1h` | `1s`–`24h`. | Choose a duration in that range. | ADR 0011 |
| `cache.max_mb` | integer MiB | `64` | 1–1024. | Choose an integer in that range. | ADR 0011 |

The exact cache is process-memory only, per client app, and clears on restart. It can return an earlier answer even if temperature changes.
The SQLite store is opt-in and records usage metadata, not prompts, responses, headers, keys or provider error text.
Budgets estimate spend observed by this process, stop paid models after a soft cap, and can overshoot for requests already in flight.

## Environment variables

Use `${NAME}` references for provider keys and optional local API keys. Every referenced variable must exist and be non-empty when loading the config. `spillway init` creates a complete config for the selected providers, including local keys; protect that file and keep provider secrets in your shell environment or secret manager.

## Validation errors

`spillway check -config config.yaml` validates without starting the server. Errors name the config path and give the correction when possible; secret values are redacted. Unknown and removed fields are errors rather than silently ignored.

For provider-specific credential, free-tier and data handling notes, see the [provider guides](providers/).

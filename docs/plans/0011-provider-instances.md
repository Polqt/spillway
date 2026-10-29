# Provider Instances

Status: complete; shipped in v1.0.0 (#15); cross-platform CI passes on `main`

## Goal

Allow one Spillway configuration to name multiple independently authorized instances of a supported provider service for legitimate project, account, environment, or workload isolation.
Each instance has its own environment-referenced credential, routing identity, endpoint settings, and optional spend budget.
Existing `providers.<preset>` configurations and model references continue to load unchanged.
The feature preserves OpenAI-compatible APIs and supports automatic spillover only across distinct provider services.

Implementation is authorized for the ticket scope recorded below.
No paid or live provider calls are authorized or required.
Release and publication remain outside this implementation scope.
The no-quota-evasion hard rule remains in `AGENTS.md`.

## Approved architectural decisions

1. Use one provider-instance registry, with legacy preset names preserved and new entries using an explicit provider-service `type`.
2. Reject `auto` and `spillway/<tier>` virtual routes whose resolved candidates contain more than one instance of the same provider service type.
3. Permit an explicit `instance/model` reference to select exactly one configured instance, with no spillover from that direct reference.
4. Associate cooldown state with verified shared quota domains where known and default to conservative provider-service-wide state when scope or association is uncertain.
5. A different credential or instance name never proves independent capacity or permits exceeding shared provider limits.
6. Optional per-instance budgets may further restrict spend; the existing process-wide daily and monthly budgets remain the absolute configured aggregate eligibility ceiling across instances and attempts.
   Enforcement retains ADR 0010's existing soft-cap limitation: concurrent in-flight requests may overshoot the configured budget.

## Current design and compatibility boundary

`config.Config.Providers` is currently a map keyed by a provider preset name.
That key is also used in model references, tiers, capability declarations, pricing, quota tracking, construction in `internal/cli/serve.go`, model listing, and dashboard state.
The current schema accepts one configured instance for each preset name.

Plan 0010 deferred a second instance of one provider and required a schema ADR.
ADR 0006 defines candidate failure behavior and direct-reference behavior but predates a distinction between an instance identity and its provider service type.
The proposal keeps its failure classes, ordering, cooldown, and stream-commit decisions while clarifying that candidates in a virtual route must have distinct provider service types.

Legacy config entries such as `providers.gemini` continue resolving to Gemini without a `type` field.
New names require an explicit `type` resolving to a supported built-in provider service.
Existing model references such as `gemini/gemini-2.5-flash` retain their exact meaning.
New references such as `gemini-research/gemini-2.5-flash` name the instance in the first segment.
The provider service type must remain distinct from the implementation adapter: Gemini and the Groq/OpenRouter presets may share an HTTP client implementation while remaining separate provider services for route validation.

## Proposed config shape

Extend each `providers.<instance>` mapping with optional `type`, `quota_domains`, and `budget` fields.
When `type` is omitted, the mapping key must be one of today's legacy preset names and resolves exactly as it does now.
When `type` is present, it must name a supported provider service, and the mapping key must be a valid, non-reserved instance slug.
Credential values remain environment references; no API key, email address, account identifier, or project identifier is stored in config.

```yaml
providers:
  gemini:                              # Existing syntax remains valid.
    api_key: "${GEMINI_API_KEY}"
    free_tier: true
  gemini-research:
    type: gemini
    api_key: "${GEMINI_RESEARCH_API_KEY}"
    free_tier: true
    quota_domains:
      project: research-project         # Opaque operator-defined label.
      billing_account: google-billing   # Opaque operator-defined label.
    budget:
      monthly_usd: 2.00
  groq-staging:
    type: groq
    api_key: "${GROQ_STAGING_API_KEY}"
    quota_domains:
      organization: groq-org
  deepseek-prod:
    type: deepseek
    api_key: "${DEEPSEEK_API_KEY}"
    quota_domains:
      account: deepseek-account
```

The labels correlate known shared scopes across configured instances; they are not provider identifiers or credentials.
The implementation must reject duplicate or conflicting domain declarations where they make state ambiguous.
If a scope is not documented or an instance cannot be associated with it safely, use provider-service-wide shared state.
DeepSeek is not one of the currently listed v1.0 presets; its provider preset and adapter support are prerequisites for using `type: deepseek` and require their own implementation scope.

## Unified registry and state model

The registry resolves each configured entry to an immutable descriptor containing instance name, provider service type, adapter, credential reference, endpoint configuration, model metadata, quota-domain labels, and optional instance budget.
The instance name remains the external model-reference identity and safe display label.
The provider service type is the stable policy and virtual-route uniqueness identity.

- **Credentials:** one secret reference and one redaction boundary per instance; no credential inheritance or key pooling.
- **Quota domains:** verified project, organization, account, billing-account, workspace, or model scope labels as applicable to that provider.
  Cooldown state is keyed by provider service, documented scope, configured domain label, and model where the upstream limit is model-specific.
  A parent-scope block affects every instance assigned to that domain.
- **Unknown scope:** when the provider response does not reveal the affected scope, or config does not identify it, use a provider-service-wide cooldown rather than assuming separate capacity.
- **Known model scope:** when the adapter can positively identify a provider-documented model limit, share cooldown state for that provider service and model only. Model cooldown keys are bounded by configured model references; unconfigured or ambiguous model scope falls back to service-wide state.
- **Circuit breakers and credential rejection:** state is per instance because endpoint and credential failures can be independent.
  This state must not trigger selection of a sibling instance of the same provider service.
- **Budgets:** existing process-wide daily/monthly budgets aggregate estimated spend from every instance and attempt and remain the absolute configured eligibility ceiling.
  An optional instance budget is an additional lower ceiling, never a way to enlarge the aggregate budget.
  Enforcement retains ADR 0010's soft-cap limitation: concurrent in-flight requests may overshoot the configured budget.
- **Free-tier and pricing:** attestations, prices, and spend eligibility remain attached to the selected instance.
  An attestation is not an upstream verification and must not override shared provider limits.

Quota-domain state is local best-effort accounting, not a replacement for provider enforcement.
The gateway cannot discover every account relationship or external request made outside this process.
If parent and child limits both apply, a request is eligible only when none of its applicable shared domain cooldowns or budgets blocks it.

## Routing and selection rules

- A direct `instance/model` reference selects exactly that instance and retains current direct pass-through semantics.
- A direct reference never spills to any other instance, whether the selected request fails for quota, payment, authorization, model, transport, timeout, or server reasons.
- A virtual route resolves its ordered candidates and rejects duplicate provider service types in the complete route, including across tiers expanded by `router.auto` or difficulty rules.
- Different provider service types remain eligible for the existing ordered spillover behavior.
- Candidate failure classes and stream commit behavior remain as defined in ADR 0006.
- There is no randomization, load balancing, retry, or route fallback between two instances of the same provider service.
- Capability, context-window, zero-spend ordering, pricing, and process-wide budget checks continue to apply to the selected instance.
- Error summaries and route reasons may show safe instance names and provider service types but never credentials, raw upstream error bodies, or guessed account identity.

This makes instance selection for a same-service workload explicit in the requested model reference or in a route containing only that instance.
It prevents quota state or an upstream failure from causing the router to switch to another credential for the same provider service.

## Provider-instance CLI design

### Recommendation for the first implementation slice

Keep the first CLI slice to the existing planned `provider list`, `provider add`, and `provider show` commands, extending their config model to identify safe named instances.
Do not add `provider use`, `provider prefer`, or `provider current` in this slice.
As previously proposed, `provider use` would save only a local CLI preference; it would not alter the gateway or any application request.
That behavior makes `use` misleading, while renaming it to `prefer` still offers little value before the CLI can submit interactive requests.
An explicit request selects an instance with the already proposed `instance/model` model reference.
This keeps CLI state out of request routing and preserves the existing meaning of `model: auto` and named virtual routes.

```text
spillway provider list [-config PATH]
spillway provider add <type> --name <instance> [-config PATH] [existing add flags] [--quota-domain SCOPE=ALIAS ...]
spillway provider show <instance> [-config PATH]
```

### First-slice command behavior

- **`provider list`** lists configured instances, provider service types, and whether each referenced environment variable is available in this CLI process.
  It makes no provider call and never prints an environment value or credential.
- **`provider add <type> --name <instance>`** adds a new named instance to the selected config through the existing validated, previewed, backed-up, atomic config editor from Plan 0010.
  `--name` must be a user-chosen safe alias matching `[a-z][a-z0-9-]{0,31}` (for example, `gemini-research` or `groq-staging`).
  Reject email addresses, UUIDs, long numeric identifiers, provider-key-like values, reserved names, and duplicate aliases.
  This format check cannot prove that an alias is non-identifying, so the prompt must tell users not to use real account, project, organization, or person identifiers.
  The command accepts environment-variable names, not provider credential values, and lists the proposed config changes before writing.
  Optional `--quota-domain SCOPE=ALIAS` flags may declare documented shared scopes using safe opaque aliases; unsupported scopes are rejected, and omitting a scope keeps the conservative provider-service-wide default.
- **Existing `provider add` compatibility:** keep the Plan 0010 spelling `provider add [<type>]` and its flags for adding the canonical legacy instance whose alias is the provider type.
  It continues to fail with the existing duplicate-provider guidance when that alias is already configured.
  The new `--name <instance>` form is additive and permits another named instance only when provider policy eligibility and schema validation allow it.
  Keep existing `provider add` confirmation, `--yes`, `--dry-run`, safe backup, error, and exit-code behavior.
- **`provider show <instance>`** shows the safe alias, provider service type, environment-variable name and presence status, configured quota-domain scope names with label values hidden, budget eligibility, and model references that use the instance.
  It never prints a credential value, environment value, guessed provider identity, raw upstream error, or sensitive URL component.

All first-slice commands are offline and make no provider API calls.
Read-only commands do not ask for confirmation.
`provider add` changes the config only after preview confirmation.
Cancellation, validation failure, an unavailable environment variable, invalid config, or a failed atomic config write leaves the previous config intact.
Unknown aliases and unsupported service types fail with a safe message naming the alias/type and the corrective command, without exposing values.

### Explicit selection and future application route configuration

Clients select an instance by setting the existing `model` field to `instance/model`; the running gateway resolves that reference using its startup config.
An explicit model reference continues to select exactly one instance and never spills to a sibling instance.
Existing `model: auto` requests continue to use the current `router.auto` chain and its existing ordering.
Manual selection never overrides quota-domain cooldowns, circuit breakers, credentials, endpoint security, per-instance budgets, process-wide budgets, or provider policy restrictions.
If a configured quota domain or budget blocks a direct instance request, the gateway returns the existing compatible error behavior and does not try another same-service instance.
No quota, rate-limit, payment, authorization, or other provider failure may trigger automatic same-provider instance rotation.

Changing the provider instance used by an application without changing its code requires a separate, explicit route-configuration interface.
This is a deferred enhancement and does not define a committed schema or change current routing defaults.
One future option is an opt-in mapping from the configured local API-key name to a route profile for requests that send `model: auto`:

```yaml
# Concept only; not part of the first-slice config schema.
routes:
  applications:
    research-worker:
      auto: research-default
  profiles:
    research-default:
      models:
        - gemini-research/gemini-2.5-flash
        - groq-staging/llama-3.3-70b-versatile
```

The local API-key name would be a route lookup key, not an authorization grant or provider identity.
Applications without an explicit mapping would retain the current `auto` behavior.
An explicit application route would still be validated against provider-service uniqueness and all applicable quota domains, budgets, endpoint checks, and policy restrictions.
Its fallback list could cross provider services only; it could not rotate among instances of one provider service.
Changing an application mapping to another credential does not reset a shared quota domain, and a new credential or domain label is not evidence of independent capacity.
The route is eligible only when the provider's documented limits and terms permit that use.
Precedence relative to explicit model references and named tiers, config reload or restart behavior, and operator diagnostics require a separate ADR and approval before implementation.

### Example client walkthrough

Assume the user already has `gemini` and adds another authorized instance under the safe alias `gemini-research`.
The user's shell already has `GEMINI_RESEARCH_API_KEY` set; the CLI never asks for the key value.

```text
$ spillway provider add gemini --name gemini-research --key-env GEMINI_RESEARCH_API_KEY -config ./config.yaml
Preview for config.yaml:
  add provider instance gemini-research (type: gemini)
  credential reference: ${GEMINI_RESEARCH_API_KEY}
  model reference example: gemini-research/gemini-2.5-flash
  no credential value will be read or written
Write these changes? A backup is saved first. (y/N): y
Instance added. Restart spillway serve -config C:\Users\me\spillway\config.yaml to load this config change.

$ spillway provider list -config ./config.yaml
INSTANCE         TYPE       CREDENTIAL ENV             ENV STATUS
gemini           gemini     GEMINI_API_KEY             set
gemini-research  gemini     GEMINI_RESEARCH_API_KEY    set
groq             groq       GROQ_API_KEY               set

$ spillway provider show gemini-research -config ./config.yaml
gemini-research (gemini instance)
  credential     ${GEMINI_RESEARCH_API_KEY}, set in this process
  quota domains  project, billing_account (labels hidden)
  budget         monthly USD 2.00; process-wide budget also applies
  model example  gemini-research/gemini-2.5-flash
```

The client explicitly selects the instance in the normal OpenAI-compatible request:

```js
const response = await client.chat.completions.create({
  model: "gemini-research/gemini-2.5-flash",
  messages: [{ role: "user", content: "Summarize this research note." }],
});
```

To use the existing automatic route, the client continues sending `model: "auto"`; adding an instance does not change that route.
An instance added or changed in config becomes available after the gateway restarts with that config.

### Compatibility with existing provider CLI commands

Plan 0010 specifies `provider list`, `provider show <name>`, and `provider add [<name>]` with `-config`, `--key-env`, `--attest-free-tier`, `--base-url`, tier selection, `--dry-run`, and `--yes` behavior.
This proposal preserves those commands and flags.
List and show expand from preset names to safe instance aliases without changing secret-redaction guarantees.
The canonical legacy alias remains the default when `provider add` is used without `--name`.
Named-instance creation adds only the optional `--name <instance>` form.
Do not add preference commands or persistent CLI state in the first slice.
Where scripts need stable parsing, a future explicit machine-readable format requires its own decision.

Plan 0010 currently excludes a second instance and refuses duplicate preset names.
Before implementation, revise that plan's no-multi-instance non-goal, provider-add duplicate behavior, help text, and provider list/show model from preset to instance identity.
This cross-plan compatibility update is now recorded in Plan 0010; it does not change Plan 0010's v1.1.0 deliverables or its reserved ADRs.

## ADR impact of the revised CLI proposal

The provider-instance ADR drafts use numbers 0028 through 0030 because Plan 0010 already reserves ADR 0025 for config mutation and ADR 0026 for local diagnostics.
Do not reuse or renumber those Plan 0010 ADRs.

1. **ADR 0028, provider instance identity and additive config:** defines safe CLI aliases, the additive `provider add <type> --name <instance>` form, and compatibility with the existing canonical-alias add command.
2. **ADR 0029, quota domains and routing boundaries:** record direct instance selection, same-provider automatic rotation prohibition, and the requirement that future explicit route configuration obey quota domains, budgets, security checks, and provider restrictions.
3. **ADR 0030, provider policy eligibility:** require `provider add` to use an eligible supported type, remain offline, and avoid suggesting that another key or project grants extra quota.

ADR 0031 and a persistent CLI state file are not justified for the first slice because no interactive CLI request workflow consumes a local preference, and a preference would not change application requests.
Defer both `provider use` and the more accurate `provider prefer` name until such a workflow exists; if reconsidered, prefer is the clearer verb.
If application-specific route configuration is approved later, draft a separate ADR for its schema, precedence, validation, update/reload behavior, and diagnostics.
Allocate that ADR number when the proposal is ready and recheck the sequence; do not reserve ADR 0031 for the deferred preference command.

Plan 0010 was amended before implementation to replace its blanket multi-instance prohibition with the Plan 0011 boundary, define the additive named-instance add form, and update list/show terminology.
Its reserved ADRs 0025 and 0026 and existing `provider add`, `list`, and `show` flags and safety behavior remain intact.

## Official provider policy and quota scopes

Official provider sources were reviewed on 2026-10-21.
These sources can change; implementation and release must recheck them.
The table separates documented limit scope from Spillway policy and does not infer contractual permission solely from a technical quota boundary.

| Provider service | Officially documented scopes | Instance and routing consequence |
| --- | --- | --- |
| Gemini API | RPM, TPM, and RPD limits apply per Google Cloud project, not per API key; model limits can differ. API keys are associated with projects and share project quota. Billing-account tiers and monthly spend caps aggregate across linked projects; project spend caps also exist and are described as experimental. | Share project/model cooldowns across keys in a project. Track billing-account spend or restrictions as a parent domain when known. A project boundary is not permission to evade billing-account, free-tier, or terms restrictions. |
| Groq | Rate limits apply at organization level. Projects can have custom per-model limits constrained by organization limits; ITPM/OTPM and spend limits are organization-level. Groq's policy prohibits exceeding published limits by registering multiple accounts or orchestrating use between organizations. | Share organization cooldown and spend state, with project/model sub-limits where known. Do not use another Groq project or organization as automatic fallback for an exhausted limit. |
| DeepSeek API | Model concurrency limits are calculated per account regardless of API key. Search and Fetch QPS limits are account-level, and Fetch also documents a per-domain QPS limit. The reviewed model docs do not establish independent capacity from separate API keys. | Share account/model concurrency state across keys. For Search/Fetch, account and Fetch domain scopes are separate applicable domains. Do not infer project-level independence or route to another account when a limit is reached. |
| OpenRouter | Account-wide controls coexist with workspace, member, and key guardrails. Key budgets are per-key spend controls and member budgets aggregate across a member's keys. OpenRouter terms prohibit creating multiple accounts to bypass use limits. | Treat key-specific budgets as an extra restriction, not independent upstream model capacity. Keep process-wide budget aggregate and do not switch accounts or keys to escape a limit. |
| Ollama | A configured base URL names a local HTTP service; no hosted provider-account quota scope is implied. Distinct URLs may still share the same server or hardware. | Track endpoint health per instance; do not infer independent compute capacity from URL identity. |
| Mock | Local deterministic test provider with no upstream credential or hosted quota. | Keep deterministic test behavior; never treat a mock instance as real provider capacity. |

Official references:

- Gemini: [rate limits](https://ai.google.dev/gemini-api/docs/rate-limits), [billing and spend caps](https://ai.google.dev/gemini-api/docs/billing), [API keys and project association](https://ai.google.dev/gemini-api/docs/api-key).
- Groq: [rate limits](https://console.groq.com/docs/rate-limits), [projects and organization ceilings](https://console.groq.com/docs/projects), [spend limits](https://console.groq.com/docs/spend-limits), [acceptable use policy](https://console.groq.com/docs/legal/ai-policy).
- DeepSeek: [model rate limit and isolation](https://api-docs.deepseek.com/quick_start/rate_limit), [Search and Fetch limits](https://api-docs.deepseek.com/guides/search).
- OpenRouter: [guardrails](https://openrouter.ai/docs/guides/features/guardrails/overview), [terms of service](https://openrouter.ai/terms).

## Required ADRs and earlier decisions

Three accepted ADRs record the hard-to-reverse choices:

1. [ADR 0028](../adr/0028-provider-instance-identity-and-config.md) defines instance identity, additive schema, legacy resolution, and compatibility.
2. [ADR 0029](../adr/0029-quota-domains-and-routing-boundaries.md) defines quota-domain state, same-service route rejection, direct selection, and budgets.
3. [ADR 0030](../adr/0030-provider-policy-eligibility.md) defines official-source review and fail-closed policy eligibility.

ADR 0006 remains authoritative for spill-triggering failures, candidate order, cooldown handling, and the stream commit point.
Its earlier use of “provider” as both candidate identity and service type is superseded by the instance/service distinction in accepted ADRs 0028 and 0029.
Its broad assumption that candidates in a virtual route are distinct providers is superseded by the explicit rule to reject a route containing multiple instances of one provider service type.
Its direct-reference no-spill decision remains in force and means a direct instance reference selects one instance only.
These accepted decisions guide the current implementation; runtime behavior ships only in a separately verified release.

## Security and privacy requirements

- Environment expansion, secret enumeration, config printing, `check`, startup logging, diagnostics, dashboard APIs, and errors must handle every instance credential without exposing credential values.
- Provider-service `type` must resolve through a built-in registry before construction.
- Instance names and shared-domain labels must be constrained opaque slugs; do not store real project, organization, billing-account, email, or user identity values.
- One instance must never inherit another instance's credential, endpoint, free-tier attestation, quota state declaration, or error scrubber state.
- Local client keys remain client identities and do not select or authorize a provider instance.
- Any future application route mapping must use local client identity only as an explicit routing lookup, never as a replacement for authentication or as a provider authorization grant.
- Keep telemetry disabled and do not add provider-account metadata to persisted usage records.
- Bound config size, instance count, provider construction, and tracked quota-domain state.
- Preserve HTTPS, loopback, redirect, timeout, outbound-request, and secret-redaction rules for every instance endpoint.

## Migration and compatibility

- Existing config entries, provider references, model IDs, and OpenAI-compatible `/v1/*` shapes remain unchanged.
- New instance entries use additive fields and strict validation; older binaries may reject the new fields, so downgrade guidance must restore a pre-feature config or remove all new entries and references.
- Do not infer `type` for arbitrary names and do not silently rewrite legacy references.
- `spillway check` must validate the new schema offline and produce secret-free errors.
- The first implementation slice preserves existing `spillway init`, `check`, `provider add`, `provider list`, and `provider show` behavior, flags, and safety guarantees while adding named-instance support.
  It does not change dashboard API response schemas or client authentication.
- The first slice does not add a local provider preference, persistent CLI state, or application-specific route mappings.
- Do not require a store migration if existing provider/model columns can contain safe instance names without changing their meaning.
  If a new quota-domain or instance field is persisted, use a forward migration and preserve existing rows as legacy preset instances.
- Add DeepSeek as a usable service type only after its adapter preset and its own implementation/release scope are approved.

## Implementation completion gates

1. ADRs 0028 through 0030 are accepted; config retains environment references, bounded aliases, quota-domain labels, and per-instance budgets.
2. Plan 0010's multi-instance exclusions and command wording are revised while its v1.1.0 scope and reserved ADRs remain intact.
3. Official policy review covers Gemini, Groq, DeepSeek, OpenRouter, Ollama, and Mock; recheck policies before release.
4. Existing provider and model fields safely retain instance aliases; no store migration is introduced.
5. Parser, validation, legacy compatibility, and secret-redaction tests pass before provider construction changes.
6. Construction uses configured service types, while model listing, catalog, pricing, free-tier eligibility, budgets, dashboard summaries, and usage retain instance identity.
7. Shared cooldowns use configured, verified quota-domain labels only when scope is known; unknown scope uses a bounded service-wide cooldown.
8. Regression coverage proves direct selection, cross-service spillover, same-service non-spill, shared domains, instance budgets, and the aggregate process ceiling.
9. Security review covers config, credentials, endpoints, routing, logs, dashboard output, catalog, and CLI output.
10. Focused tests and `make check` must pass; downgrade guidance and examples must be checked before release.
11. DeepSeek adapter/preset support remains a separately approved prerequisite and is not implemented here.

## Acceptance criteria

- Every current config fixture parses unchanged and resolves to the same provider types, names, and models.
- A config can declare separately authorized instances with distinct environment references and select each through a direct model reference.
- Invalid names, unknown service types, literal credentials where references are required, and unsafe duplicate domain declarations fail with secret-free errors.
- `auto` and `spillway/<tier>` reject duplicate provider service types across fully resolved routes, including rule-based route expansion.
- No quota, payment, authorization, model, transport, timeout, or upstream failure causes automatic selection of another instance of the same provider service.
- Verified shared quota domains share cooldown state; uncertain scope defaults to provider-service-wide state.
- Per-instance budgets may restrict use but cannot increase the process-wide aggregate budget ceiling.
- Existing `provider add`, `provider list`, and `provider show` behavior and flags remain compatible; named-instance add is additive and safe aliases contain no credential or account identity.
- Existing virtual routes and model references are regression-locked: legacy configs resolve to the same provider/model identities, `provider/model` references retain their exact meaning, and `auto` and named tiers retain their established candidate order and routing reasons when no new route config is present.
- Requests using `model: auto` remain governed by `router.auto`; no CLI preference or instance addition silently changes the route.
- Explicit application route configuration remains deferred and requires separate approval; if later added, it must preserve authentication and apply all quota-domain, budget, endpoint-security, and provider-policy checks.
- Cross-service spillover, capability filtering, zero-spend ordering, pricing, API compatibility, dashboard privacy, and usage behavior remain correct.
- Provider policy scopes are documented with dated official sources, and uncertain policy does not enable cross-instance selection.
- DeepSeek adapter support is not included and requires separate approval before it can be used as an instance type.
- Implementation is not complete until the required tests, `make check`, and security review pass.

## Verification record

- Focused configuration, CLI, catalog, quota, budget, store, provider, API, and end-to-end tests pass locally, including direct legacy and named model selection and offline provider CLI journeys.
- Legacy virtual-route order and reason strings remain covered by the existing router/API regression tests and the mock binary spillover test.
- `make check` passes on Windows 11 with Go's race detector and the dashboard browser suite; no live provider API request was made.
- The provider-instance security review confirmed that environment references remain separate, quota-domain values are redacted, custom URL details remain hidden, and unknown quota scope is reported generically and shared service-wide.
- Release and cross-platform CI verification remain release gates; this worktree has not been pushed, merged, or released.
- Manual offline verification steps are recorded in [v1.2 manual verification](../v1.2-manual-verification.md).

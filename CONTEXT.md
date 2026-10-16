# Spillway - Project Context

> A self-hosted, single-binary LLM gateway that gives developers one OpenAI-compatible endpoint, routes each prompt to the cheapest model that's good enough, and spills over to the next provider when a quota runs out.

The name comes from a dam's spillway: when one reservoir is full, the overflow is safely routed to the next channel instead of breaking things.

---

## 1. Problem

Developers building with AI juggle many providers (Gemini, Groq, Mistral, OpenRouter, Anthropic, OpenAI, local models). Each has its own SDK, rate limits, free tier, and pricing. When one hits a rate limit or quota, apps break. Most prompts don't need a frontier model, but developers default to expensive ones because routing by hand is tedious.

## 2. Solution

Spillway runs on the developer's own machine or server as one binary. Their app points its OpenAI SDK at `http://localhost:8080/v1`, and Spillway handles the rest:

- **Unified API**: one OpenAI-compatible endpoint for every provider.
- **Smart routing**: picks the cheapest model capable of handling each prompt.
- **Automatic spillover**: when a provider is rate-limited, out of quota, or down, the request spills over to its next candidate in the tier.
- **Quota awareness**: tracks each key's limits and routes *before* hitting a 429.
- **Caching, budgets, and a dashboard**: see usage, cost, and routing decisions.

## 3. Principles & Non-Goals

**Principles**
- **Self-hosted first.** Everything runs locally. No Spillway cloud, no telemetry by default.
- **Bring your own keys (BYOK).** Each user supplies their own provider keys, one per provider. Keys never leave their machine.
- **Single binary.** Download, add config, run. No external services required (Ollama is optional).
- **Drop-in.** Existing OpenAI-SDK code works by changing only `base_url`.
- **Legitimate use only.** Spillway maximizes each provider's own legitimate free tier and pricing.

**Non-goals**
- No rotating multiple accounts on the same provider to evade rate limits or free-tier caps.
- No collecting or storing other people's credentials or Google/Gmail accounts.
- Not a hosted SaaS (a hosted version may come later, but it is out of scope).

## 4. Tech Stack

| Concern | Choice | Why |
|---|---|---|
| Language | **Go** (1.26+) | Easy streaming/concurrency, trivial cross-compilation, single static binary. Track the oldest supported release; 1.26 brings `testing/synctest` and the `go.mod` `tool` directive |
| HTTP | `net/http` with `ServeMux` method patterns | Standard library is enough; no router dependency |
| Storage | **SQLite** via `modernc.org/sqlite` | Pure Go, no cgo, single file |
| Config | YAML (`config.yaml`, parsed with `go.yaml.in/yaml/v3`) + env vars for secrets | Human-editable; `gopkg.in/yaml.v3` is archived |
| Dashboard | Small web UI embedded with `go:embed` | Ships inside the binary |
| Local router model | **Ollama** HTTP API (e.g., Llama 3.2 1B/3B) | Optional; no model code linked into the binary |
| Embeddings (router) | Local embedding model via Ollama, or provider embeddings | Fast prompt classification |
| Release | GoReleaser; Linux/macOS/Windows, amd64/arm64; Docker image on distroless static | One-command installs |
| Dev tools | golangci-lint, govulncheck, pinned via `tool` directives in `tools/go.mod` | Reproducible, nothing installed globally |

Rust is **not** used in v1. Revisit only if an in-process, CPU-heavy component is ever needed.

## 5. Architecture

```
client app (OpenAI SDK)
   │  base_url = http://localhost:8080/v1
   ▼
┌──────────────────── spillway (single Go binary) ────────────────────┐
│ API layer       /v1/chat/completions, /v1/embeddings, /v1/models    │
│                 SSE streaming, OpenAI request/response schema       │
│ Router          difficulty rules → optional local classifier      │
│ Quota tracker   per-key RPM/TPM/daily limits, 429 + retry-after     │
│ Spillover       ordered candidates per tier, circuit breakers       │
│ Cache           opt-in in-memory exact response cache               │
│ Budgets         local daily and monthly spend caps                  │
│ Adapters        gemini, groq, mistral, openrouter, anthropic,       │
│                 openai, ollama (each translates to/from OpenAI fmt) │
│ Store           SQLite: usage metadata, attempts and decisions     │
│ Dashboard       embedded UI: usage, cost, routing explanations      │
└──────────────────────────────────────────────────────────────────────┘
```

### Request lifecycle
1. Receive an OpenAI-format request. Authenticate it with a local key, which also identifies the client app.
2. Check the cache; return immediately on a hit.
3. **Route**: determine the required capabilities and difficulty tier, then pick a candidate list.
4. **Filter** candidates by quota headroom, budget, and circuit-breaker state.
5. Call the first candidate through its adapter and stream the response back.
6. On a 429, 5xx, or timeout, mark the key, then **spill over** to the next candidate.
7. Log usage, cost, latency, and the routing decision with its reason. Update quota state.

## 6. Routing Design (v0.3-v0.5)

Legacy routing and Stage 1 rules are deterministic and use only the request body and local configuration; v0.3 does not call a classifier or judge.

1. **Direct model reference.** A reference such as `groq/llama-3.1-8b-instant` passes through unchanged, without capability detection or filtering.
2. **Tier virtual model.** `spillway/<tier>` checks detected requirements against declared capabilities when declarations exist, then tries eligible candidates in tier order with v0.2 spillover behavior.
   An undeclared candidate remains eligible because the client chose the tier.
3. **Auto virtual model.** `auto` resolves the ordered, de-duplicated candidates in `router.auto`, applies requirement and context-window filtering, then tries eligible candidates in order.
   For a request with requirements, undeclared candidates are skipped because Spillway is choosing the model.
   Zero-spend tiers must precede tiers with `allow_spend: true` (ADR 0008).

For virtual models, requirements come from tool definitions or tool history, image content, JSON response format, structured output response format, and a conservative request-size estimate.
The `models:` section is the sole capability source in v0.3; Spillway never guesses from model names or contacts provider catalogs (ADR 0007).
For requests with requirements, `auto` skips undeclared candidates; both virtual models skip declared candidates missing a required capability.
A declared context window filters either virtual model only when the conservative size estimate clearly exceeds it.
Every chat response includes an `x-spillway-route` reason, such as `direct`, `tier demo; candidate 1/3` or `auto; needs tools; tier demo; candidate 1/1` (ADR 0008).

An opt-in `router.mode: rules` selects a difficulty for `auto` using ordered, user-defined rules over bounded request features.
Rules are first-match-wins; unmatched requests use `moderate`, whose configured tier list is tried in full before the request fails.
Each difficulty maps to an ordered list of tiers from `router.auto`; tier order keeps zero-spend tiers before tiers that allow spending.
Rules mode preserves capability and context filtering, quota and budget checks, spillover, and candidate de-duplication.
Its bounded `x-spillway-route` reason names the matched rule or `default`, difficulty, tier, candidate position, and fallback tier without including prompt content (ADR 0014).
An optional `router.classifier.model` enables Stage 2 difficulty classification through the explicitly configured loopback Ollama provider.
The model reference must include a pinned tag, and requests send at most 4 KiB of extracted text, allow at most 32 generated tokens, and have a three-second timeout.
Classifier errors, invalid labels, or the closed `uncertain` reason code use the same deterministic rules and a fixed `classifier fallback` route marker.
Classifier prompts and outputs are never logged or persisted.
The v0.5 evaluation did not establish a promotion case: its offline mock run used an unavailable classifier, zero-cost echo answers and no blinded ratings, so the classifier remains optional and disabled unless configured.
A model-backed comparison with known costs and independent blinded ratings is still needed before recommending default enablement.

In v0.4, an opt-in SQLite store records usage metadata only; prompts, responses, headers, keys and provider error text are never written to disk (ADR 0009).
The optional exact cache is memory-only, byte-exact and isolated by client app; it caches non-streaming successful responses and disappears on restart (ADR 0011).
Cost is an estimate based on provider-reported usage or conservative token estimates and user-declared prices; failed paid attempts without usage are charged their estimated prompt cost (ADR 0010).
Daily and monthly budgets are soft caps on estimated spend observed by this Spillway process, stop paid models only and may overshoot for requests already in flight (ADR 0010).
The embedded dashboard shell is static, while its data APIs require a local key marked `dashboard: true` (ADR 0012).
`spillway catalog` prints advisory suggestions and does not affect runtime routing or pricing (ADR 0013).

## 7. Configuration (example)

```yaml
server:
  listen: "127.0.0.1:8080"
  api_keys:                        # local keys; required, even on loopback (ADR 0003)
    - { name: my-app, key: "${SPILLWAY_KEY}" }
    - { name: dashboard, key: "${SPILLWAY_ADMIN_KEY}", dashboard: true }

providers:
  gemini:     { api_key: "${GEMINI_API_KEY}", free_tier: true }
  groq:       { api_key: "${GROQ_API_KEY}", free_tier: true }
  mistral:    { api_key: "${MISTRAL_API_KEY}" }
  openrouter: { api_key: "${OPENROUTER_API_KEY}" }
  ollama:     { base_url: "http://localhost:11434" }

tiers:
  free:
    models: [groq/llama-3.3-70b-versatile, gemini/gemini-2.5-flash, ollama/llama3.2:3b]
  smart:
    allow_spend: true
    models: [openrouter/anthropic/claude-sonnet-4.5]

models:
  groq/llama-3.3-70b-versatile: { capabilities: [tools, json_mode], context_window: 131072 }
  gemini/gemini-2.5-flash: { capabilities: [tools, vision, json_mode, structured_outputs], context_window: 1048576 }
  ollama/llama3.2:3b: { capabilities: [] }
  openrouter/anthropic/claude-sonnet-4.5: { capabilities: [tools, vision, json_mode, structured_outputs] }

router:
  auto: [free, smart]  # zero-spend tiers first; only listed tiers can spend

store:
  path: ./spillway.db
  retention: 90d

pricing:
  openrouter/anthropic/claude-sonnet-4.5: { input_usd_per_mtok: 3.00, output_usd_per_mtok: 15.00 }

budgets:
  monthly_usd: 5.00

cache:
  exact: true
  ttl: 24h
  max_mb: 64
```

Capability declarations are a user-maintained statement of what each model supports; an empty list means text only.
List every supported capability explicitly and update declarations when a model's support changes.
Only `tools`, `vision`, `json_mode` and `structured_outputs` are accepted.
`context_window` is optional and filters a model only when the conservative request-size estimate exceeds it.
Direct model references bypass these checks.
`store:` is opt-in and requires an existing directory for `store.path`; retention defaults to 90 days.
Budgets require `store:` so observed spend survives restarts, and the store retention must cover the configured period.
`pricing:` gives USD per million input and output tokens; every paid model in an `allow_spend` tier must be priced when a budget is configured.
Unpriced direct paid models fail closed while a budget is enabled.
`on_exceed` is removed: reached budgets stop paid models and continue serving free models.
The exact cache is opt-in, in memory, per client app, and bounded by `cache.max_mb`; temperature is ignored for identical request bodies.
Semantic caching is not supported.
The mock provider is always treated as free, even when a price is declared for its model.
Catalog output is partial: Gemini has no supported catalog fields, and Groq publishes only context length.
Provider catalog discovery is advisory and does not change runtime routing or pricing.
Rules mode is opt-in; existing configurations without `router.mode` continue to use the legacy auto chain.
When enabled, configure every difficulty and at least one ordered fallback tier for each label:

```yaml
router:
  mode: rules
  auto: [free, smart]
  difficulty:
    simple: [free]
    moderate: [free, smart]
    hard: [smart]
  rules:
    - when: {estimated_input_tokens_gte: 12000}
      difficulty: hard
    - when: {tools_present: true}
      difficulty: moderate
```

Conditions within a rule are combined with AND; rules are evaluated in order and the first match wins.
Supported bounded conditions are `estimated_input_tokens_gte`, `requested_output_tokens_gte`, and `tools_present`.

## 8. API Surface

- `POST /v1/chat/completions`: OpenAI-compatible, with streaming and tools.
- `POST /v1/embeddings`
- `GET  /v1/models`: configured provider models, `spillway/<tier>` virtual models and `auto` when `router.auto` is configured.
- `GET  /healthz`
- `GET  /dashboard`: the embedded static dashboard shell.
- `GET  /api/usage`, `/api/decisions`, `/api/quotas`: JSON endpoints that back the dashboard.
- `GET  /api/budget`: current local daily and monthly budget status.
- Dashboard data APIs require a key marked `dashboard: true`.
- `spillway catalog`: explicit advisory provider catalog suggestions; it writes no config and changes no routing.

Response headers: `x-spillway-provider`, `x-spillway-model`, `x-spillway-route`, `x-spillway-cache`.

## 9. Security

- Bind to `127.0.0.1` by default. Every listen address, loopback included, requires at least one local key, and on loopback requests with a foreign `Host` are rejected (ADR 0003).
- Provider keys come from env vars, referenced from `config.yaml` as `${VAR}`. They are never logged and are redacted in errors.
- Encrypted at-rest key storage (encrypted file or OS keychain) is deferred until after v1.0 and needs an ADR first, because its key-management design is not settled.
- Prompt/response logging is **off by default**. Metadata (tokens, cost, latency, route) is logged.
- The opt-in SQLite store contains sensitive usage metadata, is created with owner-only permissions and supports one Spillway process per file.
- Budgets cover only estimated spend recorded by this process; external provider usage and concurrent in-flight overshoot are not included.
- Cached responses live only in process memory; a cache hit costs zero and may serve the same answer despite a changed temperature.
- No outbound calls other than to the providers the user has configured.

## 10. Suggested Repo Layout

```
spillway/
├── apps/
│   └── spillway/
│       ├── main.go             # binary entry point, calls cli.Run
│       └── web/                # dashboard (embedded via go:embed)
├── internal/
│   ├── api/                    # OpenAI-compatible handlers, SSE
│   ├── router/                 # difficulty rules, local classifier; future judge/cascade
│   ├── quota/                  # limit tracking, circuit breakers
│   ├── providers/              # Provider interface, shared types, registry
│   │   ├── openaicompat/       # one client for OpenAI-compatible APIs (Groq, OpenRouter, Ollama presets)
│   │   ├── gemini/
│   │   └── mock/               # deterministic offline provider for tests and demos
│   ├── cache/
│   ├── budget/
│   ├── store/                  # SQLite access + migrations
│   ├── config/
│   └── cli/                    # subcommands serve, init, check, version; wiring
├── tests/                      # all tests, mirroring internal/ (ADR 0004), plus e2e/
├── examples/                   # config samples, client snippets
└── .goreleaser.yaml
```

Core interface:

```go
type Provider interface {
    Name() string
    Models(ctx context.Context) ([]Model, error)
    Chat(ctx context.Context, req ChatRequest) (ChatResponse, error)
    ChatStream(ctx context.Context, req ChatRequest) (Stream, error)
}

// Stream is pull-based: the caller reads until io.EOF and always calls Close.
// Unlike a channel, it can report an upstream error mid-stream, and it leaves
// no producer goroutine behind when the client disconnects.
type Stream interface {
    Recv() (ChatChunk, error)
    Close() error
}
```

A provider that speaks the OpenAI API is a preset in `openaicompat`, not a new package.

## 11. Milestones

1. **v0.1 Proxy.** OpenAI-compatible API with streaming. Adapters for OpenAI-compatible providers (Groq, OpenRouter, Ollama) plus Gemini. A config loader.
2. **v0.2 Spillover.** User-named tiers exposed as `spillway/<tier>` virtual models, spillover through candidates in tier order, quota tracking, and circuit breakers.
3. **v0.3 Rules router.** Request requirement detection, user-declared capability filtering for virtual models, free-first `auto` chains, and `x-spillway-route` reasons.
4. **v0.4 Observability.** Opt-in SQLite usage records, estimated costs, local soft budgets, in-memory exact cache, dashboard APIs and page, and advisory `spillway catalog` suggestions.
5. **v0.5 Smart routing.** Opt-in deterministic difficulty rules and an optional local Ollama classifier; evaluation did not justify default enablement.
6. **v1.0.** GoReleaser binaries, a Docker image, docs, and `spillway init` for interactive setup.

## 12. Conventions

- Go standard formatting and linting (`gofmt`, `golangci-lint`). Full rules in `docs/CODING_STANDARDS.md`.
- `context.Context` is passed through every provider call. Every external call has a timeout.
- Table-driven tests. Provider adapters are tested against recorded HTTP fixtures, not live APIs.
- Structured logging with `log/slog`.
- Keep dependencies minimal. Prefer the standard library.

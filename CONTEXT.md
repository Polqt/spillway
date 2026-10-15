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
│ Router          rules → embedding classifier → LLM judge → cascade │
│ Quota tracker   per-key RPM/TPM/daily limits, 429 + retry-after     │
│ Spillover       ordered candidates per tier, circuit breakers       │
│ Cache           exact-match (hash) + optional semantic cache        │
│ Budgets         per-day/month spend caps, per-client-app limits     │
│ Adapters        gemini, groq, mistral, openrouter, anthropic,       │
│                 openai, ollama (each translates to/from OpenAI fmt) │
│ Store           SQLite: usage logs, cache, quota state, decisions   │
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

## 6. Routing Design (v0.3 rules)

Routing is deterministic and uses only the request body and local configuration; v0.3 does not call a classifier or judge.

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

The embedding classifier, LLM judge and cascade are planned for v0.5.

## 7. Configuration (example)

```yaml
server:
  listen: "127.0.0.1:8080"
  api_keys:                        # local keys; required, even on loopback (ADR 0003)
    - { name: my-app, key: "${SPILLWAY_KEY}" }

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

budgets:
  monthly_usd: 5.00
  on_exceed: cheap_only    # cheap_only | block

cache:
  exact: true
  semantic: false
  ttl: 24h
```

Capability declarations are a user-maintained statement of what each model supports; an empty list means text only.
List every supported capability explicitly and update declarations when a model's support changes.
Only `tools`, `vision`, `json_mode` and `structured_outputs` are accepted.
`context_window` is optional and filters a model only when the conservative request-size estimate exceeds it.
Direct model references bypass these checks.
Provider catalog discovery and pricing are planned for v0.4; classifiers and `router.mode` are planned for v0.5.

## 8. API Surface

- `POST /v1/chat/completions`: OpenAI-compatible, with streaming and tools.
- `POST /v1/embeddings`
- `GET  /v1/models`: configured provider models, `spillway/<tier>` virtual models and `auto` when `router.auto` is configured.
- `GET  /healthz`
- `GET  /dashboard`: the embedded UI.
- `GET  /api/usage`, `/api/decisions`, `/api/quotas`: JSON endpoints that back the dashboard.

Response headers: `x-spillway-provider`, `x-spillway-model`, `x-spillway-route`, `x-spillway-cache`.

## 9. Security

- Bind to `127.0.0.1` by default. Every listen address, loopback included, requires at least one local key, and on loopback requests with a foreign `Host` are rejected (ADR 0003).
- Provider keys come from env vars, referenced from `config.yaml` as `${VAR}`. They are never logged and are redacted in errors.
- Encrypted at-rest key storage (encrypted file or OS keychain) is deferred until after v1.0 and needs an ADR first, because its key-management design is not settled.
- Prompt/response logging is **off by default**. Metadata (tokens, cost, latency, route) is logged.
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
│   ├── router/                 # rules, classifier, judge, cascade
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
4. **v0.4 Observability.** SQLite usage logs, budgets, exact cache, and the embedded dashboard.
5. **v0.5 Smart routing.** The embedding classifier, the Llama judge via Ollama, and the cascade mode.
6. **v1.0.** GoReleaser binaries, a Docker image, docs, and `spillway init` for interactive setup.

## 12. Conventions

- Go standard formatting and linting (`gofmt`, `golangci-lint`). Full rules in `docs/CODING_STANDARDS.md`.
- `context.Context` is passed through every provider call. Every external call has a timeout.
- Table-driven tests. Provider adapters are tested against recorded HTTP fixtures, not live APIs.
- Structured logging with `log/slog`.
- Keep dependencies minimal. Prefer the standard library.

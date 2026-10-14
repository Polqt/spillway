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

## 6. Routing Design (layered)

The router is layered so that most requests never need an LLM call to be routed.

1. **Rules (~0 ms).** A model reference (`model: "groq/llama-3.1-8b-instant"`) passes through as-is, plus the required capabilities: vision, tools, JSON mode, and context length versus token count. A virtual model (`"auto"`, or a tier such as `"spillway/cheap"`) enables routing.
2. **Embedding classifier (~5–20 ms).** Embed the prompt, then run kNN against labeled examples (`simple`, `coding`, `reasoning`, `creative`, `long-context`) to get a tier and a confidence score.
3. **LLM judge (low confidence only).** If confidence is below the threshold, ask a small local Llama via Ollama for a difficulty score from 1 to 5. Cache decisions by prompt hash.
4. **Cascade (optional, per config).** Try the cheap tier first. Validate the response (JSON validity, refusal detection, length sanity). Escalate one tier if it fails.

Every decision records a human-readable `reason` (e.g., `"rule: needs vision"`, `"classifier: simple (0.91)"`) that the dashboard shows and that is returned in the `x-spillway-route` response header.

Prior art worth reading: LMSYS **RouteLLM**, **LiteLLM** (provider adapters), **OpenRouter** (model catalog/pricing ideas).

## 7. Configuration (example)

```yaml
server:
  listen: "127.0.0.1:8080"
  api_keys:                        # local keys; required, even on loopback (ADR 0003)
    - { name: my-app, key: "${SPILLWAY_KEY}" }

providers:
  gemini:     { api_key: "${GEMINI_API_KEY}" }
  groq:       { api_key: "${GROQ_API_KEY}" }
  mistral:    { api_key: "${MISTRAL_API_KEY}" }
  openrouter: { api_key: "${OPENROUTER_API_KEY}" }
  ollama:     { base_url: "http://localhost:11434" }

tiers:
  cheap:  [groq/llama-3.1-8b-instant, gemini/gemini-flash, ollama/llama3.2:3b]
  smart:  [gemini/gemini-pro, mistral/mistral-large, openrouter/auto]

router:
  mode: layered            # rules | layered | cascade
  judge_model: ollama/llama3.2:1b
  confidence_threshold: 0.75

budgets:
  monthly_usd: 5.00
  on_exceed: cheap_only    # cheap_only | block

cache:
  exact: true
  semantic: false
  ttl: 24h
```

The model names above are placeholders. The adapters should fetch or refresh each provider's model list and pricing instead of hardcoding them.

## 8. API Surface

- `POST /v1/chat/completions`: OpenAI-compatible, with streaming and tools.
- `POST /v1/embeddings`
- `GET  /v1/models`: real models plus the virtual ones (`auto`, `spillway/cheap`, `spillway/smart`).
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
├── cmd/spillway/main.go        # CLI entry: serve, init, check, version
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
│   └── config/
├── web/                        # dashboard (embedded via go:embed)
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
2. **v0.2 Spillover.** User-named tiers, spillover through candidates in tier order, quota tracking, and circuit breakers.
3. **v0.3 Rules router.** Capability detection, `auto` and tier virtual models, and route reasons in headers.
4. **v0.4 Observability.** SQLite usage logs, budgets, exact cache, and the embedded dashboard.
5. **v0.5 Smart routing.** The embedding classifier, the Llama judge via Ollama, and the cascade mode.
6. **v1.0.** GoReleaser binaries, a Docker image, docs, and `spillway init` for interactive setup.

## 12. Conventions

- Go standard formatting and linting (`gofmt`, `golangci-lint`). Full rules in `docs/CODING_STANDARDS.md`.
- `context.Context` is passed through every provider call. Every external call has a timeout.
- Table-driven tests. Provider adapters are tested against recorded HTTP fixtures, not live APIs.
- Structured logging with `log/slog`.
- Keep dependencies minimal. Prefer the standard library.
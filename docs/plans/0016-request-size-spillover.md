# Request-size rejections spill over to the next candidate

Status: implemented on local `main`; ADR 0034 stays proposed until the owner confirms open questions 1 to 3, and manual checks 1 to 5 need the owner's keys
Milestone: v1.1.0 (behaviour change for virtual models; see open question 1 on ADR 0015)

## Goal

When a provider rejects a virtual-model request because it is too large for that provider (HTTP 413, or HTTP 400 with structured code `context_length_exceeded`), Spillway tries the next candidate from a different provider service instead of failing.
The rejection is treated as a property of this request, not of the provider's health: no cooldown, no circuit-breaker strike and no quota state change, so the provider stays available for smaller requests.
When every eligible candidate rejects the request as too large, the client gets OpenAI's own answer for that case: 400 `invalid_request_error` with code `context_length_exceeded`.

## Scope

In:
- A new provider error kind, `providers.ErrRequestTooLarge`, distinct from `ErrBadRequest`.
- Classification in the shared OpenAI-compatible client: any upstream 413, any upstream 400 whose JSON `error.code` is the string `context_length_exceeded`, and an OpenAI-shaped SSE error event whose `error.code` is `413` or `context_length_exceeded`.
- Spillover on `ErrRequestTooLarge` for `auto` and `spillway/<tier>`, under every existing ADR 0006 rule (commit point, one attempt per candidate, tier order, distinct provider services).
- No tracker state change on `ErrRequestTooLarge`, proven by tests.
- Client-facing outcomes: direct reference, all-too-large exhaustion, and mixed exhaustion.
- Per-attempt reason `request_too_large` in the attempt log line and usage record, and `request too large` in the exhaustion message, with no provider text, prompt text or keys.
- ADR 0034 (draft in Appendix A), a relationship section in ADR 0006, a glossary term, and updates to `CONTEXT.md`, `README.md`, `docs/troubleshooting.md`, `docs/limitations.md` and `docs/providers/groq.md`.
- A binary-level test that replays the 2026-10-29 OpenCode run with fake upstreams, and a live re-run that only the owner can do.

Out:
- Gemini over-long input.
  Google documents 400 `INVALID_ARGUMENT` for malformed bodies and 500 `INTERNAL` with the example "Your input context is too long", and neither carries a distinct structured code on the OpenAI-compatible endpoint.
  Telling a too-long 400 apart from a malformed 400 needs message text, which this plan forbids.
  Gemini's 500 variant already spills as `ErrUpstreamUnavailable` and counts a breaker strike; that stays as is.
  Deferred with no milestone until Google exposes a reason code (open question 6).
- OpenRouter context overflow.
  OpenRouter's documented `ErrorResponse.error.code` is a number equal to the HTTP status, not the string `context_length_exceeded`, so its over-long 400 is indistinguishable from other 400s and does not spill.
  An OpenRouter 413 does spill under the status rule.
- Ollama.
  Ollama is understood to truncate a prompt longer than its `num_ctx` instead of rejecting it, so there is nothing to classify (open question 5).
- Gemini's 429 `RESOURCE_EXHAUSTED` for a single request larger than its per-minute token limit, which cools Gemini down service-wide today.
  That is a pre-existing conservative behaviour under ADR 0029 and is not changed here; deferred with no milestone.
- Learning a per-candidate size ceiling from past rejections, or a new config field for a per-request token cap; rejected in ADR 0034, and `models.<ref>.context_window` already filters candidates up front.
- Changing Spillway's own 10 MiB body limit answer (413 `request_too_large`), which happens before routing and is unrelated.
- Any change to the Responses or Anthropic Messages translators beyond passing the new status and code through their existing error mapping.

Precondition: the unrelated WIP in `internal/evaluation/report.go` and the untracked `scripts/test-codex-responses-live.ps1` are not part of this plan.

## Design

### What the code does today

1. `openaicompat.Client.responseErrorBody` (`internal/providers/openaicompat/client.go`) maps 400 and its `default` branch to `providers.ErrBadRequest`, so 413 and 422 become `ErrBadRequest`.
   The Gemini adapter uses the same client and only overrides `RESOURCE_EXHAUSTED` bodies through `ClassifyError`.
2. `sseStream.Recv` maps an SSE `error` event through `streamErrorKind`, which knows only numeric 429 and 402; anything else, including the string `context_length_exceeded`, becomes `ErrUpstreamUnavailable`, which spills but counts a breaker strike.
3. `chatHandler.servePlan` (`internal/api/chat.go`) stops at the first error for which `isSpillable` is false and answers with `providerError`, which turns `ErrBadRequest` into 400 `provider_bad_request`.
4. `quota.Tracker.Observe` (`internal/quota/tracker.go`) acts only on the kinds in its switch; any other error, including `ErrBadRequest`, leaves state unchanged.
5. On exhaustion, `servePlan` answers 429 with `Retry-After` when every eligible failure has a known resume time, otherwise 503 `no_candidate_available`, with a message built from fixed reason strings (`candidateReason`), never provider text.
6. A plan whose candidates were all skipped for declared size already answers 400 `invalid_request_error` `context_length_exceeded` (ADR 0008, `writePlanError`).

In the live run, Gemini's 503 spilled correctly, Groq's 413 became `ErrBadRequest`, `isSpillable` returned false, and the client got 400 `provider_bad_request` without Ollama being tried.

### External facts this plan relies on (checked 2026-10-29)

- Groq, `https://console.groq.com/docs/errors`: 413 means "The request body is too large"; the documented example body is `{"error":{"message":"Request body too large","type":"invalid_request_error","code":413}}`.
  The free-tier TPM rejection in the live run is also a 413; its body is reported in public issue trackers as `"type":"tokens","code":"rate_limit_exceeded"` with an organization ID in the message.
  So the status, not the body code, must decide; matching `rate_limit_exceeded` first would cool Groq down for every request.
- Groq's context overflow is reported publicly as 400 with `"code":"context_length_exceeded"`; it is not in Groq's published error page, so the fixture is reconstructed (open question 4).
- OpenRouter, `https://openrouter.ai/docs/api_reference/errors-and-debugging`: `error.code` is a number that matches the HTTP status for request errors.
- Google, `https://ai.google.dev/gemini-api/docs/generate-content/api-errors`: 400 `INVALID_ARGUMENT` is the generic malformed-body status; 500 `INTERNAL` lists "Your input context is too long" as an example cause.
- OpenAI returns 400 `invalid_request_error` with code `context_length_exceeded` for input beyond the context window, and stock SDKs do not retry a 400.

### Decision 1: a distinct error kind

```go
// internal/providers/provider.go, added to the existing var block

// ErrRequestTooLarge means the provider rejected this request as larger than
// the model's context window or a per-request token limit. Another provider
// may accept the same request.
ErrRequestTooLarge = errors.New("provider request too large")
```

It does not wrap `ErrBadRequest`.
Wrapping would make every `errors.Is(err, ErrBadRequest)` caller see two meanings, and the ordering of `candidateErrorKind` would become load-bearing.
Rejected: reusing `ErrRateLimited` (would cool the service down for requests that fit, and waiting never helps this request) and `ErrUpstreamUnavailable` (three large requests would open the breaker for everyone).

### Decision 2: classification by status and structured code only

In `internal/providers/openaicompat/client.go`:

```go
// contextLengthExceeded is OpenAI's structured error code for input beyond a model's context window.
const contextLengthExceeded = "context_length_exceeded"

// errorCode returns error.code from an OpenAI-shaped error object body, or nil.
func errorCode(body []byte) json.RawMessage

// isContextLengthCode reports whether code is the JSON string "context_length_exceeded".
func isContextLengthCode(code json.RawMessage) bool
```

`responseErrorBody` keeps the adapter `ClassifyError` hook first, then adds two cases before the existing 400 case:

- `resp.StatusCode == http.StatusRequestEntityTooLarge` returns `ErrRequestTooLarge` for any body, and ignores `Retry-After` and `x-ratelimit-*` headers so no cooldown can follow.
- `resp.StatusCode == http.StatusBadRequest && isContextLengthCode(errorCode(body))` returns `ErrRequestTooLarge`.

`streamErrorKind` returns `ErrRequestTooLarge` for numeric code `413` or the string `context_length_exceeded`.
Message text is never inspected (`docs/CODING_STANDARDS.md`: no string matching), so a 400 that only says "maximum context length", OpenRouter's numeric-code 400, Gemini's `INVALID_ARGUMENT`, and every 422 stay `ErrBadRequest` and do not spill.
The error string keeps today's format, `<name>: upstream status 413: <scrubbed, truncated message>`, and the scrubber still runs before truncation.

### Decision 3: spill, but leave the provider's state alone

In `internal/api/chat.go`:

- `isSpillable` adds `providers.ErrRequestTooLarge`.
- `candidateErrorKind` returns `"request_too_large"` for it, checked before `ErrBadRequest`.
- `candidateReason` returns `"request too large"`.
- `servePlan` still calls `tracker.Observe(ref, err, providers.RateLimit{})`; the tracker's switch has no case for the new kind, so nothing changes.
  No tracker code changes; only the `Observe` doc comment gains "request-size rejections" next to cancellation and request errors, and T2 locks the property with tests.

Data flow for the live case, `spillway/free = [gemini/gemini-3.5-flash, groq/openai/gpt-oss-120b, ollama/llama3.2:3b]`, streaming:

1. Gemini `ChatStream` returns 503, which is `ErrUpstreamUnavailable`: one breaker strike, spill.
2. Groq `ChatStream` returns 413, which is `ErrRequestTooLarge`: attempt outcome `request_too_large`, log line `tier candidate failed ... error_kind=request_too_large`, no tracker change, spill.
3. Ollama streams; `x-spillway-route: tier free; candidate 3/3`.

The commit point is unchanged: a 413 or a first-event error arrives before the first byte is written, so it spills; a size error after the first chunk ends the stream with an error event and no `[DONE]`.
Direct references are untouched by `isSpillable` and never spill.
One attempt per candidate and the 16-candidate bound are unchanged because the loop is unchanged.
Virtual routes already reject two instances of one provider service type (ADR 0029), so a size rejection can never move a request to a sibling credential of the same service.

### Decision 4: client-facing outcomes

`providerError` gains:

```go
case errors.Is(err, providers.ErrRequestTooLarge):
	return http.StatusBadRequest, errorDetail{
		Message: fmt.Sprintf("Provider %q rejected the request as too large for its context window or token limit; shorten the request or use a model with a larger limit.", ref.Provider),
		Type:    typeInvalidRequest,
		Code:    "context_length_exceeded",
	}
```

This applies to a direct reference, to a post-commit stream error event, and to no virtual-model path except through exhaustion below (open question 2).

Exhaustion in `servePlan` changes as follows; `candidateFailure` gains `requestTooLarge bool`, set from `errors.Is(err, providers.ErrRequestTooLarge)` even when the tracker reports a concurrent cooldown for that candidate.

```go
// exhaustionOutcome decides the status for a request no eligible candidate served.
// eligible holds exactly one failure per eligible candidate.
func exhaustionOutcome(eligible []candidateFailure, now time.Time) (status int, code string, errorKind string, retryAfter int)
```

1. If every eligible failure is a request-size rejection: 400 `invalid_request_error` `context_length_exceeded`, no `Retry-After`, usage `error_kind` `context_length_exceeded`.
2. Otherwise size rejections are left out, and the existing rule runs on the rest: 429 `rate_limit_exceeded` with `Retry-After` set to the earliest resume time when every remaining failure has one (usage `error_kind` `budget_exceeded` when all are budget blocks, else `rate_limit_exceeded`), otherwise 503 `no_candidate_available`.

Rule 2 is the one non-obvious choice (open question 3).
A size-rejected candidate cannot serve this request at any time, so its missing resume time should not turn a "retry in 7s" answer into a 503; leaving it in would make `Retry-After` disappear exactly when the only useful provider is cooling down.
The rejected alternative is the unchanged rule, which answers 503 whenever any candidate rejected for size.

The exhaustion message stays `exhaustionMessage(tier, failures)`, so entries read `groq/openai/gpt-oss-120b request too large`, mixed with plan-skip and cooldown reasons, bounded to 1800 bytes and free of provider text.
The route header grammar is unchanged: `...; exhausted (<m> eligible)`.

Status and code choice for rule 1, with rejected alternatives:
- 400 `context_length_exceeded` (chosen): OpenAI's own answer, already used by Spillway when every plan skip was for size (ADR 0008), and SDKs do not retry it.
- 413 `request_too_large`: Spillway uses it for its own 10 MiB body limit, so a user would look for the wrong fix, and OpenAI never returns 413 for token limits.
- 429 `rate_limit_exceeded` (OpenAI's TPM "Request too large" answer): SDKs would retry a request that can never succeed.

### Logging and records

- Each failed attempt keeps the existing line `msg="tier candidate failed" tier=... provider=... model=... error_kind=request_too_large` at warn level.
- The attempt record's `outcome` is `request_too_large`; the store column is free text, so no migration.
- The access log `error` attribute on exhaustion stays `no candidate available`.
- For a direct reference the access log `error` attribute keeps today's scrubbed upstream message, which for Groq includes an organization ID; that is existing behaviour for every upstream error and is noted in ADR 0034, not changed here.
- No prompt text, header value or key is logged on any path; T3 and T7 prove it with markers.

### Error model summary

| Situation | Status | `type` | `code` | Spills |
|---|---|---|---|---|
| Upstream 413, virtual model, a later candidate succeeds | 200 | - | - | yes |
| Upstream 400 `context_length_exceeded`, virtual model | as above | | | yes |
| Every eligible candidate rejected for size | 400 | `invalid_request_error` | `context_length_exceeded` | - |
| Size rejections mixed with cooling candidates only | 429 + `Retry-After` | `api_error` | `rate_limit_exceeded` | - |
| Size rejections mixed with other failures | 503 | `api_error` | `no_candidate_available` | - |
| Direct reference, upstream 413 or 400 `context_length_exceeded` | 400 | `invalid_request_error` | `context_length_exceeded` | never |
| Any other upstream 400 or 422 | 400 | `invalid_request_error` | `provider_bad_request` | no |
| Spillway body over 10 MiB | 413 | `invalid_request_error` | `request_too_large` | - |

`/v1/responses` passes status and body through; `/v1/messages` maps `invalid_request_error` to Anthropic's `invalid_request_error` through `clientErrorBody`; neither translator changes.

### Security properties and how tests prove them

- Provider key absent from the error string, response body and logs on 413 and 400 `context_length_exceeded` (T6 adapter tests, T3 and T7 handler and binary tests).
- Provider error text (marker `org_test_marker`) absent from every client response, including the exhaustion message (T3, T4, T7).
- Prompt text (marker `PROMPT-MARKER-7f3a`) absent from logs (T3, T7).
- Inbound `Authorization` never forwarded: each fake upstream asserts it received only its own fake provider key (T7).
- No same-service rotation: a direct reference makes exactly one upstream call and no other provider is called (T3, T7).

## Hard-rule check

- No quota evasion: spillover stays across distinct provider services only, enforced by existing virtual-route validation (ADR 0029); a size rejection never cools down, rotates or retries the same service, and direct references never spill.
- No credential collection: no new credential handling; only the user's configured provider keys are used.
- Secrets never leak: error strings keep the existing scrub-then-truncate path, client messages use fixed strings, and T3, T6 and T7 assert keys, provider text and prompt markers are absent.
- Single binary, no cgo: standard library only, no new module.
- Local by default: no listener, auth or `Host` handling changes, and no new outbound calls beyond the next configured candidate.
- OpenAI compatibility: the all-too-large and direct answers use OpenAI's own status, type and code for context overflow, in the standard error shape, and SSE error events keep their shape.

## Tasks

### T1: ADR 0034, ADR 0006 relationship and glossary

- Files: `docs/adr/0034-request-size-rejections-spill.md` (new, from Appendix A), `docs/adr/0006-spillover-policy.md`, `GLOSSARY.md`.
- Depends on: none; set ADR 0034 to `Status: accepted` only after the owner answers open questions 1 to 3, and keep it `proposed` until then.
- Tests first: none; Markdown only, and the docs tests in `tests/e2e/docs_test.go` must stay green.
- Acceptance:
  - ADR 0034 exists with the Appendix A text and the owner's answers folded in.
  - ADR 0006 keeps its Decision text and gains a section `## Relationship to request-size rejections (ADR 0034)` saying that upstream 413 and 400 `context_length_exceeded` now spill without changing quota state, that every other 400 and 422 still does not spill, and that the commit point, ordering and one-attempt rule are unchanged.
  - `GLOSSARY.md` gains **Request-size rejection** under Delivery: "A provider's refusal of one request because it is larger than the model's context window or a per-request token limit, signalled by HTTP 413 or a 400 with code `context_length_exceeded`. It spills to the next candidate and leaves cooldowns and circuit breakers unchanged (ADR 0034). _Avoid_: context overflow, payload too large".
  - `make check` passes.
- Status: done

### T2: `ErrRequestTooLarge` and tracker neutrality

- Files: `internal/providers/provider.go`, `internal/quota/tracker.go` (doc comment only), `tests/providers/providers_test.go`, `tests/quota/quota_test.go`.
- Depends on: T1.
- Tests first:
  - `TestErrors_RequestTooLargeIsDistinctFromBadRequest`: `errors.Is(ErrRequestTooLarge, ErrBadRequest)` and the reverse are false; `fmt.Errorf("groq: upstream status 413: x: %w", ErrRequestTooLarge)` still matches `ErrRequestTooLarge`.
  - `TestTracker_RequestTooLargeLeavesStateUnchanged`, table-driven inside `synctest.Test`, each row compares `Snapshot()` before and after five `Observe(ref, ErrRequestTooLarge, RateLimit{})` calls and asserts `Available(ref)` is true:
    - fresh candidate;
    - after two `ErrUpstreamUnavailable`: the next upstream failure opens the circuit for exactly 30s (worked value: third failure, `circuitBase`), proving the count was neither raised nor reset;
    - after one `ErrRateLimited` without `Retry-After` and waiting its 15s: the next rate limit cools for 30s, proving the backoff counter was not reset as a success would;
    - with a sibling instance of the same service type configured: the sibling stays available.
  - `TestTracker_RequestTooLargeIgnoresRateLimitHint`: `Observe(ref, ErrRequestTooLarge, RateLimit{Known: true, RemainingTokens: 0, ResetTokens: time.Minute})` leaves the candidate available.
- Acceptance: tests pass; `Observe`'s doc comment names request-size rejections as state-neutral; `make check` passes.
- Status: done

### T3: spill on request-size rejection, non-streaming, and the direct answer

- Files: `internal/api/chat.go`, `tests/api/api_test.go`, `tests/api/router_test.go`, `tests/api/usage_test.go`.
- Depends on: T2.
- Tests first:
  - `TestChat_TierSpillsOnlyForProviderFailures`: new row `request too large`, `spill: true`, second candidate called once.
  - `TestChat_TierRequestTooLargeSpillsWithoutCooldown`: tier `demo = [first/one, second/two]` with a real `quota.Tracker`; `first` returns `fmt.Errorf("upstream org_test_marker: %w", providers.ErrRequestTooLarge)`.
    Asserts 200, `X-Spillway-Provider: second`, `X-Spillway-Model: two`, `X-Spillway-Route: tier demo; candidate 2/2`.
    A second identical request calls `first` again (`first.calls == 2`), and `tracker.Snapshot()` shows `first` with no cooldown, breaker `closed` and `UpstreamFailures` 0.
  - `TestChat_RequestTooLargeSpillRouteReasons`, table over plan sources, each asserting the serving model and the exact route string:
    `spillway/demo` gives `tier demo; candidate 2/2`; `auto` with `router.auto: [free, paid]` and the size rejection on the only free candidate gives `auto; tier paid; candidate 2/2`; rules mode with the default rule gives `rules; default; difficulty moderate; tier free; candidate 2/2`.
  - `TestChat_DirectRequestTooLargeDoesNotSpill`: direct `first/one` returns the size error; asserts 400 `invalid_request_error` `context_length_exceeded`, `X-Spillway-Route: direct`, `second.calls == 0`, and the body contains neither `org_test_marker` nor `sk-test-local-cursor`.
  - `TestChat_RequestTooLargeAttemptIsLoggedWithoutContent`: request body contains `PROMPT-MARKER-7f3a`; `first`'s error contains `sk-test-local-cursor` and `org_test_marker`.
    Logs contain `msg="tier candidate failed"`, `error_kind=request_too_large` and `attempts=2`, and contain none of the prompt marker, the key or the provider marker.
  - `TestUsage_RequestTooLargeAttemptOutcome` in `usage_test.go`: the recorded request has two attempts with outcomes `request_too_large` and `success`, and an empty request `error_kind`.
- Acceptance: tests pass; `isSpillable`, `candidateErrorKind`, `candidateReason` and `providerError` handle the new kind; `TestChat_TierDoesNotSpillOnUpstreamClientStatuses` still passes unchanged because the adapter does not yet emit the new kind; `make check` passes.
- Status: done

### T4: exhaustion outcome with request-size rejections

- Files: `internal/api/chat.go`, `tests/api/api_test.go`, `tests/api/router_test.go`.
- Depends on: T3.
- Tests first:
  - `TestChat_TierAllRequestTooLargeIsContextLengthExceeded`: both candidates return the size error with `org_test_marker` and a fake key in the error.
    Asserts 400 `invalid_request_error` `context_length_exceeded`, no `Retry-After`, `X-Spillway-Route: tier demo; exhausted (2 eligible)`, no `X-Spillway-Provider`, message contains `first/one request too large` and `second/two request too large`, body under 2 KiB, no marker or key in body or logs, and usage `error_kind` `context_length_exceeded`.
  - `TestChat_TierRequestTooLargeMixedExhaustionStatus`, table inside `synctest.Test` where time matters:
    - size rejection plus `ErrUpstreamUnavailable` without retry time: 503 `no_candidate_available`;
    - size rejection plus `RetryAfterError{After: 7s, Kind: ErrRateLimited}`: 429 `rate_limit_exceeded` with `Retry-After: 7`;
    - candidate already cooling for 20s (skipped at runtime) plus a size rejection: 429 with `Retry-After: 20`;
    - size rejection plus a budget-blocked paid candidate: 429 with usage `error_kind` `budget_exceeded`.
  - `TestChat_AutoPlanSizeSkipPlusRuntimeRequestTooLarge`: `auto` with candidate A declared `context_window: 1` (plan skip `context too small`) and candidate B returning the size error; asserts 400 `context_length_exceeded`, route `auto; tier free; exhausted (1 eligible)`, and a message containing both `context too small` and `request too large`.
- Acceptance: tests pass; existing `TestChat_TierCooldownExhaustionUsesEarliestRetryAfter` and `TestChat_TierExhaustionIsBoundedAndDoesNotLeakProviderText` pass unchanged; `make check` passes.
- Status: done

### T5: streaming paths

- Files: `tests/api/stream_test.go`; `internal/api/chat.go` or `internal/api/stream.go` only if a test fails.
- Depends on: T4.
- Tests first:
  - `TestStream_TierRequestTooLargeOnOpenSpills`: `first.ChatStream` returns the size error; `second` streams two chunks.
    Asserts SSE content type, both chunks, `[DONE]`, `X-Spillway-Model: two`, route `tier demo; candidate 2/2`, and no tracker change for `first`.
  - `TestStream_TierRequestTooLargeFirstEventSpills`: `first`'s stream returns the size error on the first `Recv`; the same assertions as above, and `first`'s stream is closed.
  - `TestStream_TierRequestTooLargeAfterChunkDoesNotSpill` (partial chunks): `first` relays one chunk, then `Recv` returns the size error.
    Asserts the client received that chunk, then one `data:` error event with code `context_length_exceeded`, no `[DONE]`, and `second.calls == 0`.
  - `TestStream_TierRequestTooLargeThenClientDisconnectStopsSpill` (client disconnect): `first.ChatStream` cancels the request context and returns the size error.
    Asserts `second.calls == 0`, nothing written after headers, and usage `error_kind` `client_disconnected`.
  - `TestStream_TierUpstreamErrorMidStreamAfterSizeSpill` (upstream error mid-stream): `first` rejects for size on open, `second` relays one chunk and then fails with `ErrUpstreamUnavailable`.
    Asserts an error event with code `upstream_unavailable`, no `[DONE]`, and one breaker strike on `second` only.
  - `TestStream_TierAllRequestTooLargeIsPlainHTTPError`: both reject on open; asserts a JSON 400 `context_length_exceeded`, not an event stream.
- Acceptance: tests pass with `-race`; no goroutine outlives a test (existing leak checks in `stream_test.go` stay green); `make check` passes.
- Status: done

### T6: adapter classification

- Files: `internal/providers/openaicompat/client.go`, `tests/providers/openaicompat/client_test.go`, `tests/providers/gemini/gemini_test.go`, `tests/api/api_test.go`, `tests/e2e/groq_test.go`, new fixtures under `tests/providers/openaicompat/testdata/` and `tests/providers/gemini/testdata/`.
- Depends on: T5.
- Fixtures (fake values only; `org_test_marker` stands in for the organization ID):
  - `groq-413-tpm.json`: `{"error":{"message":"Request too large for model `openai/gpt-oss-120b` in organization `org_test_marker` service tier `on_demand` on tokens per minute (TPM): Limit 8000, Requested 35112, please reduce your message size and try again.","type":"tokens","code":"rate_limit_exceeded"}}`.
  - `groq-413-documented.json`: Groq's documented body with numeric `"code":413`.
  - `context-length-400.json`: `{"error":{"message":"Please reduce the length of the messages or completion.","type":"invalid_request_error","param":"messages","code":"context_length_exceeded"}}`.
  - `context-length-text-only-400.json`: a 400 whose message says "maximum context length" with `"code":"invalid_request"`.
  - `openrouter-400-context.json`: `{"error":{"code":400,"message":"This endpoint's maximum context length is 131072 tokens."}}`.
  - `stream-error-first-context-length.sse` and `stream-error-first-413.sse`: a first `data:` error event with code `"context_length_exceeded"` and `413`.
  - `gemini/testdata/invalid-argument-token-count.json`: Gemini's array-form 400 `INVALID_ARGUMENT` with a token-count message (reconstructed).
- Tests first:
  - `TestClient_MapsRequestSizeRejections`, table over `Chat` and `ChatStream`:
    `groq-413-tpm.json` served with `Retry-After: 60` and `x-ratelimit-remaining-tokens: 0` gives `ErrRequestTooLarge`, not `ErrRateLimited`, and `errors.As(err, &RetryAfterError{})` is false;
    `groq-413-documented.json` and an empty 413 give `ErrRequestTooLarge`;
    `context-length-400.json` gives `ErrRequestTooLarge`;
    `context-length-text-only-400.json`, `openrouter-400-context.json` and a 422 give `ErrBadRequest`.
    Every row asserts the error string lacks `sk-test-groq-fixture` and the full-body marker.
  - `TestClient_ChatStreamFirstEventRequestTooLarge`: both new SSE fixtures give `ErrRequestTooLarge`; `stream-error-other-code.sse` still gives `ErrUpstreamUnavailable`.
  - `TestClient_ChatStreamRequestTooLargeEventAcrossPartialWrites`: the upstream writes the error event in three flushed pieces split inside the JSON; `Recv` returns `ErrRequestTooLarge`.
  - `TestClient_GeminiRequestSizeErrors`: a Gemini 413 gives `ErrRequestTooLarge`; `invalid-argument-token-count.json` gives `ErrBadRequest` (documents the out-of-scope case); `resource-exhausted.json` still gives `ErrRateLimited`.
  - `TestChat_TierDoesNotSpillOnUpstreamClientStatuses`: 413 removed from its table, 400 and 422 remain.
  - `TestChat_TierSpillsOnUpstreamRequestTooLarge`: a real `openaicompat` client against `httptest.Server` returning `groq-413-tpm.json`, then `context-length-400.json`; each spills to `second` with exactly one upstream call.
  - `TestGroq_UpstreamErrorsBecomeOpenAIErrors`: new row 413 to 400 `context_length_exceeded`.
- Acceptance: tests pass; classification never reads `error.message`; `make check` passes.
- Status: done

### T7: binary-level replay of the OpenCode run

- Files: `tests/e2e/request_size_test.go` (new).
- Depends on: T6.
- Tests first:
  - `TestBinary_RequestTooLargeSpillsLikeLiveOpenCodeRun`: three fake upstreams; config tier `free = [gemini/gemini-3.5-flash, groq/openai/gpt-oss-120b, ollama/llama3.2:3b]` with `free_tier: true` on Gemini and Groq, and `router.auto: [free]`.
    Gemini answers 503, Groq answers 413 with `groq-413-tpm.json`, Ollama streams two chunks and `[DONE]`.
    A streaming `/v1/chat/completions` request with a 40 KB body containing `PROMPT-MARKER-7f3a` gets 200 SSE from Ollama with `x-spillway-route: tier free; candidate 3/3`.
    Each upstream is called once and saw only its own fake `sk-test-...` key; a second request calls Groq again.
    Process output contains `error_kind=upstream_unavailable` and `error_kind=request_too_large`, and never the prompt marker, any key, or `org_test_marker`.
  - `TestBinary_RequestTooLargeEverywhereIsContextLengthExceeded`: Gemini and Groq answer 413 and Ollama 400 `context_length_exceeded`; table over `/v1/chat/completions`, `/v1/responses` and `/v1/messages`.
    Asserts 400 on each; `error.code == "context_length_exceeded"` on chat and responses; Anthropic shape `{"type":"error","error":{"type":"invalid_request_error",...}}` on messages; no provider marker in any body.
  - `TestBinary_DirectRequestTooLargeMakesOneCall`: `model: groq/openai/gpt-oss-120b` gets 400 `context_length_exceeded`, route `direct`, Groq called once, Gemini and Ollama zero times.
- Acceptance: tests pass on Windows and Linux CI; `make check` passes.
- Status: done

### T8: user documentation

- Files: `CONTEXT.md`, `README.md`, `docs/troubleshooting.md`, `docs/limitations.md`, `docs/providers/groq.md`, `tests/e2e/docs_test.go`.
- Depends on: T7.
- Tests first:
  - `TestDocs_TroubleshootingCoversCLIRecoveryCases`: new row `request too large` that wants `context_length_exceeded`.
- Acceptance:
  - `CONTEXT.md` section 5 step 6 reads that a 429, 5xx, timeout, or a provider's request-size rejection (413 or 400 `context_length_exceeded`) spills to a candidate from another provider service, and that a size rejection changes no quota state.
  - `README.md` line 11 says Spillway spills over "when an upstream is temporarily unavailable or rejects a request as too large".
  - `docs/troubleshooting.md` has a section `## context_length_exceeded` covering the plan-time case (declared `context_window`) and the runtime case (every candidate rejected for size), how to read the per-candidate reasons, and the fixes: shorten or compact the conversation, add a model with a larger limit, or declare `context_window` to skip a small model up front.
  - `docs/limitations.md` gains a bullet: only 413 and 400 `context_length_exceeded` are recognised; Gemini's and OpenRouter's over-long-input errors have no distinct code and do not spill.
  - `docs/providers/groq.md` notes that a single request larger than the account's tokens-per-minute limit is rejected with 413 and spills to the next candidate without cooling Groq down.
  - `git grep -n -E "does not spill on 400, 413" -- docs README.md CONTEXT.md` matches only ADR 0006's original Decision text.
  - `make check` passes.
- Status: done

## Manual verification

Offline, by the verifier:

```bash
make check
CGO_ENABLED=0 go build ./...
go test -race -count=1 ./tests/quota/ ./tests/providers/... -run 'RequestTooLarge|RequestSize|MapsRequestSize'
go test -race -count=1 ./tests/api/ -run 'RequestTooLarge'
go test -race -count=1 ./tests/e2e/ -run 'TestBinary_.*RequestTooLarge'
```

Expected: every command exits 0, and each `-run` pattern matches at least one test (`go test -v` lists them).

Live, by the owner only, with the owner's free-tier keys (Git Bash, `jq` required).
Add two temporary tiers to a copy of the live config: `sizecheck: {models: [groq/openai/gpt-oss-120b, gemini/gemini-3.5-flash]}` and `groqonly: {models: [groq/openai/gpt-oss-120b]}`.

```bash
spillway serve -config config.yaml &
KEY=$(spillway key -config config.yaml default)
PROMPT=$(printf 'spillway size check %.0s' $(seq 1 9000))
big() { jq -n --arg m "$1" --arg p "$PROMPT" '{model:$m,max_tokens:16,messages:[{role:"user",content:("Reply with OK. " + $p)}]}'; }
small() { jq -n --arg m "$1" '{model:$m,max_tokens:16,messages:[{role:"user",content:"Reply with OK."}]}'; }
post() { curl -s -D - -o /tmp/body.json -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' --data-binary @- http://127.0.0.1:8080/v1/chat/completions; cat /tmp/body.json; echo; }
```

1. `big spillway/sizecheck | post`
   Expected: `HTTP/1.1 200`, `X-Spillway-Model: gemini-3.5-flash`, `X-Spillway-Route: tier sizecheck; candidate 2/2`.
   Server log: one `tier candidate failed` line with `provider=groq` and `error_kind=request_too_large`.
2. `big spillway/groqonly | post`, then immediately `small spillway/groqonly | post`.
   Expected first: `HTTP/1.1 400`, body `"code":"context_length_exceeded"`, message containing `groq/openai/gpt-oss-120b request too large` and no Groq text such as `org_`, route `tier groqonly; exhausted (1 eligible)`.
   Expected second: `HTTP/1.1 200` from Groq, proving no cooldown.
   `curl -s -H "Authorization: Bearer $(spillway key -config config.yaml dashboard)" http://127.0.0.1:8080/api/quotas` shows Groq without `cooldown_until` and with `breaker_state` `closed`.
3. `big groq/openai/gpt-oss-120b | post`
   Expected: `HTTP/1.1 400`, `"code":"context_length_exceeded"`, `X-Spillway-Route: direct`, one Groq attempt in the log, no other provider attempted.
4. Re-run the 2026-10-29 OpenCode session against `spillway/free` (the ~35k-token coding request).
   Expected, depending on Gemini that day: Gemini serves (`candidate 1/3`); or Gemini fails, Groq logs `request_too_large`, and Ollama serves with `candidate 3/3`.
   If `ollama/llama3.2:3b` declares a `context_window` below the estimate, it is skipped at plan time and the result is 503 `no_candidate_available` when Gemini also failed.
   Record the route header, the attempt lines, whether Ollama's answer shows signs of a truncated prompt (open question 5), and whether OpenCode reacts to a `context_length_exceeded` answer by compacting (open question 7).
5. Copy the Groq 413 message from the server log of step 1, replace the organization ID with `org_test_marker`, and compare it with `tests/providers/openaicompat/testdata/groq-413-tpm.json` (open question 4).

Live result, 2026-10-29, free-tier Groq `openai/gpt-oss-120b` and Gemini `gemini-3.5-flash` with a ~36k-token prompt:
- Step 1: 200 from `gemini-3.5-flash`, route `tier sizecheck; candidate 2/2`, one `error_kind=request_too_large` line for Groq.
- Step 2: 400 `context_length_exceeded`, route `tier groqonly; exhausted (1 eligible)`, message `groq/openai/gpt-oss-120b request too large` without Groq text; the small request right after got 200 from Groq.
- Step 3: 400 `context_length_exceeded`, route `direct`, one Groq attempt.
- Step 5: Groq's 413 message reads "Request too large for model `openai/gpt-oss-120b` in organization `org_...` service tier `on_demand` on tokens per minute (TPM): Limit 8000, Requested 36092, please reduce ...", matching the reconstructed fixture's wording; classification uses only the status, so the body's `type` and `code` were not needed.
- Step 4 (OpenCode re-run) is still open.

## Open questions

1. Does ADR 0015 allow this in a minor release?
   ADR 0015 says "changing a default routing behaviour ... needs a new major version and ADR", while its Decision defines default routing concretely as legacy `auto` without `router.mode` and the classifier staying disabled.
   Options: (a) ship on by default in v1.1.0, with ADR 0034 recording that the spill-trigger list is not part of "default routing" because the change only alters requests that fail today; (b) an opt-in config field such as `router.spill_on_request_too_large: true`, which adds a permanent stable-surface field; (c) wait for v2.
   Recommended default: (a).
2. Which code does a direct reference get for an upstream size rejection?
   Options: `context_length_exceeded` (consistent with the virtual answer, and what OpenAI returns), or keep today's `provider_bad_request`.
   Status is 400 either way; ADR 0015 makes `code` values stable, so this changes which stable code a situation maps to.
   Recommended default: `context_length_exceeded`.
3. Should size rejections be left out of the 429-versus-503 decision in mixed exhaustion?
   Options: leave them out, so `Retry-After` survives when the only other candidates are cooling; or keep today's rule, which answers 503 whenever any candidate has no resume time.
   Recommended default: leave them out.
4. Fixture provenance.
   The Groq 413 TPM body and the 400 `context_length_exceeded` body are reconstructed from public reports, not from Groq's published docs.
   Options: keep the reconstructions; or replace them with the owner's recorded, scrubbed bodies from manual check 5 (and a Groq context-overflow 400 if one can be produced).
   Recommended default: replace with recorded bodies before T6 is closed, keeping fake keys and `org_test_marker`.
5. Ollama and long prompts.
   Ollama is understood to truncate input beyond `num_ctx` rather than reject it, so a spill to a small local model may "succeed" on a truncated prompt.
   Options: document it in `docs/providers/ollama.md` and recommend declaring `context_window` to match `num_ctx`; or say nothing until confirmed.
   Recommended default: confirm in manual check 4, then add the note in a follow-up docs change; do not document an unverified claim in T8.
6. Gemini over-long input.
   Google documents only the generic 400 `INVALID_ARGUMENT` and a 500 `INTERNAL` example for long input, with no distinct structured code.
   Options: leave it out of scope; or capture one live Gemini over-long response (status and body without the prompt) to look for an `ErrorInfo.reason` that could be matched structurally.
   Recommended default: out of scope now; capture one response opportunistically and revisit with a new ADR only if a structured reason exists.
7. Message wording for coding clients.
   Some coding clients may start their own compaction on a context-overflow error, but whether OpenCode reacts to Spillway's code or message is unverified, and message wording is not part of the stable surface.
   Options: keep the fixed Spillway message; or mirror OpenAI's "maximum context length" phrasing to match client heuristics.
   Recommended default: keep the fixed message and rely on the stable code; revisit after manual check 4.

## Appendix A: ADR 0034 draft

T1 creates `docs/adr/0034-request-size-rejections-spill.md` with this text.

```markdown
# 34. A provider's request-size rejection spills without penalizing the provider

Date: 2026-10-29
Status: proposed

## Context

ADR 0006 does not spill on 400, 413 or 422, because the request itself is the likely cause and every candidate would reject it.
That holds for malformed requests but not for size.
Providers differ in context windows and per-minute token limits, so a request one provider service rejects as too large is often accepted by another.
On 2026-10-29 a ~35k-token OpenCode request to a free tier of Gemini, Groq and Ollama failed: Gemini returned 503 and spilled, then Groq returned 413 because its free-tier tokens-per-minute limit is 8000, and the 413 ended the request before Ollama was tried.
Groq's 413 body for that case carries `"code":"rate_limit_exceeded"`, but waiting never helps a request larger than the limit itself, and the same provider still serves smaller requests.
OpenAI reports input beyond a context window as 400 with code `context_length_exceeded`; OpenRouter's `error.code` is numeric, and Gemini reports over-long input with the generic 400 `INVALID_ARGUMENT` or a 500.

## Decision

This amends ADR 0006's list of non-spilling statuses for request-size rejections only.

- An adapter reports `providers.ErrRequestTooLarge` for an upstream HTTP 413 with any body, for an upstream HTTP 400 whose JSON `error.code` is the string `context_length_exceeded`, and for an OpenAI-shaped SSE error event whose `error.code` is `413` or `context_length_exceeded`.
  The HTTP status decides before any body code, and message text is never used to classify.
- For `auto` and `spillway/<tier>`, `ErrRequestTooLarge` spills to the next eligible candidate under every other ADR 0006 rule: the commit point, tier order, at most one attempt per candidate, and, by ADR 0029, never to another instance of the same provider service.
- A request-size rejection is a property of the request, not of the provider: it sets no cooldown, counts no circuit-breaker failure, resets no breaker or backoff, ignores `Retry-After` and rate-limit headers on the response, and leaves the candidate available to every other request.
- Direct model references never spill; a direct request-size rejection answers 400 `invalid_request_error` with code `context_length_exceeded`.
- When every eligible candidate rejected the request for size, the client gets 400 `invalid_request_error` with code `context_length_exceeded` and no `Retry-After`.
  Otherwise request-size rejections are left out of ADR 0006's exhaustion rule: 429 with the earliest `Retry-After` when every other eligible candidate has a known resume time, else 503 `no_candidate_available`.
- The attempt outcome and log `error_kind` is `request_too_large`, the exhaustion message reason is `request too large`, and neither contains provider error text, prompt text or keys.
- Every other 400, and every 422, still does not spill.

## Consequences

- Large coding requests on free tiers reach a configured provider whose limits fit, instead of failing at the first provider with a smaller limit.
- A provider that always rejects a request of that size is tried again on every such request, costing one round trip; Spillway does not learn size limits from failures, in line with ADR 0007, and users can declare `context_window` or reorder tiers to avoid the attempt.
- Each spill sends the whole prompt to another configured provider, as every spill already does.
- ADR 0010 charges failed paid attempts their estimated prompt cost, so a large request that spills across paid candidates can overcount estimated spend more than before.
- Clients see 400 `context_length_exceeded`, a code Spillway already returns under ADR 0008, in more cases; mixed exhaustion now answers 429 or 503 where it used to answer 400 `provider_bad_request`, and a direct upstream 413 keeps status 400 but changes code from `provider_bad_request` to `context_length_exceeded`.
- Spillway reads ADR 0015's "default routing" as `router.mode` and classifier defaults, so this ships in a minor release; it changes only requests that fail today and never which candidate serves a request that succeeds today.
- Gemini's and OpenRouter's over-long-input 400s have no distinct structured code and still do not spill; Gemini's 500 for long input already spills as unavailable and counts a breaker strike.
- Ollama may truncate rather than reject a long prompt, so a spill to a small local model can succeed on a truncated prompt unless its `context_window` is declared.
- For direct references the local access log keeps the scrubbed upstream message, which for Groq names the organization, as it already does for every upstream error.

## Rejected alternatives

- Classifying Groq's 413 as a rate limit would cool every Groq candidate down for requests that fit, and waiting cannot help the rejected request.
- Classifying it as a provider failure would count breaker strikes, so three large requests would stop Groq for everyone.
- Spilling on every 400 would send malformed requests to every provider, spending quota and exposing the request to more providers for no chance of success.
- Matching message text such as "Request too large" or "maximum context length" breaks when providers reword messages and contradicts the coding standard against string matching.
- Answering 413 `request_too_large` would reuse the code for Spillway's own 10 MiB body limit and point users at the wrong fix; answering 429 would make SDKs retry a request that cannot succeed.
- Remembering a per-candidate size ceiling from past rejections would add state, go stale, and infer limits from past results, which ADR 0007 rules out.
- An opt-in config field would add permanent stable surface for behaviour every user of a virtual model wants.

## What needs a new ADR

Classifying any other 400 or 422 as spillable, classifying by message text, letting a request-size rejection change quota or breaker state, or spilling a direct reference.
```

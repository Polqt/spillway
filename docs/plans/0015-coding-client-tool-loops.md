# Coding client tool loops: Gemini thought signatures and Claude Code requests

Status: implemented and shipped in v1.0.0 (7eefc35); live checks still open in local ticket 15
Milestone: v1.0.x client compatibility fixes (additive under ADR 0015; may ship in v1.1.0 instead)

## Goal

Codex on `/v1/responses` and Claude Code on `/v1/messages` complete multi-step tool loops end to end through Spillway, including on Gemini 3 models that require thought signatures.
Gemini thought signatures survive the round trip Gemini -> Spillway chat shape -> client protocol -> client history -> Spillway -> Gemini without Spillway keeping any state.
A real Claude Code request, with its hint fields and later-turn shapes, is accepted and translated instead of failing with `json: unknown field`.

## Scope

In:
- Carry Gemini `thought_signature` values to Responses and Anthropic Messages clients inside the tool-call ID they must echo, and restore them on the next request.
- Shape assistant tool-call history per provider: forward Google's `extra_content` only to Gemini, strip it for other OpenAI-compatible providers, and fill Google's documented placeholder on unsigned calls sent to Gemini (open question 1).
- Fix Responses history grouping so parallel and text-plus-call steps reach providers as one assistant message.
- Fix Anthropic history ordering so tool results directly follow the assistant tool calls.
- Accept Claude Code's `metadata`, `thinking`, `output_config.effort`, `context_management`, and `cache_control` fields as hints, and its later-turn `thinking`, `redacted_thinking`, and `tool_result.is_error` shapes.
- Defer tool-call events in translated streams until each call is complete, so the final ID is known when the client first sees it.
- An ADR, glossary terms, client docs, binary-level tests with fake upstreams, and a live manual check with `codex exec` and `claude -p`.

Out:
- Thought signatures on non-function-call parts (Google: recommended, not validated); deferred until a client-visible quality problem is measured.
- Gemini thought summaries or reasoning text surfaced as Responses `reasoning` items or Anthropic `thinking` blocks; deferred, no milestone.
- Images inside Anthropic `tool_result` content (open question 5); stays a clear 400 in this plan.
- Anthropic `output_config.format`, server tools, `container`, `mcp_servers`, `document` blocks, and `citations`; they remain clear 400s.
- `POST /v1/messages/count_tokens` (open question 9).
- A native Gemini `generateContent` adapter; ADR 0001 stands because the compatibility endpoint exposes signatures.
- Spilling on Gemini's signature 400; ADR 0006 is unchanged and this plan prevents the 400 instead.
- Codex `custom_tool_call` (freeform `apply_patch`) items; tracked separately if the live run shows Codex sending them to non-OpenAI models.

Precondition: commit 1cdedac (`fix(api): accept Codex namespaced tools and replayed output text`, PR #32) is the base for T1.
This plan builds on its `responseInputMessages(raw, namespaces)` and `responsesFunctionCall` helpers and its lenient decoding of Responses input items and tools.
The unrelated WIP in `internal/evaluation/report.go` and the untracked `scripts/test-codex-responses-live.ps1` are not part of this plan.

## Design

### What the code does today

1. `internal/providers/gemini` wraps `openaicompat.Client` against `https://generativelanguage.googleapis.com/v1beta/openai` (ADR 0001).
   Request and response bodies pass through verbatim, so on `/v1/chat/completions` Gemini's `tool_calls[].extra_content.google.thought_signature` already reaches the client and, if the client echoes it, reaches Gemini again.
2. The Responses and Messages translators decode chat responses into typed structs (`chatCompletionResponse`, the stream delta structs) that have no `extra_content` field, so the signature is dropped.
   The client echoes only `call_id` or `tool_use.id`, the next request carries no signature, Gemini 3 answers 400, and ADR 0006 does not spill on 400.
   This is the live failure reproduced on 2026-10-27.
3. `responseInputMessages` turns every `function_call` item into its own assistant message.
   For Gemini parallel calls only the first call carries a signature, so the second call becomes a separate unsigned model step and fails even after signatures are carried.
   It also produces `assistant(call1), assistant(call2), tool, tool`, which strict OpenAI-compatible providers reject because a tool message must follow the assistant message that holds its call.
4. `anthropicMessages` emits a user message's text before its `tool_result` messages.
   Claude Code appends text blocks after tool results, so the upstream order becomes `assistant(tool_calls), user(text), tool(result)`.
5. `decodeClientJSON` rejects Claude Code's top-level `metadata`, `thinking`, `output_config` and `context_management`, and `cache_control` on system blocks, content blocks and tools.
6. `anthropicContent` concatenates system text blocks with no separator, merging Claude Code's separate system blocks into one run-on string.
7. `chatHandler.servePlan` sends the same `RawBody` to every candidate, so anything provider-specific must happen inside the adapter, not in `internal/api`.

### External facts this plan relies on (checked 2026-10-27)

Google, `https://ai.google.dev/gemini-api/docs/generate-content/thought-signatures` (the old `/thought-signatures` URL now points at an Interactions-API page):
- On the OpenAI-compatible endpoint the signature is `tool_calls[i].extra_content.google.thought_signature`; tool-call IDs look like `function-call-<uuid>`.
- Gemini 3 and 4 put a signature on the first function call of each response; parallel calls after the first have none.
- Strict validation covers the current turn only: every model step after the most recent user message with ordinary content, where the first function call of each step must carry its signature or the request fails with 400.
- Parallel calls must be replayed as `FC1+signature, FC2, FR1, FR2`; interleaving gives 400.
- For history from another model, or calls not generated by the API, the documented placeholder values are `context_engineering_is_the_way_to_go` and `skip_thought_signature_validator`. Google calls this injection strongly discouraged except when it cannot be avoided.
- In streams, read until `finish_reason`; the signature may arrive in a later part.

Anthropic, `https://platform.claude.com/docs/en/api/messages` and the thinking, effort and context-editing guides:
- `metadata.user_id` is an opaque per-user identifier, at most 512 characters, used by Anthropic for abuse detection.
- `output_config.effort` is one of `low`, `medium`, `high`, `xhigh`, `max`; `output_config.format` is a JSON-schema structured-output request.
- `thinking.type` is `enabled` (with `budget_tokens` of at least 1024 and below `max_tokens`), `disabled`, `adaptive`, or `between_tools`; `enabled` and `adaptive` take an optional `display` of `summarized` or `omitted`.
- `context_management` is a beta field (`anthropic-beta: context-management-2025-06-27`) whose `edits` include `clear_tool_uses_20250919` and `clear_thinking_20251015`.
- `cache_control` is `{"type":"ephemeral","ttl":"5m"|"1h"}` and may appear at top level, on system text blocks, text, image, `tool_use`, `tool_result`, text blocks inside `tool_result`, and tools.
- `tool_result` has `tool_use_id`, `content`, optional `is_error` and `cache_control`; `tool_use.id` and `tool_result.tool_use_id` match `^[a-zA-Z0-9_-]+$`.
- Assistant history may contain `thinking` blocks (`thinking`, `signature`) and `redacted_thinking` blocks (`data`).

Client behaviour (from the live reproduction): Codex echoes `function_call` items with their `call_id` and pairs `function_call_output.call_id`; Claude Code echoes `tool_use.id` and pairs `tool_result.tool_use_id`.
Both drop fields they do not model, so neither echoes `extra_content`.

### Decision 1: the signature's canonical home inside Spillway is Google's own field

Inside Spillway the chat shape is the lingua franca, and Google already defines where the signature lives in that shape.
The translators read `tool_calls[].extra_content.google.thought_signature` from chat responses and write it back onto chat-shape assistant tool calls when rebuilding history.
This keeps `/v1/chat/completions` verbatim (clients that echo `extra_content` already work) and means the providers layer, not `internal/api`, decides who receives it.

### Decision 2: clients carry the signature inside the tool-call ID

The signed tool-call ID is the client-visible carrier.
When a translated tool call has a signature, the ID Spillway gives the client is:

```text
spwts1_<base64url, no padding, of {"id":"<provider tool-call id>","ts":"<thought signature>"}>
```

The alphabet `[A-Za-z0-9_-]` satisfies Anthropic's `tool_use.id` pattern and any string-typed Responses `call_id`.
An unsigned tool call keeps its provider ID unchanged, so Groq, OpenRouter and Ollama output does not change at all.
On every inbound request the translators decode IDs in all positions that pair a call with its result: Responses `function_call.call_id` and `function_call_output.call_id`, Anthropic `tool_use.id` and `tool_result.tool_use_id`.
Both sides of a pair decode to the same provider ID, so pairing upstream is exact even if a client mixes signed and plain forms.
In Responses output the item `id` is the provider ID and only `call_id` is signed, which keeps the echoed history smaller; Codex's echoed item IDs are already ignored.

Options considered:
- Signed tool-call ID (chosen).
  It is the one field both clients must echo byte-for-byte to pair results, it needs no client changes, it works the same for both protocols, and it is stateless across restarts and multiple Spillway processes.
  Costs: longer IDs in client transcripts, and Spillway must keep decoding `spwts1_` IDs for as long as old client sessions can be resumed, so the format is versioned and recorded in an ADR.
- Protocol-native reasoning carriers (rejected): a Responses `reasoning` item with `encrypted_content`, or an Anthropic `thinking.signature` or `redacted_thinking.data` block.
  They need two different mappings, depend on each client replaying reasoning items or thinking blocks (Claude Code's `clear_thinking` edits and model switches may drop them), associate a signature with a call only by position, and show empty thinking blocks in Claude Code's UI.
- Server-side map from call ID to signature (rejected).
  It breaks Spillway's statelessness (Responses `store` is ignored by design), is lost on restart, needs bounded memory and eviction, and persisting model-derived content would contradict ADR 0009.
- Placeholder signature only (rejected as the primary path, kept as the fallback in Decision 4).
  Google limits it to history it did not generate and warns that it degrades reasoning, which matters most in exactly the multi-step coding loops this plan fixes.

Bounds and validation, enforced in one codec:
- Provider tool-call ID: 1 to 256 bytes of printable ASCII without spaces.
- Signature: 1 to `maxThoughtSignatureBytes` (64 KiB, open question 2) from the alphabet `[A-Za-z0-9+/=_-]`.
- Inbound: an ID without the `spwts1_` prefix is a plain ID and passes through as today.
  A prefixed ID is checked for encoded length before decoding, then decoded with strict unpadded base64url and strict JSON (`id` and `ts` only), then validated as above.
  Any failure is a 400 `invalid_request` that names the input position and never echoes the ID or any decoded part.
- Outbound: a signature that fails validation or exceeds the bound is not embedded; the client gets the plain provider ID, and Spillway logs one warning with stable keys `provider`, `reason` (`too_large` or `invalid`) and `bytes`, never the value.

Integrity protection (an HMAC over the envelope) is rejected.
The authenticated local client already controls the entire prompt, a forged signature only reaches the user's own Gemini key, and Gemini validates signatures itself.
An HMAC would add a persistent local secret and key-rotation semantics for no security gain.

Key types and signatures in `internal/api/toolcall_id.go`:

```go
const (
	signedToolCallIDPrefix   = "spwts1_"
	maxThoughtSignatureBytes = 64 << 10
	maxProviderToolCallID    = 256
)

// toolCallRef is a provider tool-call ID and the Gemini thought signature it carries, if any.
type toolCallRef struct {
	ID               string
	ThoughtSignature string
}

// clientToolCallID returns the ID to give the client and false when the signature was not embedded.
func clientToolCallID(ref toolCallRef) (string, bool)

// parseClientToolCallID decodes a client-echoed ID; plain IDs return {ID: id}.
func parseClientToolCallID(id string) (toolCallRef, error)

// chatToolCall builds a chat-shape tool call, adding extra_content only when a signature is present.
func chatToolCall(ref toolCallRef, name, arguments string) map[string]any
```

The chat response and stream delta structs in `responses_compat.go`, `anthropic_compat.go` and `client_compat.go` gain one shared field type:

```go
type googleExtraContent struct {
	Google struct {
		ThoughtSignature string `json:"thought_signature"`
	} `json:"google"`
}
```

### Decision 3: providers shape tool-call history; Gemini keeps `extra_content`, others drop it

`extra_content` is a Google extension, and strict OpenAI-compatible APIs reject unknown properties on assistant messages.
Because every candidate receives the same body, the adapter decides:

```go
// internal/providers/openaicompat
type Options struct {
	// ... existing fields ...
	// KeepToolCallExtraContent forwards extra_content on assistant tool calls; other OpenAI-compatible APIs may reject it.
	KeepToolCallExtraContent bool
}
```

With the default `false`, `requestBody` removes `extra_content` from every `messages[].tool_calls[]` entry.
It rewrites only the `messages` value span, the same span technique `replaceModel` already uses, so every other top-level value stays byte-identical.
A body with no `extra_content` is forwarded exactly as today.
`gemini.New` sets `KeepToolCallExtraContent: true`.
This is the only change to verbatim pass-through on `/v1/chat/completions`, and it only removes a field the receiving provider does not define.

### Decision 4: Gemini fills Google's placeholder on unsigned tool calls (open question 1)

Before delegating `Chat` and `ChatStream`, the Gemini adapter checks each assistant message with tool calls.
If none of its calls carries a non-empty signature, it sets `extra_content.google.thought_signature` to `skip_thought_signature_validator` on the first call only.
It never replaces a real signature, never touches other messages, and returns the body byte-identical when nothing needs filling.
It applies to every Gemini model, because ADR 0007 forbids inferring model behaviour from model names; manual verification confirms that a Gemini 2.5 model accepts the placeholder.

This covers every case where Spillway cannot have a real signature: Groq-originated calls replayed to Gemini after spillover, Codex and Claude Code sessions recorded before this change, chat-path clients that drop `extra_content`, and oversized signatures that were not embedded.
Without it those requests fail with a 400 that does not spill.
Applying it to older turns is harmless because Google validates only the current turn, and it avoids Spillway re-implementing Google's turn boundary rule.

### Decision 5: one assistant message per model step in Responses history

`responseInputMessages` accumulates consecutive `function_call` items into one assistant message.
An assistant `message` item immediately followed by `function_call` items becomes one assistant message with both `content` and `tool_calls`.
An assistant message with only tool calls has `content: null`.
A `function_call_output`, a user or system message, or the end of input closes the step.
Sequential steps (`call, output, call, output`) stay separate assistant messages, which is what Gemini expects for multi-step signatures.

### Decision 6: tool results first in Anthropic user messages

`anthropicMessages` emits a user message's `tool_result` blocks as tool messages before any text or image content from the same message, preserving block order within each group.
An assistant message with only `tool_use` blocks has `content: null` instead of `[]`.
System text blocks are joined with a blank line (`"\n\n"`).

### Decision 7: translated streams emit each tool call once it is complete

Today a tool call's start event (`response.output_item.added`, `content_block_start`) is written on the call's first delta, before a later delta can carry the signature, and Anthropic offers no way to change a started block's ID.
The Responses and Anthropic stream states keep accumulating tool deltas (ID, name, arguments, signature) but write no tool events until `finish_reason` or `[DONE]`.
At finish they emit, per call in index order, the full event sequence with the final ID: for Responses `output_item.added`, one `function_call_arguments.delta` with the full arguments, `function_call_arguments.done`, `output_item.done`; for Anthropic `content_block_start`, one `input_json_delta`, `content_block_stop`.
Text deltas still stream live.
If the upstream fails mid-stream, the existing error event is written and no partial tool call ever reaches the client, which is safer than today.
Neither client acts on a tool call before it is complete, so the cost is only that argument text appears at the end of the stream.
The rejected alternative, emitting on the first delta and reading the signature from that same delta, works only if Gemini always sends the signature in the call's first chunk, which Google does not promise (open question 7).

### Decision 8: Claude Code request fields

Accepted fields are validated for shape so malformed input still fails loudly, then ignored unless the table says otherwise.
None of them is forwarded upstream.

| Field | Decision | Reason |
|---|---|---|
| `metadata.user_id` (string up to 512 or null) | Accept, never forward, never log | It identifies the user to Anthropic; sending it to Gemini or Groq would leak an identifier to a third party. Unlike Responses `metadata`, which is forwarded today, it is not a client-chosen label. |
| `thinking` `adaptive`, `disabled`, `between_tools`, `enabled` (budget 1024 or more and below `max_tokens`), optional `display` | Accept as a hint | Anthropic itself may return no thinking blocks for `adaptive`, and Spillway never emits thinking blocks. This mirrors how Responses `reasoning.summary` is accepted. Replaces the existing rejection (open question 3). |
| Unknown `thinking.type`, invalid `budget_tokens` | Reject, 400 | Mirrors Anthropic's own validation. |
| `output_config.effort` (5 values) | Accept as a hint, not translated | Translating to `reasoning_effort` would make Groq models without reasoning answer 400, which does not spill (open question 4). |
| `output_config.format` | Reject, 400 | Structured output through Messages is out of scope. |
| `context_management.edits[]` (objects with a string `type`) | Accept, ignore | `clear_thinking_*` is a no-op because thinking is never forwarded; ignoring `clear_tool_uses_*` sends full history, which costs tokens but keeps meaning. |
| `cache_control` at every documented position | Accept, ignore | Prompt caching is Anthropic-specific; the shape `{"type":"ephemeral","ttl"?:"5m"\|"1h"}` is validated. |
| `tools[].type: "custom"` | Accept | Equivalent to an untyped tool. |
| `thinking` and `redacted_thinking` blocks in assistant history | Accept, drop | Their signatures are Anthropic-specific and cannot be replayed to another provider. |
| `messages[].role: "system"` | Accept, forward as a Chat Completions system message in place; one sent between a tool call and its result moves after the result | The live run showed Claude Code sends them mid-conversation. Chat Completions requires tool results directly after their calls. |
| `tools[].defer_loading` | Accept, forward the tool without the flag | Spillway sends every tool up front, so on-demand loading has no equivalent. |
| `tool_result.is_error` | Accept, drop the flag, keep the content verbatim | Chat tool messages have no error flag; Claude Code already wraps error text in its content. Prefixing text was rejected because it changes content the client wrote. |
| `tool_result` text blocks with `cache_control` | Accept | Same as other blocks. |
| `tool_result` image blocks | Reject, 400 with a clear message | Chat tool messages are text-only (open question 5). |
| `container`, `mcp_servers`, `service_tier`, `speed`, `inference_geo`, `diagnostics`, server tools, `document`, `citations` | Reject, 400 (unchanged) | Not sent in the captured Claude Code requests; the live run will reveal any new field. |

Headers such as `anthropic-version` and `anthropic-beta`, and the `?beta=true` query Claude Code adds, are already ignored by routing and need no change.

### Cross-provider matrix

| History origin -> next candidate | What the upstream receives |
|---|---|
| Gemini, signed -> same Gemini instance | The real signature on the original call ID |
| Gemini, signed -> Groq, OpenRouter or Ollama | The original call ID, no `extra_content` |
| Groq or other, unsigned -> Gemini | The placeholder on the first call of each unsigned assistant message |
| Gemini model A, signed -> Gemini model B in the same tier | The real signature; acceptance across models is unverified (open question 6) |
| Session recorded before this change -> Gemini | The placeholder |
| `/v1/chat/completions` client that echoes `extra_content` -> Gemini | The client's signature, verbatim |
| `/v1/chat/completions` client that drops `extra_content` -> Gemini | The placeholder |

No decision in this plan changes candidate order, spill conditions, cooldowns, or which instances a route may contain.

### Error model

- A malformed or oversized signed ID: 400 with the protocol's error shape (`invalid_request_error`), code `invalid_request`, and a message naming the input position only, for example `input item 3 has a malformed Spillway tool call ID`.
- Newly rejected Anthropic fields: 400 `invalid_request_error` whose message names the field, as today.
- An upstream signature that cannot be embedded: no client error; a warning log without the value.
- A body that cannot be reshaped in an adapter (impossible after `internal/api` validation): a wrapped error from `Chat`/`ChatStream` that does not include body content.
- Gemini's own signature 400 remains `ErrBadRequest` and does not spill (ADR 0006).

### Security properties and how tests prove them

- Signatures are model-derived content: absent from logs, the usage store, `x-spillway-*` headers and error messages (log-capture and error-string tests).
- Client-supplied signed IDs are untrusted: bounded before decoding, strictly decoded, charset-checked, inserted with `encoding/json`, sent only to the Gemini instance chosen for the request, and never echoed in errors.
- `metadata.user_id` never leaves the machine and is absent from logs.
- The inbound `Authorization` header is still never forwarded; the e2e fakes assert they see only the configured fake provider key.

### Documentation

ADR 0033 records the signed tool-call ID format, its versioning promise, the `extra_content` shaping and the placeholder fallback, and notes the narrowed meaning of ADR 0001's "requests pass through verbatim".
`GLOSSARY.md` gains **Thought signature** and **Signed tool call ID**.
`docs/clients.md` lists the accepted Claude Code fields, what is ignored, what is rejected, and the signature behaviour.

## Hard-rule check

- No quota evasion: routing, spill conditions, cooldowns and same-service rules are untouched; signatures never influence candidate choice, and the placeholder is Google's documented mechanism for history it did not generate, not a limit bypass.
- No credential collection: signatures and `metadata.user_id` are not credentials and nothing new is stored; the envelope is self-contained in the client's own history.
- Secrets never leak: no new header forwarding; tests prove signatures, signed IDs and `user_id` are absent from logs and errors, and that upstreams see only the provider key.
- Single binary, no cgo: only `encoding/base64`, `encoding/json` and existing packages; no new module.
- Local by default: no listener or binding change and no new outbound destination.
- OpenAI compatibility is the contract: `/v1/chat/completions` stays verbatim except that a Google-only field is removed for providers that do not define it; Responses `call_id` and Anthropic `tool_use.id` remain valid opaque strings that match Anthropic's documented pattern.

## Tasks

### T1: ADR 0033 and glossary terms
- Files: `docs/adr/0033-gemini-thought-signatures-in-tool-call-ids.md` (new), `GLOSSARY.md`
- Depends on: none (the precondition WIP commit is already in)
- Tests first: none; documentation only, with no behaviour to test.
- Acceptance: the ADR states context, the decision (Decisions 1 to 4 and 7), the rejected alternatives, the `spwts1_` format with its bounds, and the promise that `spwts1_` IDs stay decodable within v1; it references ADRs 0001, 0006, 0007 and 0015; `GLOSSARY.md` defines **Thought signature** and **Signed tool call ID** with `_Avoid_` lines; one sentence per line, no em dashes; `make check` passes.
- Status: done

### T2: provider-side `extra_content` shaping
- Files: `internal/providers/openaicompat/client.go`, `internal/providers/gemini/gemini.go`, `tests/providers/openaicompat/client_test.go`, `tests/providers/gemini/gemini_test.go`, `tests/providers/gemini/testdata/chat-tool-signature.json` (new), `tests/providers/gemini/testdata/stream-tool-signature.sse` (new), `tests/providers/openaicompat/testdata/request-tool-extra-content.json` (new)
- Depends on: T1
- Tests first:
  - `TestClient_StripsToolCallExtraContentByDefault`: table over `Chat` and `ChatStream`, one signed call and two parallel calls; the upstream body has no `extra_content`, call IDs, names and arguments are unchanged, and every other top-level raw value is byte-identical.
  - `TestClient_BodyWithoutExtraContentIsForwardedUnchanged`: upstream bytes equal the request fixture with only `model` replaced, as today.
  - `TestClient_KeepsToolCallExtraContentWhenEnabled`: with the option set, `extra_content` arrives as the same raw JSON.
  - `TestClient_MalformedMessagesErrorOmitsBody`: a non-array `messages` with a sentinel string returns an error whose text lacks the sentinel and the provider key.
  - `TestProvider_ForwardsThoughtSignaturesToGemini`: the Gemini adapter forwards `extra_content.google.thought_signature` verbatim.
  - `TestProvider_ChatReturnsToolCallSignatureVerbatim`: the recorded fixture's `extra_content` reaches the caller unchanged.
  - `TestProvider_ChatStreamDeliversSignatureChunksVerbatim`: partial chunks (ID and name, then arguments, then a chunk with only `extra_content`, then finish) are each returned by `Recv` unchanged.
  - Fixtures hold fake values only (`sk-test-...` keys, signatures such as base64 of `signature-for-tests`).
  - Mid-stream upstream errors and client disconnect on this unchanged response path stay covered by `TestClient_ChatStreamSurfacesUpstreamFailureAfterChunk` and `TestClient_ClosingChatStreamCancelsUpstream`.
- Acceptance: `TestClient_UnknownFieldsPreserveRawValuesAndNeverForwardClientAuth` still passes; Groq, OpenRouter and Ollama instances never send `extra_content`; Gemini instances always do when present; `make check` passes.
- Status: done

### T3: Gemini placeholder signatures for unsigned calls (subject to open question 1)
- Files: `internal/providers/gemini/gemini.go`, `tests/providers/gemini/gemini_test.go`
- Depends on: T2
- Tests first:
  - `TestProvider_AddsPlaceholderSignatureToUnsignedToolCalls`: table over `Chat` and `ChatStream`: one unsigned call gets `skip_thought_signature_validator`; unsigned parallel calls get it on the first call only; a mixed history changes only the unsigned assistant message; tool and user messages are untouched.
  - `TestProvider_PlaceholderNeverReplacesRealSignature`: a signed message is forwarded byte-identical.
  - `TestProvider_BodyWithoutToolCallsIsForwardedUnchanged`: upstream bytes equal the input with only `model` replaced.
- Acceptance: a Groq-shaped tool history sent to the Gemini adapter reaches the fake Gemini with exactly one placeholder per unsigned assistant message; `make check` passes.
- Status: ready-for-human

### T4: Responses history grouping
- Files: `internal/api/responses_compat.go`, `tests/api/client_compat_test.go`
- Depends on: T1
- Tests first:
  - `TestResponses_GroupsParallelFunctionCallsIntoOneAssistantMessage`: `[user, fc1, fc2, out1, out2]` becomes `[user, assistant{tool_calls:[fc1, fc2]}, tool1, tool2]`.
  - `TestResponses_FoldsFunctionCallsIntoPrecedingAssistantText`: `[user, assistant "Checking.", fc1, out1]` becomes one assistant message with that content and one tool call, then the tool message.
  - `TestResponses_KeepsSequentialStepsSeparate`: `[user, fc1, out1, fc2, out2]` yields two assistant messages.
  - `TestResponses_ToolOnlyAssistantHasNullContent`: grouped tool-only steps carry `"content": null`.
- Acceptance: `TestResponses_RestoresNamespacedToolHistory` still passes with namespaces restored inside grouped calls; `make check` passes.
- Status: done

### T5: signed tool-call IDs on Responses (non-streaming)
- Files: `internal/api/toolcall_id.go` (new), `internal/api/responses_compat.go`, `internal/api/client_compat.go`, `tests/api/client_compat_test.go`, `tests/api/testdata/gemini-signed-tool-call.json` (new)
- Depends on: T4
- Tests first:
  - `TestResponses_SignedToolCallRoundTripsThoughtSignature`: the fake returns `function-call-1` with a fake signature; the Responses output item has `id` `function-call-1` and a `call_id` starting `spwts1_`; echoing that `function_call` and its `function_call_output` reaches the fake as tool call `function-call-1` with the original signature in `extra_content`, and a tool message with `tool_call_id` `function-call-1`.
  - `TestResponses_UnsignedToolCallKeepsProviderID`: no signature means `id` and `call_id` equal the provider ID.
  - `TestResponses_ParallelCallsSignOnlyTheFirst`: only the first `call_id` is signed, and the replay restores the signature on the first call only.
  - `TestResponses_MalformedSignedCallIDIsRejected`: table of bad base64, padding, unknown JSON field, empty signature, invalid signature characters, oversized ID and oversized encoded length; each gives 400 `invalid_request_error` whose message lacks the submitted ID and any decoded fragment.
  - `TestResponses_OversizedUpstreamSignatureIsNotEmbedded`: a signature over the bound yields the plain ID; the log has the warning with `reason=too_large` and lacks the signature.
  - `TestClientCompatibility_ThoughtSignatureAbsentFromLogs`: after a full round trip `e.logs` contains neither the signature nor the signed ID.
- Acceptance: security properties "signature absent from logs" and "client-supplied ID absent from error text" are proven by the tests above; existing Responses tests pass; `make check` passes.
- Status: done

### T6: Responses streaming with deferred tool calls
- Files: `internal/api/responses_compat.go`, `internal/api/client_compat.go`, `tests/api/client_compat_test.go`
- Depends on: T5
- Tests first:
  - `TestResponses_StreamEmbedsThoughtSignature`: one tool chunk carrying ID, name, arguments and signature yields `output_item.added` and `output_item.done` with the same signed `call_id`.
  - `TestResponses_StreamSignatureAfterArgumentsChunk`: partial chunks (ID and name; first argument fragment; last fragment with `extra_content`; finish) yield exactly one `output_item.added` for the call, a signed `call_id` identical in added and done items, and `function_call_arguments.done` with the joined arguments.
  - `TestResponses_StreamTextStillStreamsBeforeToolCalls`: text deltas are written before the finish chunk arrives.
  - `TestResponses_StreamUpstreamErrorDuringToolCallEmitsNoFunctionCall`: a tool delta followed by an upstream error gives `event: error`, no `response.completed`, and no `function_call` item.
  - `TestResponses_StreamClientDisconnectClosesUpstream`: with a real `httptest.Server` and a gated fake stream, cancelling the client after `response.created` closes the provider stream and cancels its context (pattern of `TestStream_ClientDisconnectCancelsProvider`).
- Acceptance: `TestResponses_StreamsNamespacedToolCalls` and the `responses` subtest of `TestClientCompatibility_StreamsToolCalls` pass; `sequence_number` stays strictly increasing; `make check` passes.
- Status: done

### T7: Claude Code request fields on Messages
- Files: `internal/api/anthropic_compat.go`, `tests/api/client_compat_test.go`, `tests/api/testdata/claude-code-first-turn.json` (new)
- Depends on: T1
- Tests first:
  - `TestAnthropicMessages_AcceptsClaudeCodeFirstTurn`: the fixture, a sanitized copy of the captured request with `metadata.user_id` set to `user_test_0000` and two tools, returns 200; the upstream body contains none of `metadata`, `user_test_0000`, `thinking`, `output_config`, `context_management` or `cache_control`; system blocks are joined with `"\n\n"`.
  - `TestAnthropicMessages_AcceptsHintFields`: table over every `thinking` type with and without `display`, all five `effort` values, both edit types, top-level `cache_control`, and `cache_control` with `ttl: "1h"` on a tool and on an image.
  - `TestAnthropicMessages_RejectsUnsupportedFieldsWithAnthropicError`: rewritten as a table of unknown `thinking.type`, `budget_tokens` 512, `budget_tokens` not below `max_tokens`, `output_config.format`, unknown `effort`, `cache_control.type` other than `ephemeral`, `container`, `mcp_servers`, and a non-string `user_id`; each is 400 `invalid_request_error` naming the field.
  - `TestAnthropicMessages_DoesNotForwardOrLogMetadataUserID`: the `user_id` value is absent from the upstream body and from `e.logs`.
- Acceptance: the old `thinking.enabled` rejection is replaced as decided in open question 3; security property "user identifier absent from upstream body and logs" is proven; `make check` passes.
- Status: done

### T8: Claude Code later-turn shapes on Messages
- Files: `internal/api/anthropic_compat.go`, `internal/api/responses_compat.go` (`responseOutputText` accepts `cache_control` on text parts), `tests/api/client_compat_test.go`, `tests/api/testdata/claude-code-tool-turn.json` (new)
- Depends on: T7
- Tests first:
  - `TestAnthropicMessages_AcceptsClaudeCodeToolTurn`: the fixture (assistant `thinking` with a fake signature, `redacted_thinking`, text, `tool_use` with `cache_control`; user `tool_result` with `is_error: true`, array text content with `cache_control`, then a text block) returns 200 and reaches upstream as `assistant{content, tool_calls}`, `tool`, `user`.
  - `TestAnthropicMessages_OrdersToolResultsBeforeUserText`: table of `[tool_result, text]` and `[text, tool_result]`; tool messages come first in both.
  - `TestAnthropicMessages_DropsThinkingBlocksFromHistory`: thinking text, its signature and `redacted_thinking.data` are absent upstream.
  - `TestAnthropicMessages_ToolOnlyAssistantHasNullContent`.
  - `TestAnthropicMessages_ToolResultIsErrorKeepsContent`: content arrives verbatim.
  - `TestAnthropicMessages_ToolResultImageIsRejectedClearly`: 400 naming image tool results (per open question 5).
- Acceptance: existing Anthropic tests pass; `make check` passes.
- Status: done

### T9: signed tool_use IDs on Messages (non-streaming and streaming)
- Files: `internal/api/anthropic_compat.go`, `internal/api/client_compat.go`, `tests/api/client_compat_test.go`
- Depends on: T5, T6, T8
- Tests first:
  - `TestAnthropicMessages_SignedToolUseRoundTripsThoughtSignature`: the `tool_use.id` starts `spwts1_` and matches `^[a-zA-Z0-9_-]+$`; echoing `tool_use` and `tool_result` restores the provider ID and signature upstream.
  - `TestAnthropicMessages_MalformedSignedToolUseIDIsRejected`: table over `tool_use.id` and `tool_result.tool_use_id`; 400 whose message lacks the submitted ID.
  - `TestAnthropicMessages_StreamSignatureAfterArgumentsChunk`: partial chunks with the signature in a later chunk yield one `content_block_start` whose `tool_use` ID is signed, one `input_json_delta` with the joined arguments, and `content_block_stop`.
  - `TestAnthropicMessages_StreamTextStillStreamsBeforeToolCalls`.
  - `TestAnthropicMessages_StreamUpstreamErrorDuringToolCallEmitsNoToolUse`: `event: error`, no `message_stop`, no `tool_use` block.
  - `TestAnthropicMessages_StreamClientDisconnectClosesUpstream`.
- Acceptance: the `anthropic` subtest of `TestClientCompatibility_StreamsToolCalls` and `TestClientCompatibility_StreamErrorDoesNotEmitSuccessTerminalEvent` pass; block indexes are contiguous from 0; `make check` passes.
- Status: done

### T10: binary-level tool loops and spillover across providers
- Files: `tests/e2e/thought_signature_test.go` (new)
- Depends on: T2, T3, T6, T9
- Tests first (each starts the built binary with loopback fake Gemini and Groq upstreams configured through `base_url`, fake `sk-test-...` provider keys via `t.Setenv`, and a local `sk-test-...` key):
  - `TestBinary_GeminiSignedToolLoopThroughResponses`: two streaming `/v1/responses` requests shaped like Codex; the fake Gemini asserts the second request carries the real signature on the first call and answers 200.
  - `TestBinary_GeminiSignedToolLoopThroughMessages`: the same through `/v1/messages`, shaped like Claude Code.
  - `TestBinary_SpilloverGeminiHistoryToGroqStripsSignature`: tier `[gemini, groq]`; on the second request Gemini returns 429, and the fake Groq receives the provider call ID with no `extra_content`; `x-spillway-route` names candidate 2/2.
  - `TestBinary_SpilloverGroqHistoryToGeminiAddsPlaceholder`: tier `[groq, gemini]`; on the second request Groq returns 429, and the fake Gemini receives the placeholder on the first call (dropped if open question 1 is declined).
  - `TestBinary_ChatCompletionsForwardsThoughtSignatureVerbatim`: `extra_content` in a `/v1/chat/completions` request reaches the fake Gemini unchanged, and the response's `extra_content` reaches the client unchanged.
  - Every fake asserts its `Authorization` is the configured provider key and never the local key, and every test asserts the server output lacks the signature strings.
- Acceptance: tests make no external calls; `make check` passes.
- Status: done

### T11: client documentation
- Files: `docs/clients.md`, `tests/e2e/docs_test.go` (only if `TestDocs_ClientsStateSupportedProtocols` needs the new statements)
- Depends on: T10
- Tests first: extend `TestDocs_ClientsStateSupportedProtocols` to require statements that Claude Code `thinking`, `output_config.effort`, `context_management`, `metadata` and `cache_control` are accepted and not forwarded, and that Gemini thought signatures travel in tool-call IDs.
- Acceptance: `docs/clients.md` lists accepted, ignored and rejected Anthropic fields, explains signed tool-call IDs and the placeholder fallback, and states the image-in-tool-result limitation; one sentence per line; `make check` passes.
- Status: done

After T11, run `verifier`, `/code-review` since the precondition commit, and `security-auditor` (this plan touches HTTP clients, request shaping and logging), then re-run `verifier` after fixes.

2026-10-28 verification: standards and spec reviews completed, the spec review's CR/LF base64 finding was fixed and covered by a regression test, and `make check` passed.
The CSO audit was not assessed because its private helper rejected the initial 90-second budget; its one-run rule prohibited retrying with a corrected budget.

## Manual verification

Use PowerShell from the repo root, a config whose Gemini instance is named `gemini` (substitute yours), a tier containing a Gemini 3 model, and a Groq instance.

1. Static checks.
   ```powershell
   make check
   ```
   Expected: exit 0.

2. Start the gateway.
   ```powershell
   make build
   .\bin\spillway.exe check -config $cfg
   .\bin\spillway.exe serve -config $cfg
   ```
   Expected: `check` exits 0 and `serve` logs that it is listening on `127.0.0.1:8080`.

3. Reproduce the original Responses failure and confirm the fix (second terminal).
   ```powershell
   $key = (.\bin\spillway.exe key -config $cfg default | Out-String).Trim()
   $h = @{ Authorization = "Bearer $key" }
   $tools = @(@{ type = 'function'; name = 'get_weather'; parameters = @{ type = 'object'; properties = @{ city = @{ type = 'string' } }; required = @('city') } })
   $r1 = Invoke-RestMethod -Method Post -Uri http://127.0.0.1:8080/v1/responses -Headers $h -ContentType application/json -Body (@{ model = 'gemini/gemini-3.5-flash'; input = 'Use get_weather for Paris.'; tools = $tools } | ConvertTo-Json -Depth 10)
   $call = $r1.output | Where-Object type -eq 'function_call' | Select-Object -First 1
   $call.call_id.StartsWith('spwts1_')
   $input = @(@{ type = 'message'; role = 'user'; content = 'Use get_weather for Paris.' }, @{ type = 'function_call'; call_id = $call.call_id; name = $call.name; arguments = $call.arguments }, @{ type = 'function_call_output'; call_id = $call.call_id; output = '{"temp":"15C"}' })
   $r2 = Invoke-RestMethod -Method Post -Uri http://127.0.0.1:8080/v1/responses -Headers $h -ContentType application/json -Body (@{ model = 'gemini/gemini-3.5-flash'; input = $input; tools = $tools } | ConvertTo-Json -Depth 10)
   $r2.status
   ```
   Expected: `True`, then `completed`; before this plan the second call failed with the `missing a thought_signature` 400.

4. Measure real signature sizes for open question 2 by calling Google directly with your own key.
   ```powershell
   $g = Invoke-RestMethod -Method Post -Uri https://generativelanguage.googleapis.com/v1beta/openai/chat/completions -Headers @{ Authorization = "Bearer $env:GEMINI_API_KEY" } -ContentType application/json -Body (@{ model = 'gemini-3.5-flash'; messages = @(@{ role = 'user'; content = 'Use get_weather for Paris.' }); tools = @(@{ type = 'function'; function = @{ name = 'get_weather'; parameters = @{ type = 'object'; properties = @{ city = @{ type = 'string' } } } } }) } | ConvertTo-Json -Depth 10)
   $g.choices[0].message.tool_calls[0].extra_content.google.thought_signature.Length
   ```
   Expected: a length well under 65536; record the value in the ADR.

5. Confirm the placeholder on a Gemini 2.5 model (open question 1): repeat step 3's second request directly to Google with `model = 'gemini-2.5-flash'` and a hand-built unsigned history whose first tool call has `extra_content = @{ google = @{ thought_signature = 'skip_thought_signature_validator' } }`.
   Expected: HTTP 200.

6. Codex tool loop with continuation.
   ```powershell
   .\bin\spillway.exe setup codex -config $cfg
   $dir = New-Item -ItemType Directory -Path (Join-Path $env:TEMP ('spw-codex-' + [guid]::NewGuid()))
   Set-Content -Path (Join-Path $dir 'NOTE.txt') -Value 'first line is spillway-note-ok'
   codex exec --profile spillway --model gemini/gemini-3.5-flash --skip-git-repo-check --ephemeral --color never -C $dir "List the files here with your shell tool, then read NOTE.txt with a second shell command, then reply with only its first line."
   ```
   Expected: exit 0, at least two tool calls in the transcript, final message `first line is spillway-note-ok`, and no `thought_signature` text.
   Repeat with `--model groq/openai/gpt-oss-120b`; expected: the same result, proving the unsigned path is unchanged.
   Groq's free tier allows 8,000 tokens per minute, below a Codex request, so on that tier expect a 413 and treat the step as covered by T10's fake-upstream tests.

7. Claude Code tool loop with continuation.
   ```powershell
   .\bin\spillway.exe setup claude -config $cfg
   Set-Location $dir
   claude -p --model gemini/gemini-3.5-flash --allowedTools "Bash(ls:*)" "Bash(cat:*)" Read "List the files here, then read NOTE.txt, then reply with only its first line."
   ```
   Expected: exit 0 and output `first line is spillway-note-ok`, with no `unknown field` or `thought_signature` error.
   Repeat with `--model groq/openai/gpt-oss-120b`; expected: the same result, subject to the same free-tier limit as step 6.

8. Inspect the gateway output from steps 3 to 7.
   Expected: no `status=400` lines for these requests, no signature text, no `user_id` value, and no `thought signature not forwarded` warning unless step 4 measured a signature above the bound.

9. Observe the chat path (open question 8): `opencode run -m spillway/gemini/gemini-3.5-flash "Run ls with your shell tool, then read NOTE.txt and reply with its first line."` from `$dir`.
   Expected: the loop completes; if it does, it is using either OpenCode's echoed signature or the placeholder.

10. Note whether Claude Code logged requests to `/v1/messages/count_tokens` (open question 9) and whether Codex sent `custom_tool_call` items; both are recorded, not fixed, in this plan.

## Resolved questions

All nine were resolved on 2026-10-27 with the recommended default: 1a, 2 (64 KiB), 3a, 4a, 5a, 6a, 7a, 8a, 9a.
The options are kept below for the record.

1. Should the Gemini adapter fill Google's placeholder signature on unsigned assistant tool calls?
   Options: (a) fill `skip_thought_signature_validator` on the first call of every unsigned assistant message for all Gemini models; (b) never fill, so Groq-to-Gemini spillover mid-turn, old sessions and chat clients that drop `extra_content` fail with a 400 that does not spill; (c) fill only for calls Spillway can prove were not Gemini-generated, which it cannot do statelessly.
   Recommended default: (a), because Google documents it for exactly this case and the alternative breaks spillover.

2. What bound applies to a thought signature, and do clients accept long tool-call IDs?
   Options: 16 KiB, 64 KiB, or no bound beyond the 10 MiB request limit.
   Recommended default: 64 KiB per signature, then confirm with the step 4 measurement and the live Codex and Claude Code runs; oversized signatures fall back to plain IDs and the placeholder.

3. Should `thinking: {"type":"enabled","budget_tokens":N}` be accepted?
   Options: (a) accept every valid thinking type as a hint and validate its shape; (b) keep rejecting `enabled` because Spillway cannot honour a thinking budget.
   Recommended default: (a), consistent with Responses `reasoning` hints and with clients that send `enabled` for some models; this replaces the current `TestAnthropicMessages_RejectsUnsupportedFieldsWithAnthropicError` case.

4. Should `output_config.effort` be translated to chat `reasoning_effort`?
   Options: (a) ignore it as a hint; (b) translate `low`, `medium` and `high`, and clamp `xhigh` and `max` to `high`.
   Recommended default: (a), because Groq models without reasoning answer 400 to `reasoning_effort` and 400 does not spill; revisit together with the existing Responses `reasoning.effort` translation, which has the same exposure.

5. How should images inside `tool_result` be handled?
   Options: (a) reject with a clear 400; (b) replace each image with a text marker in the tool message and append the images in a user message after all tool messages.
   Recommended default: (a) in this plan, with (b) as a follow-up if the live Claude Code run shows image reads are common.

6. Is a Gemini 3 signature valid on a different Gemini model in the same tier?
   Options: (a) forward the real signature regardless and accept a possible 400; (b) add the originating model to the envelope and substitute the placeholder when the target model differs.
   Recommended default: (a) now, test it manually by replaying a `gemini-3.5-flash` history to another Gemini 3 model, and move to (b) in a follow-up only if Google rejects it.

7. Should translated streams defer tool-call events until the call completes?
   Options: (a) defer until `finish_reason` or `[DONE]`; (b) emit on the first delta and read the signature only from that delta.
   Recommended default: (a), because Google advises reading until `finish_reason` and Anthropic cannot change a started block's ID.

8. Should `/v1/chat/completions` also use signed tool-call IDs?
   Options: (a) no, keep it verbatim with Google's `extra_content` and rely on the placeholder for clients that drop it; (b) yes, rewrite chat tool-call IDs too.
   Recommended default: (a), because rewriting IDs on the OpenAI-contract path would surprise clients that already echo `extra_content` and break ADR 0001's pass-through.

9. Should Spillway serve `POST /v1/messages/count_tokens` for Claude Code?
   Options: (a) leave it unrouted (404) as today; (b) return Spillway's conservative token estimate in Anthropic's shape.
   Recommended default: (a), unless step 10 shows that Claude Code degrades without it.

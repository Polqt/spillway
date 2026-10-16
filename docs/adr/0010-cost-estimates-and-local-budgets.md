# 10. Cost is estimated per attempt, and budgets are soft caps on paid models

Date: 2026-10-15
Status: accepted

## Context

v0.4 adds cost estimates and spending caps.
Spillway cannot read provider invoices, balances or plan state, and sees cost only after a response, from token counts and prices.
Providers report usage unevenly: non-streaming responses usually include it, streams include it only when the request sets `stream_options.include_usage`, and OpenRouter can report a billed cost.
Some providers may bill a request that failed after being partly processed, and Spillway cannot tell which failures were billed.
CONTEXT section 7 proposed `on_exceed: cheap_only | block`, but zero-spend tiers (ADR 0005) cost nothing, so blocking them protects no money.

## Decision

- A model reference is paid when the ADR 0005 rule does not make it zero-spend eligible. One exported function holds that rule for both config validation and budgets.
- Cost is computed per attempt, in integer micro-USD, and a request's cost is the sum of its attempts. For each attempt the first source that applies is used:
  free (zero-spend eligible, 0); provider-reported cost; the user's `pricing:` declaration in USD per million input and output tokens applied to reported or estimated tokens; otherwise unknown.
- A failed attempt on a paid model is never assumed to cost $0. It is priced from reported usage when the failure carries any, and otherwise charged its estimated prompt tokens with no completion tokens. A stream that fails after the commit point is also charged its estimated relayed completion tokens.
- Estimates err high: completion tokens from relayed bytes divided by 3, prompt tokens from twice the router's size estimate. Every record says whether usage was reported or estimated.
- When the store is on and a streaming client did not ask for usage, Spillway adds `stream_options.include_usage` upstream and does not relay the usage-only chunk. With the store off, bodies are forwarded unchanged.
- `budgets.daily_usd` and `budgets.monthly_usd` cap the estimated spend Spillway observes, over UTC calendar days and months. Budgets require the store, and the store's retention must cover the budget period.
- A reached budget makes paid models unavailable and never blocks free models. In tiers and `auto`, a blocked candidate is skipped like a cooling one, with the period reset as its resume time. A direct reference to a blocked paid model gets 429 `insufficient_quota` with `Retry-After`.
- Budgets fail closed: with a budget set, every paid model in an `allow_spend` tier must have a `pricing:` entry, and a direct reference to an unpriced paid model gets 400 `model_price_unknown`.
- The cap is soft: the check happens before a request and the charge after it, so concurrent in-flight requests can overshoot by their own cost.
- `on_exceed` is removed.

## Consequences

- Budgets never push a request from a free model to a paid one and never stop free traffic.
- Charging failed paid attempts by estimate can overcount requests a provider rejected before processing, and can undercount a failure that generated hidden output; both are stated as limitations.
- Cost figures are estimates everywhere they appear. Stale prices, cached-input discounts, reasoning, image or audio pricing, per-request fees, taxes and spend outside this process are not modelled or seen.
- Groq and Gemini traffic under a free-tier attestation is counted as free; if the project gains billing, Spillway still counts it as free.
- Changing the request body with the store off, assuming any failed paid attempt is free, blocking free models, hard caps with reservations, or another currency needs a new ADR.

# 22. Evaluation content retention boundary

Date: 2026-10-19
Status: proposed

## Context

The evaluation runner needs prompts and completions in memory to make requests and prepare blinded ratings.

The existing runner writes completion text and assignment data to evaluation artifacts.

Spillway's usage store already excludes prompt and response content under ADR 0009.

The v1.2 evaluation contract must preserve independent human ratings while preventing evaluation content and credentials from entering Spillway logs, reports, store rows, or dashboard APIs.

## Proposed decision

- Evaluation corpora are operator-managed and supplied from outside Spillway.
- Spillway computes a canonical digest and records only the corpus version, digest, prompt count, categories, and safe run settings.
- Prompt and response text stays in memory while requests run.
- Durable evaluation artifacts never contain completion packets, prompt copies, provider credentials, or local keys.
- An explicit operator-invoked review export may write temporary blinded prompt/response material only to an operator-selected temporary directory.
- The review export creates independently shuffled views for the two raters and keeps the blind-key mapping separate from both views.
- Temporary review material remains under operator control and must be removed after rating; durable reports retain only opaque trial IDs, ratings, and safe metadata.
- Persisted score data may contain opaque trial IDs, rater IDs, rubric scores, adjudicated severe-failure values, and notes that contain no prompt or response text.
- Evaluation metadata may be written to a user-selected private output directory.
- Dashboard evaluation summaries may be stored only when the existing opt-in `store:` setting is enabled.
- Dashboard APIs return metadata and aggregates only, require a dashboard key, are bounded, and set `Cache-Control: no-store`.
- Evaluation commands never print request prompts, response content, keys, authorization headers, or raw provider error bodies.

## Consequences

- Human raters need an operator-managed temporary review process.
- A corpus digest supports identity checks, while rerunning requires the operator to retain the matching corpus.
- Interrupted evaluations can resume from safe metadata without storing prior completions.
- Reports and aggregate dashboard data remain useful without retaining user content.
- Historical artifacts are not rewritten automatically; the retention rule applies to new evaluation runs.

## Rejected alternatives

- Persisting answer packets in the evaluation output directory would simplify rater handoff but violates the approved content-retention boundary.
- Sending prompts or completions to a hosted rating service would add an outbound destination and hosted dependency.
- Using an LLM judge would not satisfy the independent human-rating requirement and is out of scope.

## What needs a new ADR

Persisting prompt or response content in durable Spillway records, logs, reports, or dashboard APIs, adding a hosted rating service, or adding evaluation content to operational usage records requires a new ADR.

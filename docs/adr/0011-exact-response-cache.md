# 11. The exact response cache is memory only, per client app

Date: 2026-10-15
Status: accepted

## Context

v0.4 adds an exact-match cache so a repeated request can be answered without a provider call.
A cache necessarily holds responses, and the default must stay "no prompt or response storage".
Keeping cached responses in the SQLite store would put response content on disk and keep it after the process ends.
One Spillway instance can serve several client apps, each with its own local key.
ADR 0008 makes `x-spillway-route` a public contract on every chat response.

## Decision

- The cache is opt-in with `cache.exact: true` and lives only in process memory; it is empty after a restart.
- The key is SHA-256 over the client app name, a zero byte, and the raw request body bytes. Apps never share entries, and bodies that differ in any byte, including key order, miss.
- Only non-streaming 200 responses up to 1 MiB are stored, with their provider, model and route header values, which a hit replays unchanged.
- Streaming requests and requests with `x-spillway-cache: bypass` skip lookup and storage.
- Lookup happens after the body is decoded and planned, before any candidate is tried; plan errors are never cached.
- Entries expire after `cache.ttl` (1 s to 24 h) and are evicted least-recently-used once `cache.max_mb` is reached.
- When the cache is enabled every chat response carries `x-spillway-cache: hit`, `miss` or `bypass`; when it is disabled the header is absent.
- A hit is recorded in the store with its original token counts and a cost of 0.

## Consequences

- No response content reaches disk, and one app cannot read another app's answers or learn what another app sent.
- Identical requests get identical answers within the TTL regardless of `temperature`; that is what enabling the cache means, and the docs say so.
- A cached answer from a paid model can still be served after a budget is reached, because it costs nothing.
- Persisting the cache, sharing it across apps, caching streams, or semantic matching needs a new ADR.

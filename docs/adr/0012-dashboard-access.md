# 12. The dashboard is a static shell that reads data with a dashboard key

Date: 2026-10-15
Status: accepted

## Context

v0.4 embeds a dashboard in the binary (ADR 0004) and adds `/api/*` endpoints with usage, routing history, budget and cooldown data.
Loopback is not a trust boundary against the browser (ADR 0003), and a browser navigation cannot send a bearer header.
A cookie session would add a CSRF surface and session state.
Every local key identifies a client app; until now, every key could do the same things.
The dashboard shows model names that come from client requests, which are untrusted.

## Decision

- `GET /dashboard` serves a static page with no data. It is behind the Host check but not local-key auth.
- The page script asks the user for a local key, keeps it in `sessionStorage`, and calls `/api/*` with `Authorization: Bearer`. No cookie is set.
- `/api/*` requires a local key marked `dashboard: true` in `server.api_keys`; any other key gets 403. Responses use the OpenAI error shape and `Cache-Control: no-store`.
- The page is one HTML, one CSS and one JS file embedded in `apps/spillway/web`, with no external fonts, scripts or CDNs.
- It is served with `Content-Security-Policy: default-src 'self'; frame-ancestors 'none'; base-uri 'none'`, `X-Content-Type-Options: nosniff` and `Referrer-Policy: no-referrer`, contains no inline script, and writes all data with `textContent`.
- Every `/api/*` query is bounded: date range, page size and grouping come from fixed limits and closed sets.

## Consequences

- A web page the user visits cannot read usage data: it lacks a dashboard key, and DNS rebinding is stopped by the Host check.
- Client app keys cannot read other apps' usage unless the user marks them.
- The key in `sessionStorage` is readable by any script on the page, so the CSP and no-inline-script rule are part of the security boundary and are tested.
- Serving data without a key, switching to cookie sessions, or loading external assets needs a new ADR.

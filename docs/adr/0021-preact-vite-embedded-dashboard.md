# Preact and Vite for the embedded dashboard

Date: 2026-10-18
Status: accepted

## Context

The v1.0 dashboard is a small static HTML/CSS/JavaScript page embedded directly in the Go binary.
v1.1 adds onboarding, provider status, usage views and a streaming request playground, creating shared client state across several interactive flows.
The product must remain a single self-hosted binary, with no CDN or Node runtime required by users.
`go install` cannot run the Vite build, so a binary built without the `dashboardassets` build tag cannot embed the Preact assets.

## Decision

Use Preact for dashboard components and state, and Vite to build static production assets.
Pin the Node/pnpm build toolchain for development and CI, run the asset build before Go compilation, and embed its output into the existing binary.
At runtime the dashboard loads same-origin bundled assets only.
Preserve ADR 0012's dashboard-key authorization, `sessionStorage` handling, no-inline-script/CSP boundary, `no-store` API responses and safe rendering of untrusted data.

A binary built without the `dashboardassets` tag, such as one from `go install`, serves the checked-in fallback dashboard.
The fallback dashboard is a supported channel, not a legacy view.
It keeps content parity with the embedded dashboard for onboarding, provider status, the playground and route explanations.
It follows the same CSP and key rules and is covered by the same browser tests.

## Consequences

The release remains a single Go binary and users need no Node installation.
Development and CI gain a frontend toolchain, lockfile, deterministic asset build and browser-level tests.
Every dashboard feature ships in both the embedded and fallback dashboards, and browser tests run against both builds.
Vendored HTMX with Go templates was considered but would still require custom JavaScript for bearer-key handling and streaming requests while introducing a second rendering path for this UI.

## What needs a new ADR

Replacing Preact or Vite, or adding another frontend framework, requires a new ADR.
Loading dashboard assets from another origin or a CDN, or requiring Node at runtime, requires a new ADR.
Dropping the fallback dashboard, or letting its content or security rules diverge from the embedded dashboard, requires a new ADR.
Relaxing any ADR 0012 rule preserved above requires a new ADR.

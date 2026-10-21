# Cobra, Huh and Lip Gloss for CLI experience

Date: 2026-10-18
Status: accepted

## Context

The current CLI has a small standard-library dispatcher and a simple line-prompt init flow.
v1.1 needs command discovery, per-command help and a clearer interactive setup while preserving the stable v1.0 command names, flags, exit codes, secret handling and scriptable setup path.

## Decision

Use Cobra for command dispatch, help, command suggestions and shell completion, adapting the existing command functions behind the command tree and retaining `cli.Run` as the tested entry point.
Use Huh only for interactive `init`; preserve the existing explicit flags and `-yes` behavior for automation, and never prompt for provider key values.
Use Lip Gloss for restrained visual hierarchy in terminal help and status output, while keeping diagnostics plain when output is redirected or color is unavailable.

## Consequences

The CLI gains maintained dependencies and a command-tree adapter that must preserve the v1.0 compatibility contract.
Interactive and non-interactive init paths need separate coverage.
Styling must not change exit codes, diagnostic meaning, or machine-readable output.

## Considered options

Keeping the custom dispatcher avoids dependencies but leaves command discovery and completion behavior to maintain locally.
Adopting Cobra, Huh and Lip Gloss provides a consistent, accessible interactive path and common CLI navigation while retaining existing command implementations and automation flags.

## What needs a new ADR

Replacing Cobra, Huh or Lip Gloss, or adding another CLI framework, requires a new ADR.
Prompting for provider key values in any command requires a new ADR.
Using Huh prompts outside interactive `init` requires a new ADR.
Changing a v1.0 command name, flag or exit code is governed by ADR 0015 and is not permitted by this ADR.

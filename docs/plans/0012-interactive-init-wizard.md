# Plan 0012: Interactive Init Wizard

Status: complete; shipped in v1.0.0

## Goal

Replace the interactive `spillway init` questionnaire with a keyboard-first full-screen wizard while preserving the existing config contract and automation paths.

## Scope

- Use Bubble Tea v2 and Lip Gloss v2 for an alternate-screen setup flow.
- Keep Huh accessible prompts for non-TTY sessions and keep existing `-yes`, `-config`, `-providers`, `-attest-free-tier`, and `-store` behavior.
- Provide path, provider, free-tier eligibility, model, local history, review, and success screens.
- Search the local model suggestions declared by provider presets; do not call provider APIs or add a remote catalog.
- Preview the candidate config with generated local keys redacted and provider keys represented only by environment references.
- Validate that the selected config folder exists before showing the review screen, with a clear recovery hint when it does not.
- Write only after explicit save confirmation, retaining exclusive file creation and validation.

## Architecture

- Keep config construction, validation, environment status checks, and exclusive writes in the CLI/business layer.
- Keep wizard draft and step transitions separate from terminal rendering.
- Keep Bubble Tea state, keyboard handling, responsive layout, and view composition in a terminal UI layer.
- Bubble Tea v2 owns alternate-screen entry and terminal restoration.

## Compatibility and security

- `-yes` remains non-interactive and produces the same configuration semantics.
- Explicit flags continue to supply the values they supply today.
- Piped and dumb-terminal input keeps the accessible path.
- Provider credentials are never prompted for, read, displayed, or written as values.
- Generated local API keys remain hidden in preview and terminal output.
- Cancellation before save leaves no new config; existing paths are never overwritten.
- No network calls are made by `init`.

## Acceptance

- Interactive TTY users get a multi-select provider screen, grouped/searchable model selection, independent attestations, a redacted final preview, and an explicit save action.
- Back navigation preserves all selections and the current search query.
- Resizing updates the layout without losing draft state; narrow terminals collapse to a single column.
- The full-width layout shows the current draft summary beside the active step; compact layouts keep the same actions in a single pane.
- A missing parent folder is reported at the path step, before provider selection or save.
- Normal exit and cancellation restore the prior terminal screen.
- Non-TTY, `-yes`, and existing flag paths remain covered through the public CLI interface.
- Tests prove no file is written before confirmation, no overwrite occurs, and no secret appears in UI or errors.

## Deferred

- Remote provider catalog discovery, persistent wizard preferences, alternate themes, and interactive editing of arbitrary model IDs.
- New or expanded model catalogs beyond each provider's existing local preset suggestion.

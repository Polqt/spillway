# Init config overwrite and provider cleanup

Status: complete; shipped in v1.0.0 (#21); follow-up commits `ed91d7a` and `a50a100` are on `main`

## Goal

Let `spillway init` replace an existing config after an explicit review, while preserving a private backup.
Remove the shipped mock provider and its user-facing setup path.
Keep mock implementations only in test support.

## Requirements

- Existing regular config files can be selected in the init wizard and are replaced only after review.
- Replacement is atomic and the old bytes are saved to a timestamped, owner-only backup.
- Noninteractive replacement requires an explicit `--overwrite` flag.
- Init starts with no provider selected and offers real providers only.
- Multiple Gemini project aliases remain supported with environment-referenced API keys.
- Gemini aliases are direct-only and never used for same-service quota fallback.
- The shipped binary, examples, install guide, and CLI no longer expose the mock provider.
- Tests may use isolated fake providers and HTTP servers.

## Verification

- `make check`
- `CGO_ENABLED=0 go build ./...`
- Inspect the generated config, backup permissions, cancellation path, and provider list.

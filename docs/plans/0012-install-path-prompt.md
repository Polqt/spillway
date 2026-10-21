# Plan 0012: Confirmed CLI install-path replacement

## Goal

Let users upgrading from a release archive replace the existing Spillway executable found on `PATH` with the downloaded binary through one confirmed command.

## Scope

- Add `spillway install` to locate the first Spillway executable on `PATH`.
- Show the resolved destination and ask before replacing it.
- Copy the currently running binary atomically to the selected destination.
- Leave the `PATH` environment variable unchanged.
- Explain when no existing executable is on `PATH` or the binary is already at that path.
- Document the upgrade command and cover it through CLI tests.

## Acceptance criteria

- `spillway install` replaces the first executable found on `PATH` only after an explicit yes response.
- No, empty input, or unavailable input leaves the destination unchanged.
- Replacement preserves executable mode and does not expose configuration or provider keys.
- The command fails clearly when the current binary cannot be copied or the destination cannot be replaced.
- No match on `PATH` and an already-matching executable are handled without writes.
- `make check` passes.

## Constraints

- Use the standard library and the existing `findOnPath` helper.
- Do not edit persistent `PATH` settings or access the network.
- Keep existing manual archive installation instructions for first-time installs.

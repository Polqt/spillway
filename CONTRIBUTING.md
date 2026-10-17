# Contributing

Read [AGENTS.md](AGENTS.md) and [docs/CODING_STANDARDS.md](docs/CODING_STANDARDS.md) before making changes.

Run `make check` and confirm it passes before submitting a change.

Use imperative Conventional Commit messages, for example `fix(api): reject invalid requests`.

Place tests under `tests/`, mirroring the package they cover, as required by [ADR 0004](docs/adr/0004-tests-outside-packages.md).

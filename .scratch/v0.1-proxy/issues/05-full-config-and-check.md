# 05: Full config and `spillway check`

**What to build:** A user writes a real `config.yaml` with `${ENV}` references for their provider keys and local keys, runs `spillway check`, and either sees the config printed with every secret redacted (exit 0), or a precise error naming the field, the line and the variable, never a value (exit 1).
Logging anywhere in the program scrubs known secret values even if one slips into a message.
Design: plan "internal/config", "internal/redact" (Scrubber, slog handler), "internal/cli and apps/spillway", decisions 2 and 3; ADR 0003.

**Blocked by:** 02: Mock chat end to end.

**Status:** ready-for-agent

- [x] `${VAR}` expands from the environment; an unset or empty variable is an error naming the variable and YAML path, never a value.
- [x] Unknown fields are errors; `tiers`, `router`, `budgets` and `cache` get a message naming the milestone that adds them.
- [x] Validation covers: listen address and port, at least one local key on every listen address, local key names unique and `[a-z0-9-]`, known provider names, `spillway` and `auto` reserved, base URLs without userinfo, plain `http` only to loopback hosts, log level and timeouts.
- [x] `spillway check` prints valid YAML with `[REDACTED]` in place of every provider key and local key.
- [x] `spillway check` failures exit 1 with `spillway: ` on stderr and nothing on stdout.
- [x] The scrubbing slog handler removes every configured secret from log messages and attributes; a test proves it.
- [x] `make check` passes.

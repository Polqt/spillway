# Historical config fixtures with an environment-bound store

Each YAML file is copied byte for byte from the listed release commit.
Their `store.path` directory exists only where the config is meant to run, such as `/var/lib/spillway` in the container image or `.scratch/` during a local evaluation run.
`TestParse_HistoricalConfigsWithEnvironmentStoreStillLoad` replaces only `store.path` with a temporary file before parsing; every other field loads unchanged.

## v1.0

Source commit: `9c7f9f5` (tag `v1.0.0`).

- `v1.0/config.docker.yaml` from `examples/config.docker.yaml`
- `v1.0/config.eval.live.auto.yaml` from `examples/config.eval.live.auto.yaml`
- `v1.0/config.eval.live.classifier.yaml` from `examples/config.eval.live.classifier.yaml`
- `v1.0/config.eval.live.rules.yaml` from `examples/config.eval.live.rules.yaml`

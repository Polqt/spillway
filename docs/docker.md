# Docker

Spillway's image is distroless and runs as uid 65532. It has no shell or package manager. The server listens on `0.0.0.0:8080` inside the container, so publish it only on host loopback unless you have deliberately designed and secured another network boundary.

## Run with persistent storage

Create a private config file based on `examples/config.docker.yaml`. Replace its demo local key with a strong random local key before starting the container. Keep provider keys in the container environment and reference them as `${VARIABLE}` in the config; do not put provider key values in YAML.

For example, after setting `config.docker.yaml` and its local key, and choosing a provider reachable from the container:

```sh
docker volume create spillway-data
docker run --rm --read-only --stop-timeout 10 \
  -p 127.0.0.1:8080:8080 \
  -v "$(pwd)/config.docker.yaml:/etc/spillway/config.yaml:ro" \
  -v spillway-data:/var/lib/spillway \
  ghcr.io/polqt/spillway:1.1.1
```

The example config stores the optional SQLite database under `/var/lib/spillway`. The named volume keeps usage history across container replacement. If store and budgets are not enabled, the data volume is not needed. Mount the config read-only and keep the root filesystem read-only. The default graceful shutdown period is 10 seconds; Docker's stop timeout should be at least as long as `server.shutdown_timeout` if you change that setting.

The published port is bound to `127.0.0.1` on the host. A local API key is required even though the container listens on all interfaces. Container requests do not get the loopback-only `Host` header protection used by a loopback host process; keep the published port private and require local keys.

If the config uses a provider key, provide the referenced variable to the container with Docker's `--env-file` or `-e` option. Keep that environment file private and out of source control.

## Ollama limitation

The provider URL validator permits plaintext HTTP only for loopback addresses. In Docker, `localhost` refers to the container. The supported Docker note is Linux `--network host` with Ollama on the host, where the provider can use `http://localhost:11434`. Do not point the container at a non-loopback plaintext Ollama URL. The host-network approach is not supported on macOS or Windows, so Docker Spillway cannot use host Ollama there in v1.0.

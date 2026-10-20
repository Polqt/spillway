# 16. Release artifacts, images and supply chain

Date: 2026-10-16
Status: accepted

## Context

v1.0 must ship as downloadable binaries and a container image that users can verify, while keeping the single-binary, no-cgo rule.
CONTEXT section 4 names GoReleaser and a distroless image.
Dev tools are pinned in `tools/go.mod` so they never change the binary's module graph (ADR 0002).
The repository is hosted on GitHub, so GitHub Actions, GitHub Releases, GHCR and GitHub artifact attestations are available without another service.
Inside a container Spillway cannot listen on loopback, which turns off the loopback Host check (ADR 0003).
Provider `base_url` values must use https unless the host is loopback, so a containerised Spillway cannot reach Ollama on the host or in another container over plain http.

## Decision

- Releases are built by GoReleaser v2.18.2, pinned as a `tool` in `tools/go.mod` and run through Makefile targets.
- Targets are linux, darwin and windows on amd64 and arm64.
  windows/arm64 is built but not run-tested, and the docs say so.
- Every build uses `CGO_ENABLED=0`, `-trimpath`, `-ldflags "-s -w -X github.com/Polqt/spillway/internal/cli.version=v{{ .Version }}"` and the commit timestamp as file modification time, so two builds of one commit with one Go version are byte-identical.
  The release Go version is one exact patch release pinned in the release workflow and named in the release notes.
- Commit, build time and dirty state come from Go build info, not extra ldflags, so `go install ...@vX.Y.Z` reports the right version too.
- Archives are `tar.gz`, or `zip` on Windows, and contain the binary, `LICENSE`, `README.md` and `examples/config.mock.yaml`.
  `checksums.txt` lists the sha256 of every archive.
- GitHub build provenance attestations cover every archive and the image.
  cosign signatures and SBOMs are not produced in v1.0.
- Release notes are generated from Conventional Commits; there is no hand-edited CHANGELOG.md.
- The image is built from `apps/spillway/Dockerfile` on `gcr.io/distroless/static-debian12:nonroot`, pinned by a digest at least two weeks old, for linux/amd64 and linux/arm64.
  It runs as uid 65532 with `ENTRYPOINT ["/spillway"]` and `CMD ["serve","-config","/etc/spillway/config.yaml"]`, exposes 8080, keeps the store under `/var/lib/spillway`, and works with a read-only root filesystem.
- Images are pushed to `ghcr.io/polqt/spillway` with tags `X.Y.Z`, `X.Y` and `X`.
  No `latest` tag is published, so users pin a version.
- In a container Spillway listens on `0.0.0.0:8080`.
  The docs and `examples/config.docker.yaml` publish the port only to host loopback (`-p 127.0.0.1:8080:8080`) and rely on the mandatory local key.
  v1.0 adds no `server.allowed_hosts` option and no plaintext non-loopback Ollama; Ollama with Docker is documented only for Linux `--network host`.
- CI on pull requests runs `make check` with the race detector forced on ubuntu, macos and windows, on the oldest supported Go and the release Go version, plus `make vuln`, a GoReleaser snapshot reproducibility check and an image smoke test.
  Workflows pin actions by commit SHA and request only the permissions each job needs.
- Releases run from `v*` tags.
  On the v1.1 release line, pushing a version tag from `release/v1.1` starts publication.
  The manual `workflow_dispatch` path is restricted to `main` and is only for publishing an existing release candidate.
  A release candidate tag `vX.Y.Z-rc.N` is published and verified on Windows, Linux, macOS and Docker before the final tag.

## Consequences

- Users can verify downloads with `sha256sum -c checksums.txt` and `gh attestation verify`, without trusting a separate signing service.
- Reproducible builds let anyone with the same Go version check a release binary against the source.
- The image has no shell or package manager, so debugging happens through logs and `/healthz`, and Docker health checks are left to the orchestrator.
- Container users lose the loopback Host check; the mandatory local key remains the defence, and the docs say so.
- Ollama-backed tiers and the classifier are not usable from Docker on macOS or Windows in v1.0.
- Adding `latest`, cosign, SBOMs, package-manager channels, a Host allowlist for non-loopback listeners, or plaintext non-loopback provider URLs needs a new ADR.

## Rejected alternative

Using an unpinned release toolchain or publishing unverified binaries would make release artifacts harder to reproduce and verify.
Publishing a `latest` image tag or expanding Docker networking in v1.0 would make version selection or the current local-network security boundary less explicit.

## What needs a new ADR

Adding a `latest` tag, cosign signatures, SBOMs, package-manager distribution, a non-loopback Host allowlist, plaintext non-loopback provider URLs, or changing the release targets and verification guarantees requires a new ADR.

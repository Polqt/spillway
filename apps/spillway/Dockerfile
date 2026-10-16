# This multi-arch digest has been pinned in upstream source since at least June 2026.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:d093aa3e30dbadd3efe1310db061a14da60299baff8450a17fe0ccc514a16639
ARG TARGETPLATFORM
COPY --chown=65532:65532 ${TARGETPLATFORM}/spillway /spillway
COPY --chown=65532:65532 examples/config.docker.yaml /etc/spillway/config.yaml
COPY --chown=65532:65532 apps/spillway/volume/.keep /var/lib/spillway/.keep
USER 65532:65532
EXPOSE 8080
VOLUME ["/var/lib/spillway"]
ENTRYPOINT ["/spillway"]
CMD ["serve", "-config", "/etc/spillway/config.yaml"]

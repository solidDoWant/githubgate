# The server image. `make build-images` cross-compiles the binary for each
# platform into build/images/<arch>/ first, so the image needs no Go toolchain.
# distroless static brings the CA certificates for TLS to GitHub and runs as
# the nonroot user (65532).
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab

ARG TARGETARCH
# Links the ghcr.io package to this repository (its visibility and README).
LABEL org.opencontainers.image.source="https://github.com/solidDoWant/githubgate"
COPY build/images/${TARGETARCH}/githubgate /githubgate
USER 65532:65532
ENTRYPOINT ["/githubgate"]

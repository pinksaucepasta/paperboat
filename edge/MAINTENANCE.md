# Maintenance

Paperboat Tunnel is a native Go edge. Build and publish only `paperboat-edge`
(`cmd/paperboat-edge`).

## Dependencies

`quic-go` and `realclientip-go` are pinned in `go.mod`, and `tools/verify-dependencies.sh`
(part of `make check`) fails when the module graph differs from the pin. To upgrade either
one, read its release notes for handshake, datagram and address-parsing changes, change the
pin in `go.mod` and in that script together, and re-run the focused suites:

```sh
go test -race ./internal/datacarrier ./internal/control ./internal/edgehttp ./internal/runtime ./cmd/paperboat-edge
```

## Toolchain

`GO_VERSION` in `Makefile`, the `GO_VERSION` build argument in `deploy/Dockerfile`, `go.mod`
and `.go-version` carry the same Go release. `make build` refuses a different toolchain.
Change all four together.

## Before a release

`make check` must pass, then verify against a running edge:

- TLS serving over HTTP/1.1, HTTP/2 and HTTP/3, and the HTTP to HTTPS redirect.
- Certificate selection for preview, tunnel and custom hostnames.
- Connector admission, and private-access authorization with a fresh grant.
- Drain and graceful shutdown, including a replacement process taking over a serving one.

`make release-check` additionally requires a clean worktree and a digest-pinned runtime image.
`make metrics-generate` regenerates `docs/metrics.json` when a metric changes.

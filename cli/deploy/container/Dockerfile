ARG PAPERBOAT_VERSION

FROM golang:1.27.4-bookworm@sha256:4fa7c968ba93f1635670522dbb01a2c78150443e1385746924566079f36570ea AS build
ARG PAPERBOAT_VERSION

# Build context is the repository root (docker build -f cli/deploy/container/Dockerfile .). The cli builds in
# the Go workspace with the relay and shared modules it imports; the workspace here lists only those three.
WORKDIR /src
COPY cli/go.mod cli/go.sum cli/
COPY relay/go.mod relay/go.sum relay/
COPY shared/go.mod shared/go.sum shared/
RUN printf 'go 1.27.4\n\nuse (\n\t./cli\n\t./relay\n\t./shared\n)\n' > go.work \
  && cd cli && go mod download
COPY cli cli
COPY relay relay
COPY shared shared
WORKDIR /src/cli
RUN printf '%s\n' "${PAPERBOAT_VERSION}" | grep -Eq '^[0-9]{4}\.[0-9]{2}\.[0-9]{2}\.(0|[1-9][0-9]*)$' \
  && CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X github.com/pinksaucepasta/paperboat/cli/internal/buildinfo.Version=${PAPERBOAT_VERSION} -X github.com/pinksaucepasta/paperboat/cli/internal/buildinfo.ProtocolVersion=1" \
    -o /out/pb ./cmd/pb

FROM debian:bookworm-slim@sha256:7b140f374b289a7c2befc338f42ebe6441b7ea838a042bbd5acbfca6ec875818
ARG PAPERBOAT_VERSION

LABEL org.opencontainers.image.version="${PAPERBOAT_VERSION}"

RUN apt-get update \
  && apt-get install -y --no-install-recommends ca-certificates git tini \
  && rm -rf /var/lib/apt/lists/* \
  && groupadd --gid 10001 paperboat \
  && useradd --uid 10001 --gid 10001 --create-home --shell /bin/bash paperboat

COPY --from=build /out/pb /usr/local/bin/pb

# One supervisor in Go (pb daemon --foreground): it checks the state tree, enrolls on the first start from
# PAPERBOAT_ENROLLMENT_CREDENTIAL, starts the session owner and runs hostd as the paperboat user. tini only reaps orphaned processes.
WORKDIR /workspace
ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/pb", "daemon", "--foreground"]

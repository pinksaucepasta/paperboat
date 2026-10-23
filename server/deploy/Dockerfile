ARG PAPERBOAT_VERSION
ARG PAPERBOAT_COMMIT
ARG PAPERBOAT_SENTRY_DSN

FROM --platform=$BUILDPLATFORM golang:1.27.2-bookworm@sha256:5cf287a799e6b94384bad13d16b14904c531f51ba65792237e122ce42b392f61 AS build
ARG PAPERBOAT_VERSION
ARG PAPERBOAT_COMMIT
ARG PAPERBOAT_SENTRY_DSN
ARG TARGETOS
ARG TARGETARCH

WORKDIR /src/server

COPY server/go.mod server/go.sum ./
COPY shared /src/shared
RUN GOWORK=off go mod download

COPY server .
RUN printf '%s\n' "${PAPERBOAT_VERSION}" | grep -Eq '^[0-9]{4}\.[0-9]{2}\.[0-9]{2}\.(0|[1-9][0-9]*)$' \
  && printf '%s\n' "${PAPERBOAT_COMMIT}" | grep -Eq '^[0-9a-f]{40}$' \
  && case "${PAPERBOAT_SENTRY_DSN}" in "") ;; *[!A-Za-z0-9:/@.-]*) exit 1 ;; https://?*) ;; *) exit 1 ;; esac \
  && { test -z "${PAPERBOAT_SENTRY_DSN}" || printf '%s\n' "${PAPERBOAT_SENTRY_DSN}" | grep -Eq '^https://[A-Za-z0-9]+@[A-Za-z0-9.-]+/[0-9]+$'; } \
  && sentry_release= \
  && if test -n "${PAPERBOAT_SENTRY_DSN}"; then sentry_release="paperboat-server:${PAPERBOAT_VERSION}"; fi \
  && CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath \
    -ldflags "-s -w -X github.com/pinksaucepasta/paperboat/server/internal/buildinfo.Version=${PAPERBOAT_VERSION} -X github.com/pinksaucepasta/paperboat/server/internal/buildinfo.Commit=${PAPERBOAT_COMMIT} -X github.com/pinksaucepasta/paperboat/server/internal/config.DefaultSentryDSN=${PAPERBOAT_SENTRY_DSN} -X github.com/pinksaucepasta/paperboat/server/internal/config.DefaultSentryRelease=${sentry_release}" \
    -o /out/paperboat-server ./cmd/paperboat-server

FROM debian:bookworm-slim@sha256:7b140f374b289a7c2befc338f42ebe6441b7ea838a042bbd5acbfca6ec875818 AS runtime

ARG PAPERBOAT_VERSION
LABEL org.opencontainers.image.version="${PAPERBOAT_VERSION}"

RUN apt-get update \
  && apt-get install -y --no-install-recommends ca-certificates curl tini \
  && rm -rf /var/lib/apt/lists/*

WORKDIR /app

COPY --from=build /out/paperboat-server /usr/local/bin/paperboat-server

EXPOSE 8080

ENTRYPOINT ["/usr/bin/tini", "--"]
CMD ["/usr/local/bin/paperboat-server", "serve"]

ARG GO_VERSION=1.27.1
ARG PAPERBOAT_VERSION
ARG PAPERBOAT_SENTRY_DSN

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-bookworm@sha256:648f440f42a0958804efb24df176f806f9d353b41f1c0627f666428e40310f6b AS go-build
ARG PAPERBOAT_VERSION
ARG PAPERBOAT_SENTRY_DSN
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY . .
WORKDIR /src/tunnel
RUN printf '%s\n' "${PAPERBOAT_VERSION}" | grep -Eq '^[0-9]{4}\.[0-9]{2}\.[0-9]{2}\.(0|[1-9][0-9]*)$' \
  && case "${PAPERBOAT_SENTRY_DSN}" in '') ;; *[!A-Za-z0-9:/@.-]*) exit 1 ;; esac \
  && { test -z "${PAPERBOAT_SENTRY_DSN}" || printf '%s\n' "${PAPERBOAT_SENTRY_DSN}" | grep -Eq '^https://[A-Za-z0-9]+@[A-Za-z0-9.-]+(:[0-9]+)?/[A-Za-z0-9/.-]+$'; } \
  && sentry_release= \
  && if test -n "${PAPERBOAT_SENTRY_DSN}"; then sentry_release="paperboat-tunnel:${PAPERBOAT_VERSION}"; fi \
  && CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -p=2 -trimpath -ldflags "-s -w -X main.version=${PAPERBOAT_VERSION} -X github.com/pinksaucepasta/paperboat-tunnel/internal/reporting.DefaultDSN=${PAPERBOAT_SENTRY_DSN} -X github.com/pinksaucepasta/paperboat-tunnel/internal/reporting.DefaultRelease=${sentry_release}" -o /out/paperboat-tunnel ./cmd/paperboat-tunnel

FROM debian:bookworm-slim@sha256:7b140f374b289a7c2befc338f42ebe6441b7ea838a042bbd5acbfca6ec875818 AS runtime
ARG PAPERBOAT_VERSION
LABEL org.opencontainers.image.version="${PAPERBOAT_VERSION}"
RUN apt-get update \
  && apt-get install -y --no-install-recommends ca-certificates curl tini \
  && rm -rf /var/lib/apt/lists/* \
  && groupadd --gid 10001 paperboat-tunnel \
  && useradd --uid 10001 --gid 10001 --home /var/lib/paperboat-tunnel --create-home paperboat-tunnel
COPY --from=go-build /out/paperboat-tunnel /usr/local/bin/paperboat-tunnel
RUN install -d -o paperboat-tunnel -g paperboat-tunnel /var/lib/paperboat-tunnel/runtime /var/lib/paperboat-tunnel/state
USER paperboat-tunnel
ENTRYPOINT ["/usr/bin/tini", "--", "paperboat-tunnel"]
CMD ["-deployment-config", "/etc/paperboat-tunnel/deployment.json", "-state-path", "/var/lib/paperboat-tunnel/state/state.json"]

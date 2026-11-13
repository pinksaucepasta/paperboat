ARG GO_VERSION=1.27.4
ARG PAPERBOAT_VERSION
ARG PAPERBOAT_COMMIT
ARG PAPERBOAT_SENTRY_DSN

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-bookworm@sha256:4fa7c968ba93f1635670522dbb01a2c78150443e1385746924566079f36570ea AS go-build
ARG PAPERBOAT_VERSION
ARG PAPERBOAT_SENTRY_DSN
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY . .
WORKDIR /src/edge
RUN printf '%s\n' "${PAPERBOAT_VERSION}" | grep -Eq '^[0-9]{4}\.[0-9]{2}\.[0-9]{2}\.(0|[1-9][0-9]*)$' \
  && case "${PAPERBOAT_SENTRY_DSN}" in '') ;; *[!A-Za-z0-9:/@.-]*) exit 1 ;; esac \
  && { test -z "${PAPERBOAT_SENTRY_DSN}" || printf '%s\n' "${PAPERBOAT_SENTRY_DSN}" | grep -Eq '^https://[A-Za-z0-9]+@[A-Za-z0-9.-]+(:[0-9]+)?/[A-Za-z0-9/.-]+$'; } \
  && sentry_release= \
  && if test -n "${PAPERBOAT_SENTRY_DSN}"; then sentry_release="paperboat-edge:${PAPERBOAT_VERSION}"; fi \
  && CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -p=2 -trimpath -ldflags "-s -w -X main.version=${PAPERBOAT_VERSION} -X github.com/pinksaucepasta/paperboat/shared/reporting.DefaultDSN=${PAPERBOAT_SENTRY_DSN} -X github.com/pinksaucepasta/paperboat/shared/reporting.DefaultRelease=${sentry_release}" -o /out/paperboat-edge ./cmd/paperboat-edge \
  && { test -z "${PAPERBOAT_SENTRY_DSN}" || grep -qF "${PAPERBOAT_SENTRY_DSN}" /out/paperboat-edge; }

FROM debian:bookworm-slim@sha256:7b140f374b289a7c2befc338f42ebe6441b7ea838a042bbd5acbfca6ec875818 AS runtime
ARG PAPERBOAT_VERSION
ARG PAPERBOAT_COMMIT
# The deployed commit is checkable on the image itself (OCI image-spec annotation, as cloudflared and
# Teleport images carry it); a build without the full commit is refused.
RUN printf '%s\n' "${PAPERBOAT_COMMIT}" | grep -Eq '^[0-9a-f]{40}$'
LABEL org.opencontainers.image.version="${PAPERBOAT_VERSION}" \
      org.opencontainers.image.revision="${PAPERBOAT_COMMIT}"
RUN apt-get update \
  && apt-get install -y --no-install-recommends ca-certificates curl tini \
  && rm -rf /var/lib/apt/lists/* \
  && groupadd --gid 10001 paperboat-edge \
  && useradd --uid 10001 --gid 10001 --home /var/lib/paperboat-edge --create-home paperboat-edge
COPY --from=go-build /out/paperboat-edge /usr/local/bin/paperboat-edge
RUN install -d -o paperboat-edge -g paperboat-edge /var/lib/paperboat-edge/runtime /var/lib/paperboat-edge/state
USER paperboat-edge
ENTRYPOINT ["/usr/bin/tini", "--", "paperboat-edge"]
CMD ["-deployment-config", "/etc/paperboat-edge/deployment.json", "-state-path", "/var/lib/paperboat-edge/state/state.json"]

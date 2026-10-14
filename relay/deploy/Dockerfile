ARG PAPERBOAT_VERSION
ARG PAPERBOAT_SENTRY_DSN

FROM --platform=$BUILDPLATFORM golang:1.27.1-bookworm@sha256:648f440f42a0958804efb24df176f806f9d353b41f1c0627f666428e40310f6b AS build
ARG PAPERBOAT_VERSION
ARG PAPERBOAT_SENTRY_DSN
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY . .
WORKDIR /src/relay
RUN printf '%s\n' "${PAPERBOAT_VERSION}" | grep -Eq '^[0-9]{4}\.[0-9]{2}\.[0-9]{2}\.(0|[1-9][0-9]*)$' \
  && case "${PAPERBOAT_SENTRY_DSN}" in '') ;; *[!A-Za-z0-9:/@.-]*) exit 1 ;; esac \
  && { test -z "${PAPERBOAT_SENTRY_DSN}" || printf '%s\n' "${PAPERBOAT_SENTRY_DSN}" | grep -Eq '^https://[A-Za-z0-9]+@[A-Za-z0-9.-]+(:[0-9]+)?/[A-Za-z0-9/.-]+$'; } \
  && sentry_release= \
  && if test -n "${PAPERBOAT_SENTRY_DSN}"; then sentry_release="paperboat-relay:${PAPERBOAT_VERSION}"; fi \
  && CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags "-s -w -X main.version=${PAPERBOAT_VERSION} -X github.com/pinksaucepasta/paperboat-relay/internal/reporting.DefaultDSN=${PAPERBOAT_SENTRY_DSN} -X github.com/pinksaucepasta/paperboat-relay/internal/reporting.DefaultRelease=${sentry_release}" -o /out/paperboat-relay ./cmd/paperboat-relay \
  && cp "$(go list -m -f '{{.Dir}}' tailscale.com)/LICENSE" /out/tailscale.LICENSE

FROM debian:bookworm-slim@sha256:7b140f374b289a7c2befc338f42ebe6441b7ea838a042bbd5acbfca6ec875818 AS runtime
ARG PAPERBOAT_VERSION
LABEL org.opencontainers.image.version="${PAPERBOAT_VERSION}"
RUN apt-get update \
  && apt-get install -y --no-install-recommends ca-certificates tini \
  && rm -rf /var/lib/apt/lists/* \
  && groupadd --gid 10001 paperboat-relay \
  && useradd --uid 10001 --gid 10001 --home /var/lib/paperboat-relay --create-home paperboat-relay
COPY --from=build /out/paperboat-relay /usr/local/bin/paperboat-relay
COPY --from=build /out/tailscale.LICENSE /usr/share/licenses/paperboat-relay/tailscale.LICENSE
RUN install -d -o paperboat-relay -g paperboat-relay /var/lib/paperboat-relay/state
USER paperboat-relay
EXPOSE 443/tcp 443/udp
ENTRYPOINT ["/usr/bin/tini", "--", "paperboat-relay"]

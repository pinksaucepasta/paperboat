FROM --platform=$BUILDPLATFORM golang:1.27.2-bookworm@sha256:5cf287a799e6b94384bad13d16b14904c531f51ba65792237e122ce42b392f61 AS build
ARG PAPERBOAT_DIAGNOSTICS_MODULE
ARG PAPERBOAT_DIAGNOSTICS_COMMIT
ARG TARGETOS
ARG TARGETARCH
RUN test -n "$PAPERBOAT_DIAGNOSTICS_MODULE" \
 && printf '%s\n' "$PAPERBOAT_DIAGNOSTICS_COMMIT" | grep -Eq '^[a-f0-9]{40}$' \
 && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH GOBIN=/out go install -p 2 "$PAPERBOAT_DIAGNOSTICS_MODULE@$PAPERBOAT_DIAGNOSTICS_COMMIT"
RUN cp "$(go list -m -f '{{.Dir}}' "$PAPERBOAT_DIAGNOSTICS_MODULE@$PAPERBOAT_DIAGNOSTICS_COMMIT")/LICENSE" /out/LICENSE
FROM debian:bookworm-slim@sha256:7b140f374b289a7c2befc338f42ebe6441b7ea838a042bbd5acbfca6ec875818
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates tini && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/minio /usr/local/bin/minio
COPY --from=build /out/LICENSE /usr/share/doc/paperboat-diagnostics/LICENSE
COPY diagnostics-entrypoint.sh /usr/local/bin/diagnostics-entrypoint
ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/diagnostics-entrypoint"]
CMD ["server", "/data"]

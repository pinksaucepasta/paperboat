# Operations

Monitor native TLS ingress separately for HTTP/1.1, HTTP/2, and HTTP/3, plus
connector admission, private-access authorization, route generation, certificate
readiness, and drain state. Alert on sustained request failure, authorization anomalies,
certificate expiry risk, or a node that remains registered but not ready.

Sentry failure reporting is disabled by default. Enable it only with all three runtime
variables: `PAPERBOAT_SENTRY_ENABLED=true`, `PAPERBOAT_SENTRY_DSN`, and
`PAPERBOAT_SENTRY_RELEASE`. Logs and metrics default on while sampled traces default to
`0.1`; independently configure `PAPERBOAT_SENTRY_LOGS_ENABLED`,
`PAPERBOAT_SENTRY_METRICS_ENABLED`, and `PAPERBOAT_SENTRY_TRACES_SAMPLE_RATE` (0 through
1). Hosted deployments set both `PAPERBOAT_SENTRY_REGION` (a public lowercase region)
and `PAPERBOAT_SENTRY_INSTANCE` (`slot-01` through `slot-64`); invalid or partial pairs
stop startup. `PAPERBOAT_SENTRY_ENVIRONMENT` defaults to `production`. All emitted operations,
outcomes, codes, log bodies, and metric names are fixed. Reports exclude raw errors,
requests, tunneled traffic, headers, URLs, resource identifiers, file paths, locals, and
command arguments. No baggage or customer-traffic trace propagation occurs. A process
emits at most 8 failure events, 128 operation logs, 128 traces, and 64 detailed operation
metric points per minute. Every 15-second cumulative export independently emits a rotating
window of up to 64 series. Shutdown waits at most two seconds to drain; that drain does not
confirm server receipt.
The SDK counter `paperboat.operation.count` and distribution
`paperboat.operation.duration` cover fixed control, connector, usage, and lifecycle
outcomes. Every 15 seconds the process projects the existing fixed `paperboat_edge_*`
metrics: counters use `_snapshot`, histograms use `_sum_snapshot` and `_count_snapshot`,
and gauges keep their authoritative name. Existing fixed labels are preserved; resource
IDs and support references are never metric labels.
The cumulative drop gauges reveal local admission loss, and
`paperboat_reporting_signal_enabled` exposes the configured state of each signal. Fleet
views group by validated region and stable instance slot rather than node IDs or hostnames.

Recovery keeps authorization fail-closed: remove an unhealthy node from selection,
drain admitted work, restart the native edge, and verify a fresh generation before
returning it to service. Never bypass certificate, account, route, or connector checks.

Gateway deployments may set a positive `max_body_bytes` and set `max_header_bytes`
from 1 KiB through 128 KiB. Defaults are 50 MiB and 32 KiB.
Choose the smallest values that admit the published applications. The 4,096-entry
admission and route registries, 4,096-report/64 MiB persisted usage queue, and 250 ms
quota-feedback interval are fixed workload and recovery budgets rather than deployment
settings.

TLS/SNI pass-through shares public TCP 443 with edge-terminated HTTPS. One listener
inspects the initial ClientHello and selects the authoritative hostname's mode;
clients use the standard TLS/HTTPS port without an allocated port. The shared
binding is `listener_tls_443:443`; ordinary raw TCP keeps its allocated listeners.
A canonical hostname can have only one mode, including overlapping HTTP wildcards.
Remove the existing HTTP publication before publishing that hostname as TLS.
Migration rejects conflicting old publications until their owner resolves them,
then converts TLS routes to 443 and removes the old port reservation.
Clients must initiate TLS and send an exact visible SNI hostname; raw TCP,
STARTTLS, missing SNI and hidden hostname selection are unsupported. UDP443/HTTP3
continues to serve HTTPS; opaque UDP/QUIC pass-through is not supported.

The origin supplies its certificate and terminates TLS. Paperboat forwards the
inspected bytes unchanged and cannot insert HTTP login/cookie authorization or
inspect opaque application payloads. The owner explicitly publishes the route;
application authentication (including origin mTLS, if desired) belongs to the
origin. Custom aliases require verified exact domain ownership; wildcards are
unsupported for pass-through. No edge certificate is issued for these routes.

ClientHello inspection uses Go's maintained TLS parser, capped at 64 KiB of input
and five seconds within the existing 4096-connection default admission bound.
Unknown/revoked names, malformed/oversized handshakes and excess admissions close
without opening a connector stream. Current server authority is checked before
forwarding and refreshed using the existing five-second refresh/ten-second expiry
policy. Removing one hostname does not remove another route's listener. Regional
recovery accepts fresh connections; interrupted streams are closed, never replayed.

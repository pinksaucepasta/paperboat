# Runbooks

Every command below runs on the edge host. The health listener defaults to
`127.0.0.1:9090` (`--health-address`).

## Is the edge healthy?

```sh
curl -s http://127.0.0.1:9090/readyz     # 503 until the node is ready
curl -s http://127.0.0.1:9090/diagnostics
curl -s http://127.0.0.1:9090/metrics
```

`/readyz` reports each dependency (control, routes, usage, carrier). A node that stays
registered but not ready is removed from selection until a fresh generation reports ready.

## Native ingress regression

Check each layer on its own: certificate selection, route generation, connector session
and upstream target. HTTP/3 failing is not successful ingress because HTTP/2 works, so
test each protocol (`curl --http1.1`, `--http2`, `--http3-only`) against the same hostname.

Series to compare, from `docs/metrics.json`:

- `paperboat_edge_route_errors_total` by `reason` and `route_kind`.
- `paperboat_edge_origin_connect_failures_total` by `reason`.
- `paperboat_edge_certificate_failures_total` and `paperboat_edge_certificate_expiry_horizon_seconds`.
- `paperboat_edge_connector_sessions` by `state`.

Change nothing in configuration until you know which layer fails.

## Connector or private-access failure

1. Confirm the node's enrollment and credential files named in the deployment document
   are present and current.
2. Check `paperboat_edge_connector_disconnects_total` by `reason` (`auth`, `stale`,
   `timeout`) and `paperboat_edge_connector_reconnects_total`.
3. Retry with fresh authority: the host asks the control plane for a new admission or
   grant. Never reuse an expired credential, and never widen a grant to get past a denial.

## Usage reports are piling up

`paperboat_tunnel_usage_pending_reports`, `paperboat_tunnel_usage_pending_bytes` and
`paperboat_tunnel_usage_oldest_age_seconds` show the persisted usage queue (at most 4,096
reports or 64 MiB). A growing oldest age means the control plane cannot be reached or
rejects reports: check `control_url`, `control_ca_file` and the usage signing key file.

## Shutdown or drain failure

Send SIGTERM. The edge stops admitting new work, lets admitted work finish for half of
`--shutdown-timeout`, cancels the rest, then closes listeners and connector sessions. If the
process outlives `--shutdown-timeout`, check `service failure stage=shutdown` in its output,
confirm the listeners are closed (`ss -ltnup`), and only then restart it.

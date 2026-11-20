# paperboat-edge

Paperboat's public and private edge. The service owns authenticated connector-v1
carriers, route reconciliation, native TLS HTTP/1.1 and HTTP/2 ingress, HTTP/3
ingress, certificate selection, usage accounting, and node lifecycle.

The edge accepts only server-admitted connectors. HTTPS and TLS pass-through share
public TCP 443: HTTPS terminates at the edge, while pass-through preserves the origin
certificate and encrypted bytes. Each hostname has one mode. Private HTTP arrives over
an authorized connector stream and is sent to the private TLS listener. The infrastructure
hostname serves health and authenticated installation verification.
Plain HTTP redirects permanently to HTTPS.

## Run

```sh
paperboat-edge --node-id <id> --deployment-config /abs/path/deployment.json
```

`--node-id` and `--deployment-config` are required. The other flags have defaults:

| Flag | Default | Meaning |
|---|---|---|
| `--node-id` | none | Stable edge identity (`[a-z][a-z0-9_-]{0,127}`). |
| `--deployment-config` | none | Strict deployment JSON (see below). |
| `--edge-pool` | `default` | Pool this edge is assigned to. |
| `--relay-id`, `--relay-region`, `--relay-name` | node id, pool, region | Public relay metadata the edge advertises. |
| `--health-address` | `127.0.0.1:9090` | Loopback listener for health and metrics. |
| `--state-path` | `state/edge-state.json` | Private durable state file. |
| `--shutdown-timeout` | `30s` | Bound for drain and shutdown (at most 5 minutes). |

`paperboat-edge version` prints the build version.

### Deployment document

`deploy/deployment.example.json` shows every field. The loader is strict: unknown fields,
relative paths, or a missing carrier listener stop startup with a typed error.

- **Control plane:** `control_url`, `credential_issuer`, `control_credential_file`,
  `control_ca_file`, `jwks_file`, `revocations_file`, `usage_signing_key_file`.
- **Carrier identity:** `infrastructure_tls_cert_file` and `infrastructure_tls_key_file` are
  the edge's stable carrier certificate, which hosts and the control plane pin. An install
  hands it to the replacement process unchanged.
- **Listeners:** `carrier_tcp_listen_address` and `carrier_quic_listen_address` (both
  required, distinct ports), `public_https_listen_address`, `public_http_listen_address`
  (redirects to HTTPS), and `private_https_listen_address` (loopback only).
- **Public HTTPS certificate:** `public_https_cert_file` and `public_https_key_file`, both or
  neither.
- **Names:** `connector_advertise_host`, `preview_base_domain`, `tunnel_base_domain`,
  `runtime_base_domain`.
- **Limits:** `node_capacity`, `max_body_bytes` (default 100 MiB), `max_header_bytes`
  (default 32 KiB, at most 128 KiB), `control_interval`, `control_timeout`, and the optional
  `preview_carrier_interval`, `public_tcp_idle_timeout`, `request_body_idle_timeout`.
- **Node forwarding (managed edges):** a visitor who reaches a node that does not hold the carrier is passed on,
  as raw bytes, to the node that does (TLS is terminated on the holder only; raw TCP tunnel ports are bound on
  every node and forwarded the same way). Optional: `forward_enabled` (default true; self-hosted edges never
  forward), `forward_dial_timeout` and `forward_open_timeout` (3 s each), `forward_peer_interval` (10 s),
  `forward_seed_interval` (5 min), `forward_max_streams_per_peer` (4096). A forwarding node listens on every public
  TCP tunnel port its peers hold: raise the process's file limit (`LimitNOFILE` or the container's `ulimit -n`) to at
  least 65536; the edge logs `file_limit_low` at start when it is below listeners + connections + 1024.
- **Proxies and fixed routes:** `trusted_proxy_cidrs` and `public_routes` (host, optional path
  prefix, upstream).

### Health and metrics

The `--health-address` listener serves `GET /livez`, `GET /readyz` (503 until the node is
ready), `GET /diagnostics` and `GET /metrics`. Metric names and labels are listed in
[docs/metrics.json](docs/metrics.json). See [docs/operations.md](docs/operations.md) for
reporting and limits and [docs/runbooks.md](docs/runbooks.md) for failures.

### Replacing a running edge

Starting a new process next to a serving one hands over its sockets and certificates:
the new process warms up, asks the old one to release, then serves while the old one
drains. Hosts move their carriers to the new process on their own.

## Development

```sh
make check   # toolchain, dependency pins, source policy, metrics schema, fmt, vet, tests, build
make build   # bin/paperboat-edge (CGO off, Go 1.27.4)
```

The workspace module includes the shared `relay/selfhost` runtime-file
reader. Build from the Paperboat OSS workspace so both modules are available.

Production configuration is illustrated by `deploy/deployment.example.json`.
The container contains one executable, `paperboat-edge`; there are no external
proxy or signaling binaries to provision.

For native relay-only, tunnel-only, or combined installation, see
[self-host installation](../helper/deploy/README.md). The `pbh` helper generates the
strict deployment document after an authenticated dashboard claim. Installation
and claim need no DNS domains; configured browser/public routes need domain and
certificate readiness before publication. The checked-in JSON is a field and path
example for an already configured node.

## License

MIT. See [LICENSE](../LICENSE).

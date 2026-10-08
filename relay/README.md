# Paperboat relay

This module implements the private DERP-over-QUIC and DERP-over-WSS carriers and its standalone command.
It forwards opaque WireGuard packets and reliable peer discovery/control under signed
Paperboat authority. Its WSS listener is private native transport, not browser/public
HTTP ingress; the UDP peer-relay role reuses upstream Geneve. Its authenticated
lifecycle integrates startup, registry health/capacity, drain and endpoint revocation.
The current library and runtime fixtures are not a production deployment.

For native relay-only, tunnel-only, or combined installation and dashboard ownership,
see [self-host installation](deploy/README.md). `pbh` installs the service before
any account, team, or global pool is assigned.

Build from this directory into the ignored build directory:

```sh
go build -o build/paperboat-relay ./cmd/paperboat-relay
```

The command requires these flags, supplied from registered relay configuration:

| Flag | Value |
| --- | --- |
| `-listen` | Explicit UDP address and port |
| `-wss-listen` | Explicit TLS/WebSocket address and port |
| `-tls-cert` | PEM certificate file |
| `-tls-key` | PEM private key file |
| `-jwks` | Trusted issuer Ed25519 JWKS file |
| `-issuer` | Exact Paperboat issuer |
| `-node-id` | Registered relay node ID |
| `-node-generation` | Bootstrap generation of the preprovisioned node |
| `-control-url` | Authenticated control-plane HTTPS base URL |
| `-control-credential-file` | Owner-only control credential file |
| `-node-state` | Durable, owner-only startup generation/epoch state |

An authenticated dashboard claim provisions the registry row, endpoints, region, scope policy,
capacity and optional peer-relay service identity. Startup cannot create or widen that
authority. The command uses the authenticated native lifecycle APIs to claim the next
node generation and a fresh process epoch, persisting pending startup before requesting
it so a lost response can be retried. An exclusive Unix process lock prevents two local
processes from owning that state file. Unix fleet execution is supported in this gate.

Every three seconds the command reports listener readiness and actual connection capacity,
and obtains generation revocations for its bounded admitted endpoint inventory. Registry
validity is at most 60 seconds; health/capacity freshness and the command's independent
control lease are 15 seconds. Control loss cannot extend that lease: expiration drains
and closes relay connections. No application payload or credential enters observations.
SIGINT/SIGTERM drains for five seconds, publishes drain, then stops and waits for workers.
The JWKS is read once (at most 128 KiB and 16 keys); key rotation uses a controlled restart.

The [v1 carrier verifier](derpquic/authority.go) enforces
TLS certificate and signed-grant binding, node/epoch fencing and 60-second grant expiry.
The carrier uses
15-second carrier refresh, authority refresh at 30 seconds ±20% (approximately ten seconds with active regional
recovery), bounded reliable
control and two-fragment QUIC datagrams. WSS uses the same bounded packet frames over
one serialized TCP stream and therefore has TCP head-of-line blocking under loss. A configured Tailcat carrier factory replaces
native DERP/TCP selection. Admission/protocol failures fail closed; ordinary drain and
transport failures permit WSS reachability fallback and recovery to QUIC with current
authority. HTTP CONNECT proxy selection uses Go's standard proxy environment. The
carrier provides no WireGuard-data fallback onto QUIC reliable streams.
Ordinary lease expiry denies further traffic but permits reconnection with fresh signed
authority (QUIC application error 4, WSS close 4004). It does not permanently disable
the carrier. Revocation, invalid credentials and protocol violations remain fatal.

Relay capacity is bounded at 256 concurrent connections globally and 16 per account.
The peer-relay packet shaper's 4,096 packets/second rate and 128-packet burst are fixed
workload budgets. These limits define admission fairness and bounded resource use, so
installation does not expose separate tuning knobs for them.

Focused verification:

```sh
go test ./derpquic
go test -race ./derpquic
```

Tests exercise actual ephemeral TLS/QUIC and WSS peers, mixed-leg 1,312-byte forwarding
through an HTTP CONNECT proxy,
reliable control, signed admission and scope denial, expiry/revocation, restart,
fallback/recovery, overlapping reconnect, cancellation and shared account bounds. Fragment tests cover reorder,
duplicates, replay and bounded reassembly. These carrier tests do not establish
application payload encryption, sustained throughput or connection migration;
Tailcat integration and owning qualification tasks provide their separate evidence.

To enable the UDP peer-relay role on the same registered node, also supply
`-peer-relay-service` (public service descriptor JSON), `-peer-relay-disco-key`
(file containing the private discovery key as canonical rawurl base64), and
`-peer-relay-addresses` (one to four comma-separated advertised IP:port endpoints,
all using the same port). The descriptor fields are `wireguard_public_key`,
`disco_public_key`, and `virtual_address`. The registry must publish that exact public
identity with roles `relay`, `peer_relay` and transports `derp_quic`, `derp_wss`, `peer_relay_udp`.
The command verifies that the private discovery key matches its public descriptor,
and that the descriptor matches the authenticated node lease before publishing readiness.
The UDP role has independent sockets and lifecycle but reuses the same authenticated
DERP control writer. It never introduces public ingress or a temporary control carrier.

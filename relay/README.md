# Paperboat relay

This module implements the private DERP-over-QUIC and DERP-over-WSS carriers and its standalone command.
It forwards opaque WireGuard packets and reliable peer discovery/control under signed
Paperboat authority. Its WSS listener is private native transport, not browser/public
HTTP ingress; the UDP peer-relay role reuses upstream Geneve. Its authenticated
lifecycle integrates startup, registry health/capacity, drain and endpoint revocation.
The current library and runtime fixtures are not a production deployment.

For native relay-only, tunnel-only, or combined installation and dashboard ownership,
see [self-host installation](../helper/deploy/README.md). `pbh` installs the service before
any account, team, or global pool is assigned.

Build from this directory into the ignored build directory:

```sh
go build -o build/paperboat-relay ./cmd/paperboat-relay
```

The command requires these flags, supplied from registered relay configuration:

| Flag | Value |
| --- | --- |
| `-listen` | Explicit UDP address and port |
| `-wss-listen` | Explicit TLS/WebSocket address and port, distinct from `-listen`; the same port also serves STUN over UDP |
| `-tls-cert` | PEM certificate file |
| `-tls-key` | PEM private key file |
| `-jwks` | Trusted issuer Ed25519 JWKS file |
| `-issuer` | Exact Paperboat issuer |
| `-node-id` | Registered relay node ID |
| `-node-generation` | Bootstrap generation of the preprovisioned node |
| `-control-url` | Authenticated control-plane HTTPS base URL |
| `-control-credential-file` | Owner-only control credential file |
| `-node-state` | Durable, owner-only startup generation/epoch state |

Optionally, `-control-ca` names a CA bundle to trust for the control-plane HTTPS connection.

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
The JWKS is read once (at most 128 KiB and 128 keys); key rotation uses a controlled restart.

The [v1 carrier verifier](derpquic/authority.go) enforces
TLS certificate and signed-grant binding, node/epoch fencing and 60-second grant expiry.
The carrier uses
15-second carrier refresh, authority refresh at 30 seconds ±20% (approximately ten seconds with active regional
recovery), bounded reliable
control and QUIC datagrams (one datagram per packet; two fragments only when the path MTU does not fit it). WSS uses the same bounded packet frames over
one serialized TCP stream and therefore has TCP head-of-line blocking under loss. A configured Tailcat carrier factory replaces
native DERP/TCP selection. Admission/protocol failures fail closed; ordinary drain and
transport failures permit WSS reachability fallback and recovery to QUIC with current
authority. HTTP CONNECT proxy selection uses Go's standard proxy environment. The
carrier provides no WireGuard-data fallback onto QUIC reliable streams.
Ordinary lease expiry denies further traffic but permits reconnection with fresh signed
authority (QUIC application error 4, WSS close 4004). It does not permanently disable
the carrier. Revocation, invalid credentials and protocol violations remain fatal.

## Relay mesh

Peers homed on different Paperboat-managed relays reach each other through a mesh of relay-to-relay links
([mesh.go](derpquic/mesh.go), contract in `shared/nodemesh`). The lease's `mesh_peers` lists the other managed relays
with the SPKI pin of each one's serving certificate (empty on a self-hosted relay: no mesh). Each pair keeps one QUIC
connection on the relay's own UDP port with ALPN `paperboat-derp-v1`'s sibling `paperboat-derp-mesh-v1`; the lower node
id dials, both present their serving certificate and accept only a pinned one, and clients keep the derp-v1 handshake
unchanged. A control stream carries `Present`/`Gone` for the route keys each relay holds (a snapshot on link up; a broken
link clears what the peer held and the redial, backed off with jitter, refloods) and `NeedGrant`/`Grant`; forwarded
packets are QUIC datagrams naming source and destination keys.

A destination connected here always wins. When it is not, a relay that holds it gets the packet once: a packet that
arrived over the mesh is delivered here or dropped, never forwarded again. The delivering relay authorizes: it fetches the
sender's signed grant tokens from the relay the sender is connected to (the first packet of a flow is dropped meanwhile),
verifies them with the same issuer keys, and applies the pair rule and revocation fences itself; verified grants live in a
bounded LRU. Accounting is once: the ingress relay charges the carrier and account budgets and counts no `BytesOut` for
transit; the delivering relay counts the forwarded bytes. Mesh counters (`links`, `names`, `forwarded`, `accepted`,
`holder_miss`, `unreachable`, `loop_refused`, `bytes_in`, `bytes_out`) go to the control plane in the observation's `mesh`
field. A draining relay keeps its links and announcements until the drain deadline.

A relay has no connection limit: it admits every authorized connection and capacity is watched by resource alerts.
Forwarding is fair by byte budgets per carrier and per account (an account's speed is shared across its relays by
demand). `capacity_limit` in the registry is the node's declared size, shown on the capacity page; it never ranks or
refuses a relay (clients rank relays by measured RTT). The UDP peer-relay role allows 128
allocations, 16 per account, and shapes forwarding to 49,152 packets/second with a
4,096-packet burst per account. These peer-relay limits are fixed workload budgets that
define admission fairness and bounded resource use, so installation does not expose
separate tuning knobs for them.

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

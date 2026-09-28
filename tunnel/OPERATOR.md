# Self-hosted tunnel operations

A self-hosted tunnel is a public and private data-plane installation controlled by
its machine operator. The Paperboat control plane remains required for registration,
account pairing, authorization, routes, certificates, revocation and usage accounting.
Pairing never grants deployment administration or access to another paired account's
devices, routes or traffic.

Official Paperboat images report bounded, privacy-filtered failures to Paperboat's Sentry
project by default. Set `PAPERBOAT_SENTRY_ENABLED=false` to disable all reporting. An
ordinary source or Docker build has no embedded DSN and stays disabled unless the operator
sets `PAPERBOAT_SENTRY_ENABLED=true`, `PAPERBOAT_SENTRY_DSN`, and
`PAPERBOAT_SENTRY_RELEASE`. Nonempty DSN and release environment values override embedded
official-build values. Logs and metrics default on and sampled traces default to `0.1`;
use `PAPERBOAT_SENTRY_LOGS_ENABLED`, `PAPERBOAT_SENTRY_METRICS_ENABLED`, and
`PAPERBOAT_SENTRY_TRACES_SAMPLE_RATE` to tune them. Reporting keeps only fixed operation,
outcome, and code values; it excludes raw errors, traffic payloads, headers, URLs, resource
identifiers, file paths, locals, and command arguments. Each process emits at most 8
failure events, 128 operation logs, 128 traces, and 64 detailed operation metric points
per minute, with rotating cumulative exports of at most 64 series every 15 seconds.

Public browser HTTP terminates TLS at this edge. The operator can therefore observe
HTTP connection metadata and plaintext in the running process; it is not end-to-end
encrypted to the origin. Native private access remains authorized separately. TLS/SNI
pass-through preserves opaque bytes and uses the origin's certificate, but the visible
SNI name and connection metadata remain visible to the edge. HTTPS and TLS pass-through
share TCP 443; no extra TLS port or reservation is required. Use standard-port URLs.
One canonical hostname has one mode, including overlapping HTTP wildcards. Remove the
conflicting HTTP publication before creating a TLS route; the infrastructure hostname
is reserved for HTTPS. UDP 443 remains HTTP/3, not opaque QUIC pass-through.

## Public network and DNS

Use a stable public IPv4 or IPv6 address. Permit inbound TCP 80 and 443, UDP 443,
carrier TCP 27443, and carrier UDP 27444. Permit outbound HTTPS to the Paperboat
control plane and outbound connections to configured origins. The carrier ports may
be changed, but the exact public ports passed to setup must map to the same container
ports. Do not expose the loopback health or private HTTPS listeners.

Create `A` and/or `AAAA` records for the infrastructure hostname and for each managed
base domain. Route both the base and wildcard records to the edge, for example:

```text
edge.example.com              A/AAAA  PUBLIC_IP
preview.example.com           A/AAAA  PUBLIC_IP
*.preview.example.com         A/AAAA  PUBLIC_IP
tunnels.example.net           A/AAAA  PUBLIC_IP
*.tunnels.example.net         A/AAAA  PUBLIC_IP
runtime.example.org           A/AAAA  PUBLIC_IP
*.runtime.example.org         A/AAAA  PUBLIC_IP
```

The three base domains must be real, non-overlapping DNS names. When browser access is
later enabled, their registrable-domain isolation must also pass the runtime's public
suffix checks. DNS propagation and certificate distribution must complete before a
route can become ready; switching a connector alone does not move public ingress.

Obtain a publicly trusted certificate for the exact infrastructure hostname. Place its
full chain at `deploy/tls/tls.crt` and private key at `deploy/tls/tls.key`, owned by UID/
GID 10001 with directory mode `0700` and key mode `0600`. ACME DNS-01 credentials stay
with the certificate automation or Paperboat control plane and must not be mounted into
the tunnel container. The control plane distributes managed preview/tunnel certificates
over the authenticated certificate channel. For TLS/SNI pass-through, the origin owns
issuance, renewal, hostname coverage and application authentication; the edge does not
issue or replace that certificate.

## Register and start

Copy `deploy/.env.example` to `deploy/.env`, set the image manifest digest, and create
`deploy/operator` owned by UID/GID 10001 with mode `0700`. Register through Compose:

```sh
docker compose --env-file deploy/.env -f deploy/docker-compose.yml run --rm tunnel-admin operator setup \
  --state-dir /var/lib/paperboat-tunnel/operator \
  --control-url https://api.paperboat.example \
  --name "Friends edge" --endpoint-host edge.example.com \
  --tcp-port 27443 --quic-port 27444 --region in-blr \
  --failure-domain home-ups-1 --capacity-limit 128 \
  --tls-cert /run/paperboat-tunnel/tls.crt \
  --tls-key /run/paperboat-tunnel/tls.key \
  --preview-domain preview.example.com \
  --tunnel-domain tunnels.example.net \
  --runtime-domain runtime.example.org
```

For a private control-plane CA, mount its bundle under `/run/paperboat-tunnel` and pass
`--control-ca` with that absolute container path. Setup writes `operator.json`,
`runtime.credential`, `jwks.json`, `revocations.json`, `usage.key`, `deployment.json`
and `runtime.args` atomically as `0600`. The directory is `0700`; the bearer and usage
private key are never printed. Setup registers the exact usage public key and records
the returned installation-specific edge pool, node ID and generation. An interrupted
first setup reuses its pending operator identity when retried with the same control URL
and capability.
`tunnel-admin` is an on-demand Compose profile with read-write access to this protected
directory. The long-running `tunnel` service mounts only `runtime.args`, the runtime
bearer, trust and revocation documents, usage key, and deployment configuration as
individual read-only files. It cannot read `operator.json` or the pending operator key.
Run setup before starting `tunnel` so those bind-mounted files exist. Because setup and
rotation replace files atomically, apply every generated-file or TLS replacement with
`docker compose --env-file deploy/.env -f deploy/docker-compose.yml up -d
--force-recreate tunnel`; a plain restart retains the old bind-mounted inode.

Run `docker compose --env-file deploy/.env -f deploy/docker-compose.yml config`, then
start the service. Compose checks the private loopback `/readyz`, but container health
alone does not prove public DNS or certificate reachability. Use the operator's ingress
verification only after the public hostname, TCP/UDP mappings and certificate are live;
run:

```sh
docker compose --env-file deploy/.env -f deploy/docker-compose.yml run --rm tunnel-admin operator verify-ingress --state-dir /var/lib/paperboat-tunnel/operator
```

Then pair an account and require `pb selfhost list` to show `ready`. An `unavailable`
result requires checking control reachability, DNS, carrier TCP/UDP, public HTTPS/HTTP3,
certificate state, connector state and logs. It must not be bypassed by marking the node
ready manually.

## Pair and select pools

Generate a new code for each account:

```sh
docker compose --env-file deploy/.env -f deploy/docker-compose.yml run --rm tunnel-admin operator create-code --state-dir /var/lib/paperboat-tunnel/operator
pb selfhost pair CODE
pb selfhost list
pb selfhost pool set tunnel --mode self-hosted-only --installation INSTALLATION_ID
```

The account confirms the displayed installation identity and trust notice. `mixed`
selects among eligible Paperboat-operated and selected self-hosted installations using
current health and regional policy. `self-hosted-only` never falls back and reports an
unavailable pool. Relay and tunnel policies are independent; direct private P2P remains
preferred. Run local administration through `tunnel-admin`; use `operator list` and
`operator revoke --account-id ACCOUNT_ID` for local
administration. `pb selfhost remove INSTALLATION_ID` previews distinct account-side
removal. Pairings are never inherited by another account. With no explicit pool policy, mixed
mode includes all active pairings. Setting a policy makes its installation list exact;
new pairings are not added automatically, and an empty list selects no private nodes.
The operator pays the server and network costs. Signed, authorized self-hosted traffic
is counted but cannot debit Paperboat bandwidth quota; account/device entitlements
still apply.

## Run relay and tunnel together

Relay and tunnel remain separate installations, identities, credentials, state and pool
choices even on one host. Configure the relay under `paperboat-relay/deploy` first and
the tunnel under this directory. From this workspace layout,
`docker-compose.both.yml` includes both service definitions. Export the values from both
`.env` files or pass one combined `--env-file`, then validate it before starting. Do not
share either `operator` directory or state volume. Ensure the
relay and tunnel do not claim the same TCP or UDP host port; the examples use relay 443
and tunnel 80/443/27443/27444, so choose a different relay port or separate public IP
when they share a host.

## Backup, rotation, update and removal

Back up the protected operator directory, TLS material, Compose `.env`, and both tunnel
volumes. The operator directory is an administrative secret and must never be mounted
into the long-running service or copied into either runtime volume. Encrypt backups and
never restore one identity into two live installations.
`operator rotate` atomically replaces the runtime credential; force-recreate afterward
so Docker binds the replacement inode. Certificate replacement also requires a
force-recreate for the infrastructure certificate. Rotate the tunnel usage signing key with `operator
rotate-usage --state-dir /var/lib/paperboat-tunnel/operator`; it registers the new public
key and atomically rewrites `usage.key`, after which the runtime must be force-recreated. Control
trust and revocations are refreshed by the running service after bootstrap; investigate
stale-control health rather than copying credentials from another node.

For updates, retain the current digest, pull the new pinned digest, recreate, and verify
private health plus paired public readiness. Rollback uses the prior digest and unchanged
volumes. Active TCP/QUIC streams may close during restart or regional failure and are not
replayed or seamlessly migrated.

Before decommissioning, move paired accounts away or warn that self-hosted-only will be
unavailable. Run `operator remove` through `tunnel-admin`, stop Compose, then delete
protected state, private
keys, volumes, backups and DNS records under the operator's retention policy. Revoking
one account does not remove the installation or other isolated pairings.

Repeat setup with the original registered name, host, carrier ports, region, failure domain and capacity. Certificate paths, the control CA and local tunnel base domains may be regenerated; recreate the runtime container afterward. To change registered node settings, remove the installation and create a new one, then issue fresh pairing codes. Setup rejects a mismatch before rewriting runtime files.

Tunnel ingress authority refreshes every 5 seconds and lasts at most 10 seconds. Removal
or policy changes withdraw subsequent authority; existing traffic closes when withdrawal
is applied or its authority expires during a control-plane outage. This is a bounded
revocation window, not immediate global connection termination or seamless TCP migration.

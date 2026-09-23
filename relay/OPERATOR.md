# Self-hosted relay operations

A self-hosted relay is an independently operated data-plane installation. It still
requires the Paperboat control plane and a signed-in Paperboat account. Pairing an
account grants that account use of this exact installation; it does not grant server
administration, device access, or access to another paired account's traffic. Relay
payloads remain end-to-end encrypted by the endpoints.

Official Paperboat images report bounded, privacy-filtered failures to Paperboat's Sentry
project by default. Set `PAPERBOAT_SENTRY_ENABLED=false` to disable all reporting. An
ordinary source or Docker build has no embedded DSN and stays disabled unless the operator
sets `PAPERBOAT_SENTRY_ENABLED=true`, `PAPERBOAT_SENTRY_DSN`, and
`PAPERBOAT_SENTRY_RELEASE`. Nonempty DSN and release environment values override embedded
official-build values. Logs and metrics default on and sampled traces default to `0.1`;
independently set `PAPERBOAT_SENTRY_LOGS_ENABLED`,
`PAPERBOAT_SENTRY_METRICS_ENABLED`, and `PAPERBOAT_SENTRY_TRACES_SAMPLE_RATE` (0 through
1) to override them. Hosted deployments set both `PAPERBOAT_SENTRY_REGION` (a public
lowercase region) and `PAPERBOAT_SENTRY_INSTANCE` (`slot-01` through `slot-64`); invalid
or partial pairs stop startup. `PAPERBOAT_SENTRY_ENVIRONMENT` defaults to `production`. Reporting
uses fixed operation, outcome, and code values. It excludes raw errors, carrier payloads,
headers, URLs, resource identifiers, file paths, locals, and command arguments. Trace
context is never added to customer traffic and baggage propagation is disabled. A process
emits at most 8 failure events, 128 operation logs, 128 traces, and 64 detailed operation
metric points per minute. Every 15-second cumulative export independently emits a rotating
window of up to 64 series. Shutdown waits at most two seconds to drain; a drain does not
prove server receipt.
The SDK counter `paperboat.operation.count` and distribution
`paperboat.operation.duration` cover fixed lifecycle outcomes. Every 15 seconds the
relay also exports cumulative `paperboat_relay_*_snapshot` and
`paperboat_peer_relay_*_snapshot` gauges for admissions, sessions, packets, allocations,
and capacity. These carry only fixed outcome labels.
The cumulative drop gauges reveal local admission loss, and
`paperboat_reporting_signal_enabled` exposes the configured state of each signal. Fleet
views group by the validated region and stable instance slot; they do not infer identity
from hostnames or node IDs.

## Network and identity setup

Use a host with a stable public IPv4 or IPv6 address. Create an `A` and/or `AAAA`
record such as `relay.example.com` and wait until public DNS resolves to that address.
Allow inbound TCP and UDP on the two ports passed to `operator setup` (443 for each in
the example). Allow outbound HTTPS to the Paperboat control origin. Do not publish an
HTTP administration port.

Obtain a publicly trusted TLS certificate whose SAN covers the exact relay hostname.
An ACME HTTP-01 client needs TCP 80 temporarily; a DNS-01 client instead needs narrowly
scoped credentials for `_acme-challenge.relay.example.com`. Renewal belongs to the
operator. Place the full chain at `deploy/tls/tls.crt` and its private key at
`deploy/tls/tls.key`; make the directory `0700`, the key `0600`, and both owned by UID/
GID 10001 used by the image. Never put DNS credentials in the relay container.

Copy `deploy/.env.example` to `deploy/.env`, set the immutable image digest, create
`deploy/operator` owned by UID/GID 10001 with mode `0700`, then run registration once
through Compose:

```sh
docker compose --env-file deploy/.env -f deploy/docker-compose.yml run --rm relay-admin operator setup \
  --state-dir /var/lib/paperboat-relay/operator \
  --control-url https://api.paperboat.example \
  --name "Friends relay" --endpoint-host relay.example.com \
  --tcp-port 443 --quic-port 443 --region in-blr \
  --failure-domain home-ups-1 --capacity-limit 128 \
  --tls-cert /run/paperboat-relay/tls.crt \
  --tls-key /run/paperboat-relay/tls.key
```

The command creates the directory as `0700` and atomically writes `operator.json`,
`runtime.credential`, `jwks.json`, and `runtime.args` as `0600`; it never prints the
bearer credential. Preserve them.
Relay `--capacity-limit` accepts at most 256, matching the runtime's global connection
ceiling; the runtime also enforces 16 concurrent connections per account.
An interrupted first setup can be retried with the same control URL and capability.
The generated runtime arguments contain the returned node ID and generation and the
current control-plane JWKS. Do not edit them, invent keys, or copy a browser token.
`relay-admin` is an on-demand Compose profile with read-write access to this protected
directory. The long-running `relay` service mounts only `runtime.args`,
`runtime.credential`, and `jwks.json` read-only; it cannot read `operator.json` or the
pending operator key. Run setup before starting `relay` so those bind-mounted files
exist. Because setup and rotation replace files atomically, apply every generated-file
or TLS replacement with `docker compose --env-file deploy/.env -f
deploy/docker-compose.yml up -d --force-recreate relay`; a plain restart retains the old
bind-mounted inode.

Validate configuration with `docker compose --env-file deploy/.env -f
deploy/docker-compose.yml config`, then start the service. The relay has no public HTTP
health endpoint. Use `pb selfhost list` after pairing: `ready` means the authenticated
node lease and listeners have been observed; `unavailable` is a failure that requires
checking DNS, both transports, certificate validity, control reachability, and logs.

## Pairing and pool policy

Generate a fresh bounded single-use code and send it through a private channel:

```sh
docker compose --env-file deploy/.env -f deploy/docker-compose.yml run --rm relay-admin operator create-code --state-dir /var/lib/paperboat-relay/operator
pb selfhost pair CODE
pb selfhost list
pb selfhost pool set relay --mode self-hosted-only --installation INSTALLATION_ID
```

The account sees the installation identity, capability, node and trust notice before
confirmation. `mixed` allows eligible Paperboat-operated and selected self-hosted nodes;
`self-hosted-only` fails explicitly when its selected installations are unavailable.
Direct private P2P remains preferred. Configure relay and tunnel pools independently.

Run protected administration through `relay-admin`. List or revoke account access with
`operator list` and `operator revoke
--account-id ACCOUNT_ID`. Revocation fences new authority and existing connections under
the bounded control-plane lease; it does not grant the operator visibility into payloads.
Account-side `pb selfhost remove INSTALLATION_ID --yes` removes only that account's
pairing. Codes are not inherited by other accounts.

## Maintenance and removal

Back up the protected operator directory, TLS key/certificate, Compose `.env`, and the
`relay-state` volume. The operator directory is an administrative secret and must never
be mounted into the long-running service or copied into its state volume. These assets
contain installation identity and credentials; encrypt the
backup and never restore it as a second concurrently running installation. Rotate the
runtime bearer with `operator rotate`, then force-recreate the relay so the process binds
and reads the rewritten credential. Renew TLS before expiry and force-recreate after
replacing its files.

For updates, preserve the current image digest, pull the new digest, recreate the
service, and require `pb selfhost list` to return `ready`. Roll back by restoring the
recorded digest and the same state volume. Existing streams close during restart and
are not replayed.

Before decommissioning, move paired accounts to another eligible pool or warn them that
self-hosted-only selection will become unavailable. Run `operator remove` through
`relay-admin` while the
operator identity still exists, stop Compose, then delete the operator directory, TLS
private key, state volume, backups and DNS records under the operator's retention policy.

Repeat setup with the original registered name, host, carrier ports, region, failure domain and capacity. Certificate paths, the control CA and local tunnel base domains may be regenerated; recreate the runtime container afterward. To change registered node settings, remove the installation and create a new one, then issue fresh pairing codes. Setup rejects a mismatch before rewriting runtime files.

Relay authorization is observed every 3 seconds. Account revocation closes that account's
active carriers on the next successful observation; a relay that loses control-plane
contact fails closed when its existing lease expires, at most 15 seconds after renewal.
Old grants remain fenced if the account later pairs again. Clients reconnect with fresh
authority; TCP streams are not replayed.

# Preview And Tunnel Control-Plane Recovery

Use this runbook for preview leases, durable tunnels, routes, custom domains,
connectors, edge assignments, and certificate distribution. Preserve the last
known-good generation while investigating. Never delete state, manufacture an
acknowledgement, or retry a mutation with a new idempotency key.

## Record safe evidence first

Record the account-safe resource ID, operation ID, request or correlation ID,
desired and observed generations, typed health dimension, first observation,
and next retry time. Do not record credentials, authorization headers, signed
grants, certificate private keys, provider tokens, origin payloads, or private
hostnames.

## Reconciliation or edge-assignment lag

1. Confirm the server worker is running and PostgreSQL is healthy before
   changing any resource.
2. Compare the desired configuration generation with the connector session,
   edge process epoch, assignment generation, and last complete snapshot.
3. Treat an incomplete snapshot, stale acknowledgement, or old process epoch as
   rejected input. Keep the active generation and allow the normal retry path
   to reconcile.
4. If a replacement is staged, require its route and origin readiness before
   draining the old assignment. Never mark the new assignment ready manually.
5. Close the incident only after the durable operation and edge observation
   agree and the retired generation can no longer remove the replacement.

## Managed names and a dead edge

Managed tunnel, preview and runtime hostnames are covered by DNS-only wildcards (`*.pprbt.app`, `*.runtime.pprbt.dev`)
that list every managed edge; the server publishes no per-hostname records for them, and a custom domain's CNAME
points at the resource's own managed name. Edges do not forward, and a machine holds a carrier to every ready node of
its workspace's two regions, so a visitor may land on any edge. A route is reported ready once any live edge serves it;
an edge that lags answers its share of visitors wrongly until it converges (an address move still waits for every live
edge to serve the new name).

With `PAPERBOAT_CERTIFICATES_MANAGE_EDGE_WILDCARDS=true` the server keeps both wildcards itself (owner
`edge_wildcard:<name>`, every 10 s on the worker leader): the A/AAAA records are the verified public ingress addresses
of every managed edge that is `ready`, not draining, has a heartbeat under 15 s, is registered and sits in an enabled
region. A node that dies, is drained or retired leaves the set on the next tick; a node that becomes ready (and has
passed `verify-ingress`) joins it. The server never publishes an empty set: with no ready edge it logs
`edge_wildcard_dns_empty` and leaves the old records. Nothing to remove by hand any more.

A node whose load (`load_percent` in the admin panel: the worst of its sustained CPU and its connections against its
own limit) reaches `PAPERBOAT_NODE_CAPACITY_LOAD_OUT_PERCENT` (85) is left out of the names and comes back only
below `PAPERBOAT_NODE_CAPACITY_LOAD_IN_PERCENT` (70); a region always keeps its least loaded ready node, and a
node with no recent report is never dropped. The admin panel shows such a node as paused for load (`in_rotation`
false). The same rule, thresholds and stored state apply to managed relays. Balancing happens inside a region: the
client picks the region by round trip and then uses the first relay the server lists in it, so within a region the
list is ordered least loaded first (a relay with no recent report last). A relay at or above the load-out threshold is
left out of the list altogether (account configuration and peer-relay grants, within about 5 s, the candidate cache
TTL) and returns below the load-in threshold; the last ready relay of a region is never left out, and a self-hosted
owner's own relays are never touched.
The relay decision runs on the worker leader every 10 s whether or not `PAPERBOAT_CERTIFICATES_MANAGE_EDGE_WILDCARDS`
is on. Log lines:
`edge_wildcard_dns_published` (hostname, addresses), `edge_wildcard_dns_conflict` (hostname, error),
`edge_wildcard_dns_empty`.

## Turning on the wildcard DNS sync (one time)

The sync refuses to touch A/AAAA records it did not create (it reports a conflict and changes nothing), so the
hand-made records for the two wildcard names must be deleted. Enable first, then delete, so the gap is seconds (the
worker publishes within its 10 s loop) and not a redeploy.

1. Deploy the server with `PAPERBOAT_CERTIFICATES_MANAGE_EDGE_WILDCARDS=true` (default false). It starts logging
   `edge_wildcard_dns_conflict` every 10 s for the two names: expected until step 3.
2. Admin panel, Infrastructure, Nodes: every managed edge shows State `ready` and Ingress verified (run Verify ingress
   on any that does not); the Load column fills in within a minute.
3. In the DNS provider zone of each name (the public zone for the preview wildcard under the preview base domain, the
   runtime zone for the runtime wildcard under the runtime base domain; the zone ids are the server's
   `PAPERBOAT_CERTIFICATES_PUBLIC_DNS_ZONE_ID` and `PAPERBOAT_CERTIFICATES_DNS_ZONE_ID`) delete every A and AAAA
   record whose name is exactly the wildcard (`*.<base domain>`). Keep everything else.
4. Within 30 s: `dig +short A x.<preview base domain>` and `dig +short A x.<runtime base domain>` return one address
   per ready managed edge, the server log shows `edge_wildcard_dns_published`, and there are no further conflicts.
5. Rollback: set the flag to false and redeploy, then re-create the hand-made records. The sync leaves the records it
   created in place until it is removed: delete those by hand first.

## Move a region to a bigger machine

Enrol the new node into the same region, so the region briefly has two nodes, and swap without a gap:

1. Install the new machine with `pbh install --region <id>`, claim it from the admin panel, and wait until it is `ready`.
2. `verify-ingress` on the new node; it joins the wildcards on the next tick and machines attach to it.
3. **Drain** the old node (`drain`, or `drain-move` to push its machines off now, see operator-admin.md). It leaves the
   wildcards at once.
4. Watch `carriers` on the old node (`GET /v1/admin/nodes`) fall to 0.
5. **Retire** the old node and remove the machine.

Do not retire first: with one node in the region the region has no edge until the new one is ready.

## Public TCP tunnel ports on the edges

Raw TCP tunnels get a port from 10000-59999 (`shared/edgeports`, the one value the server allocator, the edge
listener, pbh and `pbh doctor` all use) and the edge listens only on ports owned by a live public TCP tunnel; it
refuses and reports any port outside the range. The host needs three things, and `pbh install` (and every `pbh serve`
start) sets up the first two:

1. The outgoing port range above the public one: `/etc/sysctl.d/90-paperboat-edge.conf` with
   `net.ipv4.ip_local_port_range = 60000 65535`, applied at once. pbh refuses, naming the cause, when another sysctl
   setting overrides it.
2. The host firewall: if ufw, firewalld, iptables/ip6tables or nftables is filtering inbound traffic, pbh adds allow
   rules (comment `paperboat-edge`) for the ports the installed components listen on: the claim port (7277 by
   default), TCP 443 and UDP 443 (HTTP/3), the carrier TCP 7272 and QUIC 7273 of an edge, a relay's WSS/STUN
   and QUIC ports, and TCP 10000-59999 on a tunnel node. iptables and nftables rules do not persist, so `pbh serve` re-adds them on each start;
   ufw and firewalld keep theirs. The claim port stays open because the claim listener keeps running and accepts
   the same code again for a trust-rotation retry.
3. The cloud firewall (security group, security list, VPC firewall): pbh cannot change it from the host; it prints
   "allow inbound TCP 7277, 443, 7272, 10000-59999 and UDP 443, 7273 from 0.0.0.0/0 (and ::/0 for IPv6)" (the
   ports of that node). The managed edges' Oracle Cloud lists must be changed to these ports (they allowed the old ones).

`pbh uninstall` removes exactly what `/var/lib/paperboat-selfhost/hostnet.json` records pbh added (rules, sysctl
file, and restores the previous outgoing range). `pbh doctor` reports each check and probes reachability of one test
port through the control plane (`POST /v1/selfhost/port-probe`: the server connects to the node's registered public
address and expects the nonce echoed): `timeout` means a cloud firewall drops it, `unreachable` that the host
firewall rejects it, `refused` that the host answered with nothing listening. From outside the cloud you can also
`nc -vz <edge-ip> <live tunnel port>`.

## Forwarding between edge nodes

Managed edge nodes forward a visitor to the node that holds the machine's carrier, so a visitor can land on any managed
node (DNS stays round-robin over every ready, non-draining managed node of every region) and still reach the app.

- **What it is.** The node the visitor reached reads the TLS server name, finds a holder in its directory and copies the
  still-encrypted stream to it over a persistent link between the two nodes (mutual TLS, pinned to the carrier certificate
  each node registered). The holder terminates TLS and applies every policy, so a forwarding node never decrypts and never
  meters the visitor. A forwarded stream is never forwarded again (hop limit 1). Raw TCP tunnels are held under
  `tcp:<port>` and forwarded the same way. HTTP/3 is not forwarded.
- **Where the data comes from.** `POST /v1/edge/forward/peers` lists the other managed edge nodes (ready or draining,
  heartbeating, carrier certificate registered) with their pins; `POST /v1/edge/forward/holders` pages which nodes hold
  which names. Both answer only managed nodes, and a self-hosted node gets an empty answer and is never listed: the two
  populations are never mixed. Nodes also tell each other what they hold over the link; the server answer is the seed and
  the repair for a link that is not up.
- **A draining node** keeps its links and the streams already forwarded through it until they end or the drain deadline.
  It is no longer a new holder: it refuses new forwards (`draining`) and the forwarding node tries the next holder; peers
  list it as draining and use it last. A node that is down has its names withdrawn when its link breaks.
- **Counters.** Each edge reports `forward` in its heartbeat and each relay `mesh` in its observation (links, held names,
  forwarded, accepted, holder misses, unreachable, loop refusals, bytes in and out). The operator node list shows them as
  `forward`. Rising `unreachable` or `holder_miss` with a healthy fleet means links are down or the holder snapshot is
  behind: check the peers answer for the nodes in question and their last heartbeat.
- **DNS.** Nearest-region steering is not part of this: name resolution stays round-robin. Geo or latency steering needs
  Cloudflare Load Balancing, a separate paid product (about 5 USD a month for two origins plus per-origin and
  health-check charges; geo and latency steering and wildcard hostnames depend on the plan, some only on Enterprise;
  all unverified and not bought). A proxied (orange cloud) record is not an option: it would terminate TLS outside our
  trust boundary.

## DNS ownership or certificate failure

1. Separate `dns` health from `certificate` health. A DNS mismatch must not
   withdraw the managed Paperboat hostname or a still-valid certificate.
2. Use the current domain instruction projection. Verify the normalized
   hostname, authoritative CNAME or TXT target, provider proxy mode, DNSSEC,
   and CAA result. Never infer instructions from an edge node.
3. For a customer-owned domain, verify its `_acme-challenge` CNAME points to
   the immutable Paperboat challenge target before retrying issuance. The shared
   Paperboat-owned public wildcard and separate runtime wildcard have one owner each: the server
   writes their TXT records directly through the configured provider and must
   not depend on an external CNAME.
4. Confirm one issuance lease owns the domain and certificate generation.
   Preserve the previous active certificate until every captured edge target
   has staged and acknowledged the replacement.
5. For renewal or revocation failure, inspect the durable retry state and
   expiry horizon. Do not copy provider credentials or certificate key bytes
   to an edge or a ticket.

## Managed certificate deployment readiness

Run this gate before deploying or restarting the server or rolling a
paperboat-edge image. Production requires
`PAPERBOAT_CERTIFICATES_ENABLED=true`; `false` is not a production workaround
for missing references or failed issuance. A healthy process is not
sufficient: the server-side issuer, durable certificate ledger, authenticated
distribution handler, and edge broker must all be composed together.

### Reference-only secret injection

1. Keep only public certificate settings and opaque references in the server
   environment. The required overlay names are the `PAPERBOAT_CERTIFICATES_*`
   names in `deploy/.env.example`; the reference values identify the ACME
   account key, envelope master key, DNS token, distribution credential, and
   owner, but are not secret bytes.
2. For a reference `R`, the environment adapter derives
   `PAPERBOAT_CERT_SECRET_*` by uppercasing each alphanumeric rune, replacing
   every other rune with `_`, and appending `_` plus the first 12 lowercase
   hexadecimal characters of `SHA-256(R)`. This digest suffix prevents
   punctuation-normalization collisions. For example, the reference
   `secret://edge/distribution` maps to the name
   `PAPERBOAT_CERT_SECRET_SECRET___EDGE_DISTRIBUTION_db21dd9c8bbf`.
   The example is a name only and must never be populated with a value in
   source control, SQL, logs, or evidence.
3. Inject the referenced values into the server process only at startup or
   restart, with the secret manager's normal bounded environment mechanism.
   Do not persist or print PEM data, private keys, DNS tokens, master keys, or
   credentials. Confirm that
   `PAPERBOAT_CERTIFICATES_DISTRIBUTION_CREDENTIAL_REFERENCE` resolves to
   exactly the same bytes loaded through
   `PAPERBOAT_EDGE_CONTROL_CREDENTIAL_FILE`; a mismatch must fail closed.
4. The production overlay must set `PAPERBOAT_CERTIFICATES_ENABLED=true`.
   Never set it to `false` while waiting for a reference, issuance, or
   distribution to recover. A missing reference or a startup error is a
   rollout blocker, not a reason to add a plaintext fallback.
5. Configure `PAPERBOAT_CERTIFICATES_PUBLIC_DNS_ZONE_ID` for the shared preview/tunnel
   base and `PAPERBOAT_CERTIFICATES_DNS_ZONE_ID` for the runtime base and delegated
   challenge zone. The server routes TXT issuance/cleanup and owned address
   publication to their matching authoritative zone; unknown suffixes fail closed.
   Grant the token DNS Edit on these zones, plus Zone Read for deployment discovery.
   Do not add platform `_acme-challenge` CNAMEs or grant DNS credentials to an edge.

### Read-only rollout gates

1. Verify the live server configuration uses the current generated overlay,
   then inspect the certificate worker and distribution-handler health without
   sending a mutation. A `404` or `5xx` on a distribution path, an absent
   worker, or a credential-mismatch startup error fails this gate.
2. Apply baseline migration 1 before starting the serving path, and wait for the
   migration service to finish successfully:

   ```sh
   docker compose --env-file deploy/.env -f deploy/docker-compose.yml \
     up -d migrate
   docker compose --env-file deploy/.env -f deploy/docker-compose.yml \
     wait migrate
   ```

   Start the server and edge only with the platform certificate and private
   distribution path available. From the paperboat-server repository,
   run the deterministic, read-only gate after both services are running:

   ```sh
   ./deploy/certificate-preflight.sh \
     --env-file deploy/.env \
     --compose-file deploy/docker-compose.yml \
     --wait-seconds 300
   ```

   The gate waits for baseline migration 1, both durable platform targets
   (`platform_cert_public_v1` and `platform_cert_runtime_v1`), and active edge
   distributions for every current ready edge process. A distribution counts
   only when `edge.state = 'active'` and its observed certificate generation
   matches the active server certificate. Do not admit public preview/tunnel
   traffic until both platform targets and active edge distributions pass.
3. From a read-only database connection, substitute only the two configured
   public base domains for the psql variables below. This query deliberately
   omits ciphertext and private-key columns:

   ```sh
   psql "$PAPERBOAT_DATABASE_DSN" \
     --set=ON_ERROR_STOP=1 \
     --set=preview_base_domain="$PAPERBOAT_PREVIEW_BASE_DOMAIN" \
     --set=runtime_base_domain="$PAPERBOAT_RUNTIME_BASE_DOMAIN" <<'SQL'
   BEGIN READ ONLY;
   SELECT 'platform_target' AS source, id, kind, hostname, desired_state,
          certificate_state, certificate_reference, certificate_expires_at,
          certificate_failure_code, retry_count, next_retry_at
     FROM paperboat.tunnel_platform_certificate_targets
    WHERE hostname IN ('*.' || :'preview_base_domain', '*.' || :'runtime_base_domain')
   ORDER BY id;

   SELECT 'certificate' AS source, target_kind, hostname, leaf_hostname, state,
          strategy, certificate_generation, expires_at, renewal_at,
          failure_code
     FROM paperboat.tunnel_certificate_records
    WHERE hostname IN ('*.' || :'preview_base_domain', '*.' || :'runtime_base_domain')
   ORDER BY 2, 3, 7 DESC;

   SELECT 'distribution' AS source, cert.target_kind, cert.hostname,
          edge.edge_node_id, edge.edge_process_epoch, edge.state,
          edge.observed_certificate_generation, cert.certificate_generation,
          edge.observed_at, edge.failure_code
     FROM paperboat.tunnel_certificate_edge_distributions AS edge
     JOIN paperboat.tunnel_certificate_records AS cert
       ON cert.id = edge.certificate_id
    WHERE cert.hostname IN ('*.' || :'preview_base_domain', '*.' || :'runtime_base_domain')
   ORDER BY 2, 3, 4, 5;

   SELECT domain_id, owner_id, domain_generation, lease_until
     FROM paperboat.tunnel_certificate_issuance_locks
    WHERE lease_until > now()
      AND EXISTS (
            SELECT 1
              FROM paperboat.tunnel_platform_certificate_targets AS target
             WHERE target.id = tunnel_certificate_issuance_locks.domain_id
               AND target.hostname IN ('*.' || :'preview_base_domain', '*.' || :'runtime_base_domain')
          )
   ORDER BY domain_id;
   ROLLBACK;
   SQL
   ```

   Missing parents, non-ready states, expired records, generation mismatches,
   failed rows, or unexpected active locks require stopping the rollout. The
   transaction is read-only; never repair these rows by hand.
4. Exercise the tunnel's native TLS listener without writing state. Resolve
   one synthetic, one-label host under each wildcard and run
   `openssl s_client -verify_return_error` with the matching SNI. Require a
   peer certificate, a valid chain, and a SAN covering that exact one-label
   host; `no peer certificate` or a TLS alert is a failure. Do not capture certificate
   key material. A successful public TLS handshake is the end-to-end broker
   check.

### Rollback

Stop before changing the next service when any reference is missing, the
distribution credential differs from the edge control credential (the
distribution credential must equal the edge control credential byte-for-byte),
either platform wildcard is not active, SQL rows are absent or stale, the
handler is unregistered, or a public TLS probe has no certificate. Keep the previous signed server and
tunnel images, generated configuration, and last-known-good certificate state
active. Use the deployment's signed rollback and journal; do not edit SQL,
DNS, firewall, keys, or certificate volumes by hand.

If composition was incomplete, restore the matching server image/config and
runtime references first. Wait for baseline migration 1, both platform target rows,
and active edge distributions by rerunning `certificate-preflight.sh`; then
verify worker health, edge distribution generations, and both public TLS
probes before reopening the edge rollout. Record only bounded status/error
categories, resource IDs, and timings. Never put secret values, certificate
private keys, or provider tokens in rollback evidence.

## Connector loss, rotation, drain, or revoke

1. Identify the exact connector, credential generation, session, process
   generation, and aggregate operation.
2. During rotation, keep the old credential only for the configured overlap.
   Complete the aggregate operation only after every captured target proves and
   becomes ready with the new credential generation.
3. During drain, stop new streams and let existing streams finish within the
   bounded deadline. Reject acknowledgements from a replaced session or wrong
   operation ID.
4. During revoke, preserve `revoked` and `forced_closed` terminal state even if
   a late drain acknowledgement arrives.
5. A final connector going offline does not delete the tunnel, route, domain,
   DNS target, or certificate.

## Private-access failure

Private traffic must follow:

```text
browser -> hostd-owned PAC/CONNECT proxy -> authenticated carrier -> edge -> origin
```

No browser cookie, redirect, login flow, extension, or public request header is
an access credential. Interpret `401` as missing or invalid current machine
authentication, `403` as an authenticated but denied or revoked route binding,
and `503` as temporary hostd, carrier, edge-binding, or authorization-authority
unavailability. Confirm the exact account, route, machine session, connector,
configuration, assignment, edge node, and process epoch before retrying. Keep
denial responses non-enumerating.

## Control-plane or database outage

1. Stop new mutations and preserve established last-known-good traffic.
2. Restore PostgreSQL and the control worker from the current compatible
   release. Do not edit operation, snapshot, or assignment rows by hand.
3. Resume with the original operation and idempotency key. Duplicate delivery
   must converge to the same durable result.
4. Wait for a complete snapshot and exact acknowledgements. Reject stale reads,
   deltas, or observations that cannot prove the current generation.
5. Verify preview expiry, durable route continuity, certificate distribution,
   connector recovery, and private-access fail-closed behavior before reopening
   mutations.

## Update rollback

Stop rollout when a candidate fails signature, compatibility, process health,
route generation, origin readiness, edge canary, or certificate checks. Keep
the previous signed release and last-known-good runtime active, let the update
journal drive rollback, and quarantine the failed version. Do not replace the
release selector manually. Reopen rollout only after the signed manifest,
deployment plan, runtime verifier, server readiness check, and rollback test
agree.

## Closure

Close the incident only when typed health has recovered, the durable operation
is terminal, desired and observed generations agree, old sessions are fenced,
bounded retry queues are draining, and no secret entered logs or evidence. For
user-visible diagnostics, follow the `pb tunnel doctor` support-bundle flow and
review its redaction manifest before upload.

### Lazy private/team port sharing

`PUT /v1/lazy-policies` reserves an exact loopback HTTP, HTTPS or h2c target for an
owned machine. Supply `machine_id`, `target` (`scheme` of `http`, `https` or `h2c`, and a loopback `address`),
`access_mode` (`private` or `team`), `ownership_mode` (`persistent_port`) and `expires_at`;
`expected_generation: 0` creates a policy. Updates supply its
`id` and current `expected_generation`. Port sharing follows replacement listeners
at the same approved address. It does not start applications or wake machines.

The response contains the stable hostname and policy generation. Owner reads use
`GET /v1/lazy-policies/{policy_id}`; revocation uses `DELETE` with
`{"expected_generation": N}`. Revocation tombstones the hostname permanently.
Expiry stops permission while retaining the reservation for owner inspection and
reapproval. Re-enrollment requires owner reapproval of the new installation.
Team use additionally requires an explicit `lazy_policy` binding and `use` grant
through the existing team API. Membership or URL knowledge alone is insufficient.
Scoped machine credentials use resource kind `lazy_policy` and the policy ID for
both `resource_id` and `route_id`.

Authenticated first requests coalesce at the server before any application body
is read. Only the current daemon registration may receive the exact-target lease.
Readiness uses a bounded `HEAD /` against the exact origin; protocol failures and
5xx responses are unavailable, and redirects are not followed. The existing
ephemeral preview engine owns forwarding and cleanup: five minutes
with zero streams retires forwarding, eight hours caps an instance (earlier policy
or credential deadlines win), and streams are capped at one hour and current
authorization. A later authorized request can reactivate the reserved name.
Neither traffic nor a listening application extends expired authority.

Activation is bounded to ten seconds, with a three-second origin connection
budget, four concurrent activations per environment, sixteen per account and
thirty-two waiters per activation. Admission exhaustion returns 429 with
`Retry-After: 1`. Authenticated failures distinguish `host_offline`,
`origin_unavailable`, `activation_timeout`, `generation_conflict` and
`forwarding_failed`. No partially forwarded request is replayed automatically.

Browser access does not depend on the public suffix list. Edge cookies are
host-only `__Host-` cookies (Secure, Path=/, no Domain), and the edge refuses
cookie-carrying requests that a sibling preview/tunnel host or another site
started other than as a top-level document navigation (Sec-Fetch-Site). Listing
the base domain on the PSL later adds browser-level isolation and needs no code
change. These APIs do not provision DNS/TLS. Enable it on an installation with
`pbh install --browser-access`. The console the browser signs in at is the
server's `PAPERBOAT_BROWSER_LOGIN_ORIGIN` (first origin): the server returns it
on every begin and accepts a handoff only from it, so an edge has no console
address of its own to get wrong.

### Shared TCP443 TLS pass-through

HTTP termination and TLS pass-through use one public TCP443 listener. TLS route
bindings contain `public_tcp_listener_id: listener_tls_443` and `public_tcp_port: 443`;
raw TCP keeps its allocated port range. Origins own pass-through certificates and
application authentication. Publish a hostname in only one mode, including preview
and custom-domain aliases and overlapping one-label wildcard HTTP names.

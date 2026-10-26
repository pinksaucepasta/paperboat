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
points at the resource's own managed name. Edges do not forward, so a route is reported ready only when every live
edge serves it. When an edge dies or is drained for good, remove its A (and AAAA) record from both wildcards by hand:
the wildcards are static, so until you do, about half of new connections go to the dead edge.

## Public TCP tunnel ports on the managed edges

Raw TCP tunnels get a port from 20000-29999 on every managed edge, and the edge listens only on ports owned by a
live public TCP tunnel. Both edge hosts therefore accept inbound TCP 20000-29999 (iptables rule
`paperboat-public-tcp-tunnels`, inserted before the final `REJECT`, persisted in `/etc/iptables/rules.v4`; the
previous file is kept as `rules.v4.bak-publictcp`). The cloud security lists already allow these ports; nothing
else was opened. Re-check from outside the cloud (any host that is neither edge) with a live tunnel port:
`nc -vz <edge-ip> <port>` succeeds for a listening tunnel port, answers `Connection refused` for an unused port
inside the range and `No route to host` outside it (30000), which is the host `REJECT`. A timeout instead means a
security list or the host rule is missing. If `pbh install` or a reinstall resets iptables, re-apply the rule and
`iptables-restore --test < /etc/iptables/rules.v4`. Verified 2026-10-26 from the Hetzner control host against
140.245.11.41 and 152.67.0.60 (port 20000 open, 25000 and 29999 refused, 30000 no route).

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
paperboat-tunnel image. Production requires
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
`pbh install --browser-login-origin https://<console origin>`.

### Shared TCP443 TLS pass-through

HTTP termination and TLS pass-through use one public TCP443 listener. TLS route
bindings contain `public_tcp_listener_id: listener_tls_443` and `public_tcp_port: 443`;
raw TCP keeps its allocated port range. Origins own pass-through certificates and
application authentication. Publish a hostname in only one mode, including preview
and custom-domain aliases and overlapping one-label wildcard HTTP names.

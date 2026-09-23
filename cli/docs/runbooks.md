# Paperboat CLI Integration Runbooks

Capture only timestamps and stable request, project, environment, endpoint, intent, and operation
IDs. Never capture codes, tokens, URLs containing credentials, terminal output,
file/preview bytes, private keys, candidate addresses, or local/machine paths.

## WorkOS outage

Detection: dashboard sign-in or enrollment fails while existing client sessions
remain otherwise healthy.

1. Stop repeated dashboard sign-in or enrollment attempts that could amplify the outage; honor server retry hints.
2. Confirm the failure is isolated to browser identity rather than Paperboat API reachability.
3. Keep existing sessions operating until their normal expiry; do not bypass WorkOS.
4. After recovery, enroll using a fresh dashboard command and verify invalid and expired enrollment tokens are rejected.

## Signing-key rotation or rollback

Detection: runtime or endpoint-certificate verification rejects an otherwise authorized environment with
an unknown key or signature error.

1. Confirm the active `kid`, issuer, audience, and configured JWKS overlap without recording proofs.
2. Keep the previous public key published during the configured overlap window.
3. Roll back the active signing key if new proofs fail while old-key verification succeeds.
4. Verify one new connect, one old-key overlap verification, and revocation before retiring the old key.

## Tunnel outage

Detection: control-plane authorization succeeds but route readiness or WSS/HTTPS
dialing fails across projects.

1. Use `pb doctor <environment>` to distinguish route readiness from host-runtime health.
2. Stop reconnect storms and honor configured retry bounds.
3. Do not expose a machine port or bypass Paperboat with raw SSH. `pb ssh` is valid only when
   its byte stream succeeds through the same direct/relay/WSS transport policy.
4. After recovery, verify terminal attach, reconnect, resumable file transfer, and revoked-route rejection.

## Local daemon unavailable

Detection: `pb status` or `pb wait` cannot open the owner-only local API after its bounded
lazy-start attempt.

1. Verify the socket path and parent directory are owned by the current user and are not
   symlinks or group/world writable. Do not delete an unfamiliar socket or lock file.
2. Check `systemctl --user status paperboatd.service` on Linux or
   `launchctl print gui/<uid>/com.pinksaucepasta.paperboatd` on macOS.
3. If the service is active, preserve its typed health state and inspect only bounded,
   redacted service diagnostics. Do not bypass the local API with direct control-plane
   polling.
4. Retry `pb status`; it may repair a missing definition only when the socket is absent or
   refusing connections. Permission, protocol, and invalid-state failures require fixing
   ownership or upgrading `pb`, not repeated reinstall attempts.
5. Verify one snapshot read, one watch transition, service restart, stale-socket recovery,
   and `pb uninstall` unloading the service before state removal.

## Fly start or machine failure

Detection: readiness remains in a machine-starting state, reports machine failure,
or times out before route/runtime checks.

1. Correlate the project and machine lifecycle event in the control plane.
2. Confirm entitlement, credits, volume attachment, image identity, and runtime health.
3. Avoid repeated replacement while volume ownership is uncertain.
4. After recovery, run `pb doctor <project>`, attach once, and verify the persistent workspace.

## Host-runtime authorization mismatch

Detection: route and runtime are healthy but mint, token exchange, WebSocket ticket,
terminal scope, or `file:transfer` scope is rejected.

1. Compare issuer, environment ID, owner ID, audience, scope, and clock configuration.
2. Revoke the affected downstream sessions; never broaden a credential scope to diagnose.
3. Reconcile the machine endpoint identity and host generation, then broker a new descriptor.
4. Verify terminal-only credentials cannot transfer files and file-only credentials cannot attach.

## Endpoint or account-root key compromise

1. Stop new private operations and record only endpoint IDs, certificate fingerprints, generations,
   and timestamps. Never export private key material for diagnosis.
2. For one endpoint, revoke its certificate, advance authorization state, remove its local endpoint
   state through the supported logout/unpair flow, and re-enroll it under the existing account root.
3. For account-root loss or suspected compromise, revoke every endpoint certificate, advance the
   account authorization generation, complete the explicit root reset/recovery flow, and re-pair
   every CLI and machine. The server must not fabricate or escrow a replacement root.
4. Verify old certificates, descriptors, relay admissions, and encrypted transfer resources fail;
   then verify one newly paired terminal and file transfer on the new generations.

## Stuck device grant

Detection: polling remains pending beyond the authoritative expiry or an approved
grant cannot be consumed exactly once.

1. Stop polling at expiry and preserve no device code locally.
2. Check grant state transitions and rate-limit events by hashed grant/network identifiers.
3. Expire or deny the grant through the server-owned operation; do not issue tokens manually.
4. Verify a new flow succeeds and the old code remains unusable.

## Stolen device

1. Revoke the device's client session from the dashboard immediately.
2. Verify the Paperboat token family, endpoint certificate, runtime sessions, and tunnel access
   are revoked within the configured bound.
3. Run `pb auth logout` on the device if recovered so queued local cleanup completes.
4. Review metadata-only access events; rotate unrelated account credentials only if evidence warrants it.

## File-transfer cleanup failure

Detection: staged or pending files exceed configured age/space bounds or cleanup
reports repeated failures.

1. Stop accepting transfers if storage safety limits are threatened; terminal access remains separately scoped.
2. Inspect counts, sizes, ages, and environment IDs only, never file contents or source paths.
3. Restore the host-runtime cleanup worker and run its idempotent cleanup operation.
4. Verify expired files are gone, active files remain readable, traversal is rejected, and new transfers obey retention.

## Served preview workload drift

Detection: an active preview has no ready carrier or origin, its owner session expired, its
route generation is stale, or its source identity is invalid.

1. Identify the preview, machine, operation, connector session, and config generation
   without recording source paths, public URLs, request data, or credentials.
2. Use `pb preview stop <preview>` to converge server and host-runtime state. Do not remove
   only the route or terminate only the carrier.
3. For a replaced or escaping source, keep the preview stopped and have the owner select
   the intended target again. Never weaken the stored source identity check.
4. If cleanup was interrupted, let hostd reconcile the terminal lease before removing any
   orphaned local owner session.
5. Verify no route, owner session, carrier registration, credential-renewal loop, or private
   proxy rule remains for the stopped preview.

## Recovery evidence

For every incident, record the redacted timeline, affected stable IDs, configured
version/protocol, root cause, containment, recovery verification, and whether
alerts or thresholds need adjustment. Exercise these runbooks against a
production-shaped environment before release.

Preview, durable tunnel, custom-domain, connector, private-access, and update
procedures are collected in
[runbooks-preview-tunnels.md](runbooks-preview-tunnels.md).

## One support reference and optional Sentry

Each CLI invocation carries one `pb-` plus 32 lowercase hexadecimal support reference.
The same reference crosses PB control API requests, local daemon HTTP IPC and daemon RPC;
human and JSON errors show this reference, while request IDs remain internal. Keep the
reference and the safe error explanation when contacting support. It is not a credential.

Official release binaries enable sanitized error, log, trace and metric reporting using
release-injected defaults. Set `PB_SENTRY_ENABLED=false` to disable it. Ordinary source
builds have no default destination: explicitly set `PB_SENTRY_ENABLED=true`, your own
`PB_SENTRY_DSN` (HTTPS, without a query or fragment), and `PB_SENTRY_RELEASE` (the exact
artifact revision) to enable reporting. Runtime DSN/release settings override defaults.
Build defaults prevent accidental reporting by source installations; they do not prove
binary origin or stop deliberate spoofing. Treat client reports as untrusted diagnostics. No
terminal/file/ENV payload, command arguments, raw error text, request data, URLs or log
stream is submitted. Reports contain approved classification/component/reference/release
and sanitized code frames. Expected cancellations, input and authorization outcomes are
filtered. Reporting has an eight-event queue, three-event/minute process budget and a
two-second send/flush bound. Flush is not proof of Sentry acceptance.

With reporting disabled or the network offline, unexpected CLI failures/panics make a
best-effort, 250 ms local diagnostic-marker request to the running daemon. Its existing
50 MiB, seven-day diagnostic ring retains the same reference and the standard safe
bugreport export includes it. If the daemon/local IPC is also unavailable, the terminal
or JSON error remains available but the reference has no guaranteed local retained
record. Keep that output and reproduction details; do not infer that a report was sent.
The CLI does not write concurrently into the daemon-owned ring or silently upload raw logs.

PB staff can look up the one reference through the server's audited support API. Sentry
project access, retention and deletion are deployment/operator responsibilities. Enabling
reporting does not grant terminal/file access or weaken native end-to-end encryption.

# Paperboat native peer contracts v1

Paperboat ships one private-network implementation: the upstream Tailcat/Tailscale
WireGuard data plane with Paperboat control-plane authorization. JSON contracts are
closed world, field names are snake case, timestamps are UTC RFC 3339, and binary
values use canonical unpadded base64url. Unknown fields, rollback, expired authority,
and binding mismatches fail closed.

## Endpoint identity

Each enrolled CLI and machine owns its private keys. The control plane stores signed
public endpoint certificates and revocation state. Machine enrollment is confirmed by
the user-visible safety code before the CLI signs the machine certificate. Private keys
never enter control-plane requests, database records, logs, or audit events.

## Network authorization

The control plane issues a short-lived signed network configuration for the exact
account, endpoint, machine generation, WireGuard key, virtual address, relay inventory,
and application capabilities. The endpoint verifies the signature and monotonic
generation before installing it. WireGuard reachability does not itself authorize an
application operation.

The native relay uses the separately scoped `paperboat-relay-grant+jwt` admission
contract. Relay registration, generations, revocation floors, bounds, and drain state
remain controlled by the authenticated node lifecycle. The relay never stores private
file payloads.

## Application authorization

QUIC authenticates the approved Ed25519 endpoint key. Each terminal, SSH, transfer,
preview, private-access, or Codex stream also carries a short-lived credential bound to
its exact operation, consumer, resource, deadline, and byte limit. Reconnect obtains
current authority and resumes through the owning application protocol; it never replays
commands implicitly.

File transfer uses the native operation stream and validates chunk and whole-file
SHA-256 digests. It has no transfer-key delivery protocol, content-key envelope, or
application AEAD layer.

## Managed SSH

Client public keys, machine targets, and observed host keys are generation-fenced.
Changed host keys remain pending until explicitly promoted. Readiness requires a current
client key, target, and approved host-key set. Audit records contain identifiers and
fingerprints, never private keys or session content.

## Stable failures

Authentication, authorization, identity, protocol, revocation, generation, malformed
response, and integrity errors are terminal. Availability and transport interruption are
retryable only through a fresh current-authority check.

# Contract Pack

Status: implemented v1 contract set, pending the final paid-product gate.

These docs capture the initial contract work for `paperboat-server`.
They define the implementation baseline for later releases while keeping environment-
specific provider values in dynamic configuration and database catalogs.

## Documents

- [decisions.md](decisions.md) - current contract decisions.
- [http-api.md](http-api.md) - Paperboat server HTTP API and JSON response contract.
- [provider-contracts.md](provider-contracts.md) - WorkOS, Stripe, GitHub, and
  dynamic catalog boundaries.
- [cli-authorization.md](cli-authorization.md) - device authorization grants, scoped CLI sessions,
  refresh rotation, and client revocation.
- [private-access.md](private-access.md) - stable-host machine proof, route-bound
  grants, edge authorization, generation fencing, and fail-closed HTTP behavior.
- [config-sync.md](config-sync.md) - configuration repository connections, machine
  assignment, writer leases, conflict actions, and status revisions.

## Approval Rule

Contract changes after this baseline require explicit approval. Production provider
values and connected verification remain release evidence, not code defaults. Values that can change after launch must remain dynamic
configuration or database records.

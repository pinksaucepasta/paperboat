# HTTP API Contract

Status: implemented contract baseline.

This document freezes the `paperboat-server` HTTP/JSON contract shape for production
implementation. Endpoint behavior remains server-side authoritative; clients are
untrusted.

## General Rules

- JSON field names use `snake_case`.
- IDs are opaque strings. Ordinary IDs use a full singular noun
  followed by `_` and canonical lowercase UUIDv4, with no legacy format alternatives.
  Entity type and authorization come from fields and authoritative resource state,
  never a parsed prefix. See [identifier decisions](decisions.md#identifiers-and-naming)
  for cryptographic formats and the coordinated rollout boundary.
- State enums are lowercase strings.
- Mutations that create provider resources require idempotency keys.
- Billing-impacting and irreversible mutations write audit events.
- Provider credentials, tokens, SSH keys, API keys, and secrets are never returned.
- `paperboat-server` does not proxy live terminal, preview, or WebSocket traffic.

## Response Shape

Success responses:

```json
{
  "data": {}
}
```

Error responses:

```json
{
  "error": {
    "code": "machine_not_ready",
    "message": "Machine is not ready for connection.",
    "support_reference": "support_550e8400-e29b-41d4-a716-446655440000",
    "request_id": "req_...",
    "details": {}
  }
}
```

## Cross-Cutting Headers

Requests:

- `Authorization: Bearer <token>` for non-browser clients where approved.
- Cookie session plus CSRF header for dashboard browser writes.
- `Idempotency-Key` where the operation contract requires it, including machine setup,
  checkout/customer portal, configuration repository provisioning and resource mutations.
- Resource ETags or explicit expected versions/generations for optimistic concurrency,
  as defined by each operation in [OpenAPI](../openapi.json).

Responses:

- `Request-Id` on every response.
- Rate limit headers where limits apply.

## Endpoint List

### Health

- `GET /.well-known/jwks.json`
- `GET /v1/client-configuration`
- `GET /healthz`
- `GET /readyz`

### Auth and Session

- `GET /v1/me`
- `POST /v1/auth/workos/callback`
- `POST /v1/auth/logout`
- `GET /v1/auth/csrf`
- `POST /v1/auth/device/authorize`
- `POST /v1/auth/device/token`
- `GET /v1/auth/device/requests/{user_code}`
- `POST /v1/auth/device/requests/{user_code}/approve`
- `POST /v1/auth/device/requests/{user_code}/deny`
- `POST /v1/auth/token/refresh`
- `POST /v1/auth/token/revoke`
- `GET /v1/auth/cli-client-sessions`
- `DELETE /v1/auth/cli-client-sessions/{cli_client_session_id}`

The machine/session contract, polling outcomes, rotation, and revocation behavior are frozen
in [cli-authorization.md](cli-authorization.md). Browser approval uses cookie plus CSRF;
CLI machine reads and connects use scoped Paperboat bearer access tokens. Revoking a CLI login session fences ENV delivery without requiring personal or Team ENV key rotation.

### Selected workspace

`GET /v1/workspaces` returns `{items:[{id,name,kind,role}]}` in the normal data
envelope: Personal first (`id: "personal"`), then active, nonsuspended team memberships.
`personal` is reserved and cannot be a team ID. Selection uses one identity; it never
changes credentials, enrolls a machine again, or transfers existing resources.

User machine/resource reads and management requests carry `?workspace=personal` or an
exact team slug. Unknown or revoked selections fail; they do not fall back to Personal
or borrow a grant from another team. Signed preview/tunnel creation also includes the
same `workspace` in its signed JSON body. Query-only selection and mismatched body/query
are rejected. Internal machine lifecycle requests without a user selection remain bound
to their exact authorized resource.

Previews, tunnels and terminal sessions persist nullable `owner_team_id`: null means
Personal, and a team ID fixes ownership independently of publication audience and host
ownership. Ordinary updates cannot transfer ownership. Switching changes new work and
selected inventory; running resources keep their ownership and lifecycle. Team membership
alone never grants remote execution. Exact selected-team machine capabilities are required;
a personally owned signed host can publish its own service to its selected team using its
existing host capability, without granting that team general access to the machine.
Team publication also requires the team's active subscription. Preview and tunnel
creation reject an unpaid team with `403 team_subscription_required`, outcome
`unchanged`, and recovery action `manage_team_billing` before allocating a resource
or operation. The signed proof uses the canonical URL path; the signed JSON body
binds workspace selection and must agree with the query selector.

A team terminal descriptor requests the selected team's terminal capability alone.
Personal ownership of its host does not add file-transfer or private-access authority
inside team scope. `file_transfer` is optional in a terminal descriptor; clients validate
it when present, and shared viewer/controller descriptors cannot include it. Personal
descriptors retain their existing explicitly eligible bundled capabilities.

ENV scope inventories and explicit key-management targets follow the selected workspace;
a conflicting explicit target returns `409 workspace_target_conflict`. Machine-bound ENV
host configuration retains its complete, explicitly granted multi-source selection, so
changing a workspace never filters and replaces another source implicitly. Configuration
status and adoption management follow the assignment's existing `adopted_team_id`; team
default operations require that exact selected team. Repository credentials and GitHub
inventory remain owned by the authenticated identity. Selecting a team does not share
those credentials, adopt a default, or apply configuration to a host.

### Personal and team subscriptions

- `GET /v1/billing/workspaces/personal`
- `GET /v1/billing/workspaces/personal/products`
- `POST /v1/billing/workspaces/personal/checkout`
- `POST /v1/billing/workspaces/personal/portal`
- `GET /v1/billing/workspaces/teams/{team_id}`
- `GET /v1/billing/workspaces/teams/{team_id}/products`
- `POST /v1/billing/workspaces/teams/{team_id}/checkout`
- `POST /v1/billing/workspaces/teams/{team_id}/portal`
- `GET /v1/billing/workspaces/teams/{team_id}/seats/preview?quantity=N`
- `POST /v1/billing/workspaces/teams/{team_id}/seats`

Product discovery uses `limit` (1–200, default 50) and `offset`, returning
`{items,pagination:{limit,offset,total,next_offset}}`. Each product includes its
catalog type, version and verified `unit_amount` in currency minor units,
`currency` and recurring `interval`. Unverified price fields are null and cannot
be selected for Checkout. Workspace overview also includes the current product.
The client displays price and affected Personal or Team scope before calling
Checkout; Team prices are per purchased seat, with seats_bought and seats_used shown separately.
Checkout requires `product_version` alongside the selected product code and
rejects a changed catalog entry with `409 product_changed` before a provider
operation, so the client reloads prices for review. Taxes and the final total
are confirmed by the provider. Return URLs do not establish payment success;
only the verified workspace subscription projection does.

See [provider contracts](provider-contracts.md) for ownership, quantities and recovery.

### Teams

Removing a device revokes that device and stops all future ENV delivery to it, including already-connected sessions. It does not require ENV key rotation or change the personal or Team key epoch. The removed device may keep the old key and any ENV it already downloaded. Rotating ENV keys is an optional Owner/Admin action for lost or compromised devices. Member removal and ENV grant revocation still require rotation.

- `GET /v1/teams/{team_id}` returns the current team state, including `env_status` as
  `not_initialized`, `ready`, `rotation_pending`, or `deleted`. A role never grants ENV
  decryption by itself. Member removal and ENV grant revocation can set `rotation_pending`; device removal cannot.
- `GET /v1/teams/{team_id}/activity?cursor={opaque}&limit={integer}` returns
  `{items,next_cursor}`. Each item contains `id`, `actor_account`, `action`, `created_at`,
  and allowlisted `metadata`; `action` omits the stored `team.` prefix. `limit` defaults
  to 50 and is capped at 200. The cursor is opaque and scoped to the selected team.
- Every activity page rechecks current membership and permits only the active owner or an
  active admin. Removed members, ordinary members, and deleted teams cannot read activity.
  Activity remains visible for 90 days and expired rows are physically removed in bounded
  batches. Payload contents, ENV secrets, terminal contents, keystrokes, and file contents
  are excluded from activity metadata.

The CLI surfaces these contracts through `pb team get` (`env_status`) and
`pb team activity TEAM [--limit N] [--cursor CURSOR] [--json]`. The dashboard exposes
activity only to current owners/admins and shows actionable ENV rotation-pending state.
Lifecycle details are in the CLI [`team-lifecycle.md`](../../../cli/docs/team-lifecycle.md)
guide.

### Configuration Sync

User-edited configuration sources are TOML: shared `.paperboat/config-sync.toml`
and the machine file reported by `pb config sync path`. Empty or comment-only machine
files inherit shared defaults. These source files are not HTTP payloads; config
assignment, approval, and runtime API bodies remain JSON.

- `POST /v1/config-repositories` connects `provider=git` using `external_ref` as an
  explicit Git endpoint, `display_name` and optional `branch` (default `main`). The
  server stores an opaque authoritative repository identity and makes no outbound
  probes. Custom credentials remain on machines; GitHub connection behavior is unchanged.
  Repository access grants expose `transport`; custom grants have no provider password.

- `GET /v1/config-sync/status` requires an authenticated account with an active entitlement and
  returns the authoritative policy plus bounded environment, assignment, consent, helper,
  repository, revision, conflict, and recovery state. Stale helper observations are `offline`;
  unassigned environments are `disabled`. Policy reports the server rollout mode
  (`disabled`, `read_only`, or `leased_writes`) for every machine.
- `PUT /v1/machines/{machine_id}/config-assignment` accepts independent
  `pull_repository_id` and `push_repository_id` targets plus explicit `automatic_updates`,
  `path_rules` and `repository_bindings` as defined in [config-sync.md](config-sync.md).
  Both arrays are returned on assignments and runtime descriptors; absent/empty rules
  mean no managed files. The runtime also returns `assignment_version`.
  Changing only update policy preserves the active personal assignment identity and sync baseline;
  version fencing, renewed consent, approval clearing, and credential revocation still apply.
  Changing targets, path rules, repository bindings, mode, warning revision, adoption, or re-enabling a revoked assignment creates
  a new identity. Personal targets belong to the authenticated account. `POST .../approve` binds approval
  to the exact currently observed pull revision and assignment version; stale revisions fail.
- `GET|PUT|DELETE /v1/teams/{team_id}/config-default` exposes the owner/admin-managed
  default pull repository identity. `GET|PUT|DELETE /v1/config-sync/team-default-adoption`
  exposes the current account's single explicit adoption. Adoption succeeds only when the
  active member has independently connected the same provider repository. Defaults never
  contain credentials or push targets, and PB membership changes do not revoke provider access.
- `GET|PUT|DELETE /v1/config-sync/overrides` lists and changes exact account-path overrides.
  Mandatory exclusions return `mandatory_exclusion` and cannot be weakened.
- `POST /v1/config-sync/recovery-key/export` and `/rotate` require CSRF plus a short-lived,
  purpose-bound WorkOS reauthentication proof. Export responses are non-cacheable and audited.
- `POST /v1/config/classify` requires helper proof plus a current assignment-bound config
  credential and accepts only the versioned, bounded relative-path metadata schema. Provider API
  keys, contents, absolute paths, identities, repository coordinates, and workspace names are
  never accepted from or returned to a helper.
- `POST /v1/config-sync/environments/{environment_id}/conflict-resolutions` accepts
  native user bearer authentication with `machines:connect`; browser sessions still
  require a trusted Origin and CSRF. The adjacent `/force` route uses the same
  authentication boundary and requires explicit force confirmation. Resolution records an
  explicit `keep_local`, `keep_remote`, or `externally_resolved` action against the current
  assignment version, remote revision, relative path, and deterministic conflict revision.
  Stale or ambiguous actions return a conflict.
- `POST /v1/config/conflict-resolutions/pending` and
  `/v1/config/conflict-resolutions/acknowledge` require helper proof and the current
  assignment-bound config credential. A helper sees only its active binding and acknowledges
  only after the selected result lands.
- `POST /v1/runtime-observations` accepts authenticated runtime availability and diagnostics
  plus an optional validated `config_sync` object. Its required `updated_at` timestamp tracks the
  freshness of the sync daemon independently. A status timestamp newer
  than its enclosing sample is persisted as a sanitized `status_clock_invalid` error at sample time,
  allowing later clock-corrected status to replace it. Paths and errors are sanitized and bounded;
  file contents, credentials, and raw command output are never accepted or persisted.
- The same endpoint accepts a join-only `terminal_join` observation containing
  `access_session_id`, `terminal_session_id`, and `attachment_id` alongside the required
  `environment_id`, `resource_id`, `reporter_version`, and `sampled_at` envelope fields.
  `environment_id`, `resource_id`, and `sampled_at` are required; the native runtime also
  sends its `reporter_version`. The observation requires
  the current signed machine proof and exact current team, membership, binding, grant, access,
  and terminal-role generations. Success returns HTTP 202 with `{data:{recorded:true}}`.
  Malformed or mixed observations return 400, invalid current terminal access returns 403,
  and unavailable durable audit recording returns 503. This metadata-only event is distinct
  from `terminal_session_access_authorized` and never records terminal contents.
- An initial environment observation may accompany runtime diagnostics before
  `environment_injection` is advertised. It must be `pending` with null authority,
  global, and machine cursors and every required observation member. Normal machine
  proof and environment binding validation still apply; the capability is advertised
  only after the host verifies its encrypted bundle.

### Signed Updates and Maintenance

- The native installation and signed-release matrix is exactly macOS (`darwin`) ARM64,
  Linux AMD64 and ARM64, and Windows AMD64 and ARM64. Darwin AMD64 descriptors,
  release indexes, and release-authority requests are rejected. Machine setup has only
  `host` and `client` modes; `client` is the receive-side runtime.
- First installation and update decisions use a TUF-verified
  `paperboat.release-index/v1` target. The signed index carries the artifact digest and length,
  platform and architecture, worker/supervisor compatibility bounds, minimum version, revocation,
  and a deterministic rollout policy.
- Cohort eligibility is stable for `(cohort_seed, machine_id)`, is evaluated locally from the
  signed policy, and fails closed for revoked releases, unsupported targets, invalid windows, and
  machines outside the signed percentage. The control plane does not accept a mutable cohort
  override from a client.
- `POST /v1/runtime-observations` accepts an optional `update` object with schema
  `paperboat.update-observation/v1`. The server authenticates it with the machine proof and
  fences late or replayed state using installation generation, worker generation, operation ID,
  boot ID, timestamp, and a payload hash. Stale observations never replace newer status.
  Authenticated runtime presence is recorded independently from optional availability and update
  observations. A stale, invalid, or conflicting auxiliary observation is omitted from durable
  status and reported by stable code in the accepted response's `auxiliary_rejections` list; it
  never makes a live runtime appear offline.
- `GET /v1/machines/{machine_id}/update-status` returns the latest durable update observation.
- `GET /v1/machines/update-summary` returns a fleet page for the selected `workspace` (Personal by default) and complete filtered state counts. It accepts `limit` (1–200, default50), `offset`, literal case-insensitive `q`, and update `state` (including `not_reporting`); `pagination` supplies `total` and nullable `next_offset`. Team selection uses current membership and the same machine management/capability visibility as machine inventory; Personal excludes team-owned and foreign machines. Machines that have
  not reported update state are explicitly counted as `not_reporting`; they are never folded into
  healthy or idle results.
- `GET|POST /v1/machines/{machine_id}/maintenance-approvals` lists or requests an explicit,
  owner-scoped approval. Requests require `Idempotency-Key`, bind to one target version and
  action, and expire within 24 hours. `POST .../{approval_id}/approve` and `/reject` are
  optimistic, audited state transitions. Approval never bypasses TUF verification or local
  compatibility checks.
  GET accepts `limit` (1–200, default50), `offset`, literal `q`, and `state` (pending/approved/rejected/expired/consumed). The response includes `approvals` and `pagination` with complete filtered `total` and nullable `next_offset`, ordered by creation time and ID descending. Due pending approvals expire before counts and page reads; exact machine management authority is checked before this transition.

### GitHub

- `GET /v1/github/status`
- `POST /v1/github/oauth/start`
- `POST /v1/github/oauth/callback`
- `POST /v1/github/config-repositories/provision`

Expiring GitHub user access tokens renew using the stored encrypted refresh token.
Renewal serializes with reconnect and revocation, rotates both credentials, and
never retries the provider operation inside a database transaction. Temporary
provider failures preserve credentials for retry; an invalid or expired refresh
credential reports disconnected and requires reconnecting GitHub.

### Machines and native access

- `GET /v1/machines`
- `POST /v1/machines/setup`
- `PUT /v1/machines/{machine_id}/capabilities`
- `POST /v1/machines/{machine_id}/connection-descriptor`
- `GET /v1/machines/{machine_id}/connection-readiness`
- `GET|POST /v1/machines/{machine_id}/terminal-sessions`

Machine terminal descriptors support authenticated CLI access to the same enrolled
source and target machine. Create-and-connect accepts
`create_session: {name, idempotency_key, cwd?}`; an explicit launch directory requires
an owned source matching the target and a canonical absolute target-platform path
of at most 1024 UTF-8 bytes. Omission uses the enrolled root. Idempotent replay never
changes the original session or directory, and launch failure never falls back.

Every enrolled machine can initiate authorized access and serve its enabled capabilities.
Incoming terminal/exec, managed SSH, file receiving and preview/tunnel serving default
on; peer relay is opt-in. Disabling an incoming capability does not disable outgoing
access. Capabilities never replace exact-resource authorization, revocation or readiness.

### Preview Leases

- `POST /v1/previews` creates one durable `preview_lease` plus a resumable create
  operation. A dashboard request may select any currently online owner machine in the
  account. A request made by the owner host must carry its renewable machine identity
  and exact request proof; the server derives the machine actor from that proof and
  does not use a CLI client-session ID as a machine identity. Every 202 operation and
  exact 200 replay also returns `X-Paperboat-Operation-ID` with the durable server
  create operation ID. Hosts use this safe header to resume local carrier work; the
  operation ID is never added to the preview resource. The response is an operation
  while dispatch or readiness is pending; it never publishes a usable preview before
  readiness.
  The request requires `owner_session_kind`: `local_lease` for a live hostd-issued
  owner lease or `foreground` for the selected runtime lifetime. The server stores
  the kind unchanged and signs it with dispatch; callers cannot select the internal
  `lazy_runtime` kind. Session IDs do not select lifecycle behavior.
  The request always includes `domains`, which may be an empty array and may
  contain at most eight normalized exact, apex, or one-label wildcard hostnames.
  An account selecting a self-hosted-only tunnel pool must supply a custom domain;
  an empty domain list returns `400 preview_domain_required`, changes no state,
  and directs the caller to `configure_preview_domain`.
  Domain rows are created in the same transaction as the lease. The managed
  Paperboat preview endpoint remains immediately usable while DNS and TLS for
  custom aliases reconcile independently.
- `GET|POST /v1/previews/{preview_id}/domains`,
  `GET|DELETE /v1/previews/{preview_id}/domains/{domain_id}`,
  `POST /v1/previews/{preview_id}/domains/{domain_id}/verify`, and
  `GET /v1/previews/{preview_id}/domains/{domain_id}/instructions` expose the
  account-scoped domain lifecycle. Mutations require idempotency and, after
  creation, the exact domain ETag. A binding targets only the preview lease and
  never creates a durable tunnel or route. Lease renewal advances the alias
  target generation atomically; stop, expiry, or owner loss withdraws aliases
  immediately while retaining user-owned DNS and enforcing the reuse quarantine.
- Every `preview_lease` response includes a bounded `domains` summary array. It
  contains no tunnel or route identifiers. Edge admission publishes only DNS-
  verified, certificate-ready aliases for the exact preview generation.
- `POST /v1/previews/{preview_id}/readiness` is machine-authenticated only. It requires
  `Authorization: Bearer <machine-identity>`, the same value in
  `X-Paperboat-Machine-Identity` when that header is sent, a strict unpadded
  base64url `X-Paperboat-Machine-Proof`, `Idempotency-Key` equal to the server create
  operation ID, and the exact current strong `If-Match` lease ETag. The proof binds
  method, path, body, machine, installation generation, and operation ID. The strict
  body is `{owner_machine_id, owner_session_id, allocation_state, edge_state,
  origin_state}`; all three readiness dimensions must be `ready`.
- A successful readiness call returns the updated `preview_lease` and its new strong
  ETag. If a host retries after an uncertain response, reusing the same operation key,
  proof, body, and original ETag returns the already-ready lease/ETag. A different
  owner, session, operation key, proof, body, or generation remains unauthorized or a
  precondition conflict. Browser authentication cannot complete this CAS.

### Tunnel Origin TLS

For account/team-scoped self-hosted HTTP edges, a managed route with a verified,
conflict-free custom domain uses that domain as its primary hostname during
certificate bootstrap. Global edges retain the managed hostname. A certificate-ready
alias matching the primary hostname publishes one matcher rule. Domain revocation
withdraws that hostname before forwarding.

Deleting a durable tunnel atomically revokes and quarantines its live domain bindings
and withdraws every durable certificate generation, including wildcard leaves.
Deleting one domain applies the same certificate withdrawal to that binding only.
Encrypted certificate material and edge distribution records remain available to the
existing CA and edge revocation workers; user-owned DNS records are unchanged.

Durable health uses fresh exact edge observations for every active route; a connected
connector alone does not mean ready. Creation and resume finish after this readiness
check, while pause and delete finish after assignment detachment. Completed operations
retain their historical outcome when current health later degrades. Ingress authority
allows the connector protocol’s existing two-minute bound for future issue times;
expiry, the ten-second authority lifetime, and exact resource/identity checks remain strict.

Public route protocol `tls` is opaque TLS pass-through selected only by the
exact SNI hostname. It is available only on public tunnels, uses a plain `tcp`
origin, and shares the server-reserved TLS listener and port across routes and
regions. The managed hostname is always admitted; a custom alias is admitted
only while its exact domain binding is ownership-verified with certificate
strategy `none`. The origin presents and owns the certificate. Wildcards,
missing SNI, HTTP paths/headers, and edge HTTP authentication do not apply.

- HTTPS origins use system CA and hostname verification by default. `custom_ca` requires
  an opaque Paperboat credential reference. Optional mTLS is a separate client credential
  reference and works with either system or custom CA verification.
- `tls.server_name` controls SNI and certificate hostname verification. `preserve_host` and
  `host_override` independently control the HTTP Host header. Changing one never changes the
  other implicitly.
- Credential reference fields accept only bounded `keychain`, `credential-manager`,
  `secret-service`, `protected-file`, or `tpm` Paperboat references. Request and response JSON,
  logs, audit metadata, and errors never contain resolved CA, certificate, or private-key bytes.
- `insecure_development` is accepted only under explicit development policy. Every accepted
  change emits a safe warning audit event. It is never a production default.
- Origin probes and live streams use the same policy. HTTPS, h2c, and mTLS failures keep the
  previous ready generation active; generation replacement closes old idle origin connections.

### Operator administration

Operator-only account, billing and infrastructure operations are defined in
[OpenAPI](../openapi.json) and the [operator runbook](../runbooks/operator-admin.md).
Every operation requires current operator authority and an audit record.

## Canonical Error Codes

Initial contract:

- `unauthenticated`
- `forbidden`
- `payment_required`
- `github_required`
- `validation_failed`
- `idempotency_conflict`
- `quota_exceeded`
- `machine_not_ready`
- `provider_unavailable`
- `provider_outcome_unknown`
- `provider_conflict`
- `tunnel_unavailable`
- `credential_issuer_unavailable`
- `github_config_not_ready`
- `invalid_pagination`
- `invalid_sort`
- `invalid_version`
- `version_required`
- `version_conflict`
- `rate_limited`
- `authorization_pending`
- `slow_down`
- `access_denied`
- `expired_token`
- `invalid_grant`
- `invalid_client`
- `invalid_scope`
- `device_request_not_pending`
- `device_request_expired`
- `device_request_consumed`
- `internal_error`

Adding or renaming public codes after approval requires explicit contract approval.

## Private Control-Plane Endpoints

The following endpoints are private service-to-service contracts and are not browser or
CLI APIs:

- `POST /v1/connectors/admission` requires the renewable machine-control bearer credential and an
  unpadded base64url `X-Paperboat-Machine-Proof` envelope signed by that machine's enrolled
  Ed25519 key. The proof binds `POST`, the exact path and body hash, machine/environment,
  operation ID, and a lifetime of at most one minute. The strict body contains
  `operation_id`, `environment_id`, `machine_id`, `connector_id`, `edge_pool`, and protocol version `1.0`.
  The response is the canonical connector-admission document: the same operation and
  identity bindings, generation, assigned node/pool, one `{host,port}` endpoint, at least
  one revisioned route handoff, protocol version, optional capabilities, and the scoped
  connector credential. It contains no provider credential or internal alternate port.
  Exact retries replay the encrypted recorded document; changed body or proof bindings
  fail before a new admission is minted.
- `POST /v1/tunnels/{tunnel_id}/connectors/enrollments` and
  `POST /v1/tunnels/{tunnel_id}/connectors/enrollments/exchange` are stable-host-only
  mutations. Both require the renewable `machine_control` bearer credential,
  `X-Paperboat-Machine-Identity`, `X-Paperboat-Machine-Proof`, and `Idempotency-Key`.
  The proof covers the exact method, path, body, and idempotency key. The verified machine
  claims provide the account, user, host, and machine actor; `host_id` must match those
  claims. Browser sessions, cookies, CSRF tokens, and client-session bearers are rejected.
  Issue returns the short-lived single-use enrollment secret only at issuance; exchange
  additionally requires the enrollment credential's signed Ed25519 proof of possession and
  returns only the server-reserved, secret-free activation projection. Exact retries replay
  the original result, while changed bindings fail closed.
- `POST /v1/helper-enrollments` is the one-time credential exchange for an enrolled
  user-owned machine.
- `POST /v1/config/credentials` accepts a machine-control credential, a bounded `{}` JSON
  body, and `X-Paperboat-Machine-Proof`; it returns a short-lived `config_sync` credential
  bound to the active machine assignment, installation generation, and warning revision. Exact operation
  replays return the original credential; conflicting replays return `operation_conflict`.
- `POST /v1/preview-launches` is the existing helper route used by the server's
  canonical dispatcher. The request is a strict `paperboat.preview-tunnel/v1`
  `preview_dispatch` projection containing the already-created preview lease,
  operation ID, owner machine/session, typed target, lifecycle snapshot, lease ETag,
  expected generation, and non-secret idempotency/request/correlation identifiers.
  The bearer is a short-lived, single-use `preview_launch` credential bound to the
  exact request hash and every identity field. Hosts return only the matching
  `accepted|ready|failed` outcome; acceptance never completes the create operation.
- `POST /v1/machine-installation-failures` accepts an active helper identity,
  its exact Ed25519 request proof, and only `pairing_id` plus the bounded stage
  `service_install|service_readiness`. It transitions the matching `installing|connecting`
  pairing installation to `failed_retryable`; its stage is `artifact_verification`, `service_install`,
  or `service_readiness`; it carries no local paths, error output, credentials,
  artifact URLs, or process data. Retry preserves the existing machine, environment,
  helper identity, and seat while rotating bootstrap generation and material.
- `GET /v1/trust/revocations` requires the edge-control bearer credential and returns the
  bounded revocation document consumed by tunnel trust snapshots (`jtis`, `environments`,
  `connector_generations`, and `key_ids`).
- `POST /v1/edge/routes/observations` accepts the bounded applied route snapshot from the assigned
  tunnel node. Each item is accepted only when route revision, node ID, and connector
  generation still match current desired ownership; stale observations return
  `version_conflict` and cannot mutate newer intent.
- `POST /v1/edge/private-access/grants` and `POST /v1/edge/private-access/authorize` are the
  edge-only private preview/tunnel authorization boundary. Their exact proof, grant, audience,
  generation, revocation, and non-enumerating denial contract is documented in
  [private-access.md](private-access.md).
These endpoints never return repository/provider secrets. Assignment replacement, consent
revocation, helper replacement, and environment revocation invalidate subsequent credential
use through the snapshot document.

## Operation support reference

User-facing failures use one `support_reference` (`support_` plus a canonical lowercase UUIDv4).
Clients carry it in `Support-Reference` on PB control requests and reuse it through retries;
the server returns it in the response header and error envelope. Request IDs and Sentry
event IDs remain internal mappings. The audited staff lookup and account-safe status APIs,
privacy policy, reporting configuration and retention are documented in
[support references](../runbooks/support-references.md).

### Installed machine runtime carrier

`POST /v1/runtime/carrier-attachment` authenticates the existing renewable machine
identity and exact request proof. Its body contains `operation_id`, `process_id`,
`worker_generation`, `installation_generation`, and `runtime_listen_port`. It may include
`edge_preference` with measured primary and secondary node IDs/process epochs, and
`active_edge_node_id`/`active_edge_process_epoch` to promote a connected standby.
Both generations must match
the enrolled machine; `process_id` fences overlapping processes and is not authority.
Managed runtime hostnames use managed edges that receive the platform certificate;
customer self-hosted edges are excluded from selection and live route authority.
The account tunnel pool policy still applies, so a self-hosted-only policy does not
silently fall back to a managed edge.
The server derives the existing runtime route. The machine uses its own configured
loopback listener as the forwarding target; the server records its selected port
for diagnostics. The response
is `{data: {schema: "paperboat.runtime-carrier/v1", binding, attachment_generation,
config_content_hash, edge_endpoints, hostname, route_kind, route_revision,
expires_at, backup, edge_candidates, active_edge_node_id,
active_edge_process_epoch}}`. `backup` is a second complete admission when a
separate eligible failure domain exists. Candidates are bounded to 32 and
carry probe addresses only; the carrier still authenticates its exact edge.
`binding` carries the account/machine, carrier identity,
installation/process/config/route generations, current edge process, pinned edge
certificate and current machine public key/thumbprint. Existing wire fields
`machine_identity_public_key` and `machine_identity_thumbprint` remain explicit.
The host renews before the 60-second admission expiry; unchanged bindings retain
their carrier sessions. It connects both admitted edges but opens terminal output
only on the server-confirmed active edge. An active change requires a fresh ready
observation for the requested exact node and process epoch. The previous output
publication closes, so browser clients obtain a new ticket and screen checkpoint.
Neither the server admission nor the edge snapshot carries
a local target.

`POST /v1/edge/runtime/carrier-admissions` takes `edge_node_id` and
`process_epoch`, authenticated to that exact registered node process, and returns
`{schema: "paperboat.runtime-carrier/v1", complete: true, admissions: [...]}`. It is a complete
snapshot bounded to 4096 items and 16 MiB; overflow fails closed.
`POST /v1/edge/runtime/carrier-observations` adds `observations` containing
`route_id`, `attachment_generation`, `session_id`, and `ready`. The entire batch
must match current bindings. Only an authenticated actual carrier observation
admits a route. A standby observation cannot alter active browser routing.
`ready:false` on the active edge withdraws immediately; readiness expires after 30
seconds without observation. Machine revocation, generation/key changes, edge
replacement, route replacement and admission expiry withdraw eligibility even
if older control-route observations still exist. Error codes distinguish invalid
requests, stale bindings and retryable runtime-carrier unavailability.

Tunnel viewer authority uses a dedicated access revision when no route-specific browser policy exists. Route, name, and lifetime edits advance the resource revision without revoking unchanged access. Native access-mode changes advance the access revision, including changes back to a previous mode. Edge assignments only include routes present in their exact immutable configuration snapshot.

Canonical edge publication reports `prepared` after exact carrier, origin and local TLS readiness. This grants only the exact staged assignment's ingress authority and does not mark health or operations ready. The edge then publishes its local matcher and confirms `ready`; only that observation promotes the assignment and completes readiness. Failed confirmation retains the published prepared assignment and retries the same tuple. Route removal with remaining routes waits for their usable publication, not only host configuration activation.

Native account bootstrap uses `POST /v1/config-repositories/{repository_id}/read-access`
with optional `{ "url": "credential-free-machine-alias" }` for custom Git only and
`machines:read` bearer scope. It requires exact active repository ownership, checks
repository version again after issuance, and grants only `repository_contents_read`.
It does not authorize machine filesystem access, publish, or bypass mapped-root consent.
The response is never cached; client access expires within five minutes (provider
credential expiration remains provider-authoritative). GitHub returns its scoped read
token for the registered HTTPS endpoint; custom Git returns no provider secrets.
The browser proxy must not expose this native credential endpoint.


### Terminal browser sharing and code joins (v1)

Account access uses `GET/PUT /v1/terminal-sessions/{session_id}/browser-access-policy`.
Audiences are `owner`, `specific_emails`, and `team`. A single bounded `allowed_emails`
list accepts exact verified account emails and `*@company.com` patterns; only that
exact domain matches, never `sub.company.com`, partial local wildcards, or wildcard
subdomains. `interactive_emails` remains exact addresses only, covered by the viewer
list and explicitly confirmed when added. A browser team policy is an explicit grant
for this terminal to an active paid team the owner belongs to; both owner and viewer
membership and team state are checked currently. It creates no native machine or
terminal grants. The owner retains account access under every policy.

Code access is independent: owner-only `GET/PUT /v1/terminal-sessions/{session_id}/code-access`.
The default state is disabled, viewer, generation 0. PUT carries `expected_generation`,
`enabled`, `role`, `expires_in_seconds` (3600, 28800, or 86400 when enabled), `reset`,
and `confirm_interactive_access`. Activation/reset returns a random 12-character
code once; GET never returns it. Only a terminal-bound hash is stored. Control requires
explicit confirmation; code guests never receive owner authority. Reset, stop, or
expiry invalidates old guest sessions and tickets without changing account policy.
Owner GET also lists at most 64 admitted guest sessions (display name and expiry),
which is not live participant presence. Rotation/stop deletes these sessions and their
tickets; expired identities fail authorization immediately and are removed on the
next join/reset or terminal deletion.

The shared dashboard link is `/terminal/{session_id}`, outside the signed-in dashboard.
`GET /v1/terminal-sessions/{session_id}/browser-session` tries current account access
first, then an exact-terminal guest cookie, and returns only authorized terminal metadata
plus `access_method: account|code`. Denial reveals neither existence nor settings.
`POST /v1/terminal-sessions/{session_id}/code-join` accepts a code and a 1–64-character
display name without requiring an account; exact trusted Origin remains mandatory.
It issues a cryptographic 256-bit guest bearer in a terminal-specific
`__Host-pb-terminal-<id>` Secure, HttpOnly, SameSite=Strict cookie on Path=/, expiring
with code access. This cookie is never accepted as an account login, inventory,
native access, or another terminal's authority. Codes and bearers must not enter
URLs, logs, browser persistent storage, or diagnostics. Joining includes recent
terminal output/current screen, which may contain secrets.

`POST /v1/terminal-sessions/{session_id}/guest-browser-ticket` requires that guest
cookie and exact trusted Origin. It retains the existing 30-second single-use E2EE
ticket, ephemeral browser public-key binding, exact edge/runtime/process fences,
and eight-pending-tickets/session limit. Ticket identities have mutually exclusive
account-session or guest-session foreign-key branches. Browser-only runtime credential
`subject`, `user_id`, and `account_id` bind the opaque guest principal ID for guests;
this does not create or impersonate an account. Guests receive only `terminal:view`
or explicitly enabled `terminal:control`, never `terminal:operate`. The control plane
reauthorizes active connections every two seconds and fails closed on failed/expired
checks; edge watchdog/check latency also bounds withdrawal. Existing account ticket
authentication and CSRF are unchanged. Native shared-terminal joins remain account-only.

Code admission is bounded to 120 attempts/minute and 64 unexpired guest identities
per terminal, serialized in PostgreSQL across server processes. Rate limits return
429 with `Retry-After: 60`; capacity returns 409 with guidance to reset access.
Normal code expiry choices support a small collaboration session through a full
workday, with an explicit 24-hour maximum rather than permanent guest authority.

## Discovery pages and filters

Customer inventories select only authorized resources in the selected workspace, apply
filters before limiting the page, and use a stable ID tie-break. Offset collections accept
`limit` (default 50, maximum 200) and `offset` (default 0), and return
`pagination: {limit, offset, total, next_offset}`. `next_offset` is null at the end.
Machines and per-machine terminal sessions return `items`; the global terminal catalog
retains `sessions`, Team Inbox retains `requests`, config repositories retain `items`,
and config sync status retains `environments`. Config sync summary state covers the full
filtered inventory. Operator release-authority collections retain `requests` or `bundles`
and now include the same pagination object. Admission-bounded teams, workspaces, favorites,
and environment scope documents remain complete snapshots.

Text `q`, one domain `state`, and `owner=mine|shared|<account_id>` select within current
resource authority, never expand it. Terminal inventories also support `closed=true|false`.
Inbox `owner=mine` means recipient and `owner=shared` means sender. Machine and terminal
inventories interpret `mine` as owned and `shared` as granted; exact account IDs remain
subject to the same authorization predicate. Invalid values and repeated parameters fail
with `invalid_inventory_query` rather than being silently ignored.

Preview and tunnel inventories retain `items,next_cursor`; cursors bind account, workspace,
and `q/state/owner` filters. Preview state uses `allocating`, `connecting`, `ready`,
`owner_disconnected`, `expired`, `stopped`; `active` groups only `allocating`,
`connecting`, and `ready`. `owner_disconnected` is terminal after the owner reconnect
grace expires: its allocation and edge are released, renewal is refused, and it remains
available in history or via its explicit state filter. A still-owned lease awaiting
allocation or reconnect remains active. Tunnel state
filters use desired state `active`, `paused`, `deleted`. Preview reads include `route_id`
when a current ready carrier route is available for its viewer access policy.
GitHub repositories and config repository candidates use `items,next_cursor`, with default
limit 50 and maximum 100. Candidates filter private repositories at the provider before
paging; their opaque cursor binds account, limit, and private visibility. Provider access is
checked on every page and immutable repository reads use the provider ID directly.

`GET /v1/config-repositories/{repository_id}` reads the exact active owned repository.
`GET /v1/team-invitations` and `GET /v1/teams/{team_id}/invitations` return authorized
`items,pagination`, supporting `q/state/owner`; states are `pending`, `accepted`, `cancelled`,
`expired`. Recipients and current team administrators can read
`GET /v1/team-invitations/{invitation_id}`; no invitation code or secret is disclosed.
Recipients decline using `POST /v1/team-invitations/{invitation_id}/decline` with
`{operation_id}`; replay rechecks the current recipient before returning the saved result.
Team creation accepts an optional bounded `name`; rename uses the existing team mutation
with `{operation_id,expected_generation,action:"rename",name}`. Existing unnamed teams
project their slug as their name.

Self-hosted installation discovery uses `GET /v1/selfhost/installations` (or the
operator-only `/v1/admin/selfhost/installations`). The response keeps
`data.installations` and requires `data.pagination` with `limit`, `offset`, `total`,
and nullable `next_offset`, plus `data.counts` with complete filtered `ready`,
`enrolled`, and `pending` totals. Limit defaults to 50, maximum 200; offset is
0–2147483647. Exact `scope_kind`/`scope_id` authorize discovery; omission lists all
currently visible installations. `workspace` is not accepted. Optional filters
are `q` (literal case-insensitive substring of name, endpoint host, installation
ID or node ID, valid UTF-8 at most 160 bytes, no NUL), `capability=relay|tunnel`,
`enrollment_state=pending|enrolled`, and `ready=true|false`. Unknown and duplicate
parameters are rejected. Filters apply before pagination; count and page share
one database snapshot, ordered by name, capability, installation ID. Enrollment
and pool-policy responses remain bounded atomic documents.
`GET /v1/selfhost/installations/{installationID}` returns one exact current
installation in `data`, with no query parameters. Absent or invisible records
return 404. The separate operator detail route only reads global installations.

Self-hosted network enrollment and removal manage a scoped installation. Removal revokes
that installation's runtime authority and scope eligibility. It retires the physical node
and signing keys only when no active installation still owns that node. The current scoped
installation model does not expose an independent pairing detach operation.

`GET /v1/usage` requires the selected workspace's read authority and RFC3339 `start` and
`end` within the existing 90-day window. Resource groups use `limit` (default 50, maximum
200) and `offset`, with `pagination:{limit,offset,total,next_offset}`. Every resource has
`path_pagination` in the same shape; `path_limit` defaults to and cannot exceed 32, and
`path_offset` continues its paths. Totals and series cover all matching measurements,
independently of both pages. Group ordering is kind, ID, route ID, then machine ID; paths
order by mode, hosting, then node ID. Filters include `resource_kind`, `resource_id`,
`mode`, and `hosting`. An absent `resource_route_id` or `machine_id` includes all values;
an explicitly empty value selects the empty value exactly. Invalid, unsupported, or
repeated query parameters are rejected. Exhausted path pages preserve the resource and
its totals. Every group and path is reachable by continuation; the response has no
truncation flag.

`POST /v1/config-sync/environments/{environment_id}/conflicts/{conflict_revision}/connection`
authorizes an exact current conflict for a native CLI client. The request includes
`assignment_version`, `path`, `expected_remote_revision`, the authenticated
`cli_client_session_id`, and an owned `source_machine_id`. The response is a signed direct
descriptor with `config:compare`. The grant binds assignment, version, path, conflict
revision, remote revision, machine installation, and exact CLI client identity. No
terminal is created. The endpoint never returns or persists private file payloads; the
host sends the comparison through the native encrypted transport.

`GET /v1/environment/hosts` discovers only the account's active ENV-capable registered
machines that have already received an owner-authorized projection. It returns
`{items:[{machine_id,selection_generation,projection_revision,fence_generation,state}],
pagination:{limit,offset,total,next_offset}}`, ordered by machine ID. `limit` defaults
to 50 and is bounded to 1–200; `offset` is nonnegative. Optional `state` accepts only
`pending` or `ready`, filtered before paging. Only the Personal workspace is applicable;
invalid, unsupported, and repeated query parameters are rejected. It excludes newly
registered recipients with no prior projection and never expands selection or grants.
Exact `GET /v1/environment/hosts/{machine_id}` supplies the owner's current selection
and cryptographic binding for automatic refresh through the existing signed projection
PUT. `ready` means current-generation ciphertext is published with the current fence;
it does not assert that an offline host has applied it. Scope changes fence the old
projection and leave it `pending` until an unlocked authorized owner publishes a successor.
No plaintext values, recipient keys, selection names, or ciphertext are returned by the
collection. Revoked, deleted, foreign-owned, and ENV-disabled hosts are excluded.

Exact owner `GET /v1/environment/hosts/{machine_id}` also returns
`observation:null|{schema,observation_seq,host_recipient_key_id,
projection:null|{revision,document_id},fence_generation,state,error_code,observed_at}`
and `applied:boolean`. Observation states are `pending`, `applied`, `failed`, and
`revoked`; only failure includes a bounded public error code. The stored report is
strictly parsed and bounded to 4096 bytes. `applied` is true only for a published current
installation binding whose observation reports `applied` with the exact current recipient
key, projection revision, document ID, and fence. An old applied report remains visible
with `applied:false` after publication or authority changes. Missing observations return
null and false. These fields contain no ENV values or file payloads. Bundle `ready`
continues to mean published and deliverable, independently of host application.

After a Personal vault total-loss reset, an active Team owner can still read the
signed encrypted Team predecessor and its persisted historical writer public key
through Team metadata to prepare confirmed total-loss Team replacement. This does
not restore the old key grant or authorize ordinary scope reads, writes or host
publication. Other members retain current-epoch fencing.

Team ENV metadata includes `entitled`, reflecting the Team's current active
subscription. Initialization saves the encrypted scope and current owner key grant;
it does not activate paid Team benefits. Scope reads/writes and host publication
from that scope require entitlement in addition to exact active ENV grants and
current key epochs. An otherwise authorized caller receives HTTP 403
`team_entitlement_required` with an action to activate the Team subscription in
Billing. Missing/revoked membership or grants retain the ordinary forbidden response.

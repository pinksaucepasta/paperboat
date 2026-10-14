# Contract Decisions

Status: current v1 decisions; implementation and deployment evidence live in TRACKER.md.

## Authority

The current [product plan](../../../plans/paperboat-product-and-transport-plan.md),
server router, v1 schemas and owning-task evidence in
[TRACKER.md](../../../plans/TRACKER.md) define the implemented boundaries.

## Approved Or Draft Decisions

### Identifiers and Naming

Status: approved convention; rollout evidence and remaining identity consolidations
are recorded in TRACKER.md.

- Ordinary generated resource, operation, request, and credential-instance IDs use
  one format: `noun_UUID`, with a full singular lowercase noun and canonical
  lowercase UUIDv4 text, as defined by
  [RFC 9562](https://www.rfc-editor.org/rfc/rfc9562.html#section-5.4). UUIDv4 avoids
  exposing a creation timestamp. Use the maintained `github.com/google/uuid`
  implementation and cryptographic randomness; never substitute a timestamp when
  randomness fails. Examples are `user_UUID`, `machine_UUID`, `terminal_UUID`,
  `pairing_UUID`, `keyset_UUID`, `request_UUID`, `operation_UUID`, and `support_UUID`.
  Use the owning entity's noun, not abbreviations or workflow chains such as
  `usr` or `endpoint_enrollment_request`. More specific context belongs in fields.
- IDs are opaque. Resource kind, credential class, owner kind, and authorization
  come from typed fields and authoritative state, never the spelling of an ID.
  There is no alternate legacy format, compatibility reader, or fallback generator.
  Signed binary identities and cryptographic fingerprints retain their protocol encoding.
- Preview owner lifetime uses the required immutable `owner_session_kind` field
  (`local_lease`, `foreground`, `lazy_runtime`), persisted and bound in request hashes
  and signed launch claims. Hostd owner IDs use `session_UUID`; their prefix never
  decides whether a released local lease can be registered again.
- Terminal records and runtime sessions share `terminal_UUID`; descriptors do not
  carry a second terminal or constant thread identity. Pairing owns bootstrap,
  approval and installation progress. Lazy activations persist `session_UUID`;
  boot and generation metadata fence authority without encoding an ID.
  Managed SSH observations persist server-issued `keyset_UUID`; fingerprints
  remain separate cryptographic evidence.
- A machine owns its subordinate control scope with the same `machine_UUID`.
  Runtime helper incarnations retain distinct IDs and generation fences. Carrier
  bindings reference the actual machine, route and preview; runtime and preview
  receiver sessions remain separate `session_UUID` instances.
- Managed tunnel endpoints use `endpoint_UUID`. Signed configuration carries
  `stable_endpoint_id` explicitly; readable DNS names are independent and never
  decoded into identity. Consumers compare the enrolled endpoint binding exactly.
- Canonical configuration repository records own GitHub provisioning metadata.
  Provider identities, credential custody, provisioning receipts, operator receipts,
  capability/availability receipts and bandwidth periods use their existing natural
  keys when no separate resource lifecycle exists.
- A resource has one authoritative ID across its URL, API, and runtime. Removing
  duplicate identities requires a coordinated schema/producer/consumer cutover,
  separately from changing random generation.
- JSON uses `snake_case`: an object's own identifier is `id` where its resource
  context is unambiguous; references use clear names such as `machine_id`,
  `terminal_id`, `operation_id`, and `request_id`. Go uses established initialisms
  (`ID`, `UUID`, `JWT`, `JTI`). Do not shorten names into product-specific codes or
  repeat context already supplied by the object/package.
- Retain the standard JWT `jti` claim from
  [RFC 7519](https://www.rfc-editor.org/rfc/rfc7519.html#section-4.1.7).
  A token instance is distinct from an authentication session or resource.
- Cryptographic key fingerprints, content hashes, and deterministic replay keys
  retain their defined algorithms, exact domain-separated inputs, and protocol
  validation. Bearer secrets and nonces retain purpose-specific entropy, encoding,
  proof binding, expiry, and cleanup; UUIDs are not bearer-secret replacements.
- Telemetry accepts bounded safe opaque identifiers in typed fields. ID-format
  consistency must not weaken secret rejection or payload redaction. UI displays
  resource names, status, and access; internal IDs are reserved for links and
  deliberate technical diagnostics.

Support references use that same convention: `support_UUID`. They correlate
evidence and grant no access. A future authenticated help URL uses the complete
identifier; it does not introduce a second wrapper or alternative ID format.
Lookup must resolve the authoritative owner/resource and check current account
authority before reading any logs or third-party evidence. Preserve the existing
owner-scoped support-report association, stable request/support correlation,
bounded retention, and sanitized Sentry event references. The prefix is routing
context, never proof of ownership. Automated diagnosis and issue creation are
separate future work; this rollout does not add a help endpoint, LLM integration,
or issue writer.

Reference evidence: [Octelium's UUID helpers](../../../references/octelium/cluster/common/vutils/vutils.go),
[NetBird's opaque XIDs](../../../references/netbird/management/server/peer.go), and
[Tailscale's typed IDs](../../../references/tailscale/tailcfg/tailcfg.go) demonstrate
different valid representations. Paperboat chooses one consistent prefix vocabulary
and UUID representation with explicit ownership types; it does not copy their
unrelated storage or authorization models.

### Control Plane Boundary

Status: approved baseline.

`paperboat-server` is a control plane only. Live terminal, preview, HTTP, and
WebSocket traffic stays out of `paperboat-server`. Native private traffic is
end-to-end encrypted between machine runtimes. Browser HTTP terminates TLS at the edge;
browser terminals use an encrypted machine-to-browser stream across that edge.

### Persistence Backend

Status: approved.

Decision:

Postgres ships from the first production release. Subsequent migrations create Paperboat
objects inside a dedicated `paperboat` schema so the service does not collide with
provider-managed or pre-existing `public` schema tables.

Implementation rules:

- `paperboat-server migrate` applies forward-only migrations.
- Goose executes immutable SQL migrations from `internal/db/migrations`.
- sqlc generates typed application queries from `internal/db/queries`; generated code is
  checked in under `internal/db/dbsqlc`.
- Runtime repositories use explicit transaction boundaries and set their transaction
  search path to the `paperboat` schema.
- Billing products are managed in Postgres through the Stripe-verified operator API;
  deployments do not write them from a catalog seed file.

### WorkOS Session Model

Status: approved baseline.

Decision:

- Dashboard uses WorkOS authentication and sends the callback to
  `POST /v1/auth/workos/callback`.
- Server maps WorkOS subject plus email to a Paperboat user idempotently.
- Browser session uses an HttpOnly secure session cookie and CSRF token.
- The server stores WorkOS refresh credentials encrypted. Each browser request is
  authorized against the WorkOS access-token deadline; expired tokens are refreshed
  through WorkOS before the request proceeds. Confirmed provider revocation ends the
  Paperboat session. Sessions without refresh credentials require a new sign-in.
- Core feature APIs require both authenticated session and active entitlement.
- Allowed dashboard origins are environment-specific dynamic config values.
- WorkOS redirect and callback URLs are environment-specific provider configuration.
- Startup `browser_sessions` configuration controls the Paperboat cookie lifetime and renewal
  threshold (defaults 30 days and 7 days). Renewal also requires an active WorkOS session; it does not
  implement a separate idle timeout or immutable absolute lifetime.
- Private browser resource sessions separately use configurable absolute and idle
  lifetimes (defaults 12 hours and 30 minutes). Parent session expiry, revocation,
  resource expiry and generation checks still cap access. Positive whole-second
  durations are required; renewal threshold must be shorter than login lifetime,
  and resource idle duration must not exceed its absolute duration.
- Configuration changes apply after restart to issuance/renewal. Stored absolute
  deadlines are not rewritten and this is not a retroactive session-revocation tool.
- Team activity retention remains a fixed 90-day product/integrity contract, enforced
  by the baseline's append-only audit trigger as well as listing/pruning. It is not
  an operator-tunable deletion setting.

### Stripe personal and team subscriptions

Status: approved model; commercial values unset.

Free and Pro have identical personal features. Optional allowances, usage accounting
and quota enforcement are implemented with unset values imposing no limits. Team
subscriptions are billed per active member and apply only to that team's authorized
resources. They do not upgrade personal subscriptions or grant permissions.

The authoritative Stripe mapping is `billing_products`; products and prices remain
inactive/unset until decided. Checkout replacement, verified webhooks and uncertain
operation recovery follow [provider-contracts.md](provider-contracts.md) and the
[billing recovery runbook](../runbooks/billing-uncertain-operations.md). Superseded
Polar runtime has been removed; historical migrations remain.

### GitHub OAuth and Config Repo

Status: approved baseline.

Decision:

- GitHub OAuth is optional and authorizes GitHub configuration repository access.
- Server stores encrypted GitHub token material.
- Server manages multiple private configuration-repository connections per user,
  separately from the zero-or-one assignment on each environment.
- The `pb` config worker handles ongoing pull/apply/commit/push. The server owns repository
  authorization, assignment eligibility, scoped repository access, and writer leases.
- OAuth scopes are dynamic config; production default is the minimum GitHub scopes that
  allow provisioning and syncing authorized private configuration repositories.
- Config repo name is dynamic config and defaults to `paperboat-config` in local examples.
- Token material is encrypted at rest, revalidated before sensitive provider operations,
  and treated as revoked when GitHub rejects validation or refresh.
- Config-sync access is short-lived and bound to the repository, assignment,
  environment, installation generation and operation. User-facing APIs do not return
  provider credentials.

### Machine access

One installation initiates authorized operations and serves its enabled capabilities.
Fresh enrollment defaults incoming terminal/exec, managed SSH, file receiving and
preview/tunnel serving on; peer relay is opt-in. Machine capability controls govern
incoming behavior without removing outgoing access.

The server authorizes the exact resource and current installation generation before
issuing a short-lived native descriptor or browser ticket. Machine identity, resource
ownership, tenant isolation and revocation remain independent of capability selection.
See [CLI authorization](cli-authorization.md) and [private access](private-access.md).

### Paperboat CLI Client Sessions

Status: Contract frozen in [cli-authorization.md](cli-authorization.md).

Decision:

- WorkOS plus `paperboat-server` is the only Paperboat identity authority.
- Device authorization grants are explicitly approved or denied through the dashboard and are single-use.
- Each installation has an independently revocable client-session family with rotating
  refresh tokens and family revocation on replay.
- CLI and helper may share only the documented Paperboat credential profile and OS
  secure-store references, never private application state.

## Readable generated names — 2026-10-03

User-approved disjoint lists contain exactly 64 qualities, 64 atmospheres and 64
waypoints in `internal/naming`. Automatic terminal handles are three independently
random words; managed preview/tunnel/machine-service URL labels add four uniformly
random digits, including leading zeroes. Cryptographic randomness failures return
an error, never a counter or timestamp fallback. Allocation persists names once;
terminal handles skip retained names for the owner across machines, and exact
hostnames are atomically reserved across producers. Collision retries are bounded.
Readable URLs are independent of opaque resource and signed runtime identities.
Existing persisted references are retained; names never regenerate on reads.

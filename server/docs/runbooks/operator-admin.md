# Operator backend

Paperboat operators use the server's `/v1/admin` APIs. The admin frontend is
not implemented. Operator accounts and sessions are stored separately from
customer accounts and WorkOS. Team roles and customer sessions do not grant
operator access.

## Staff permissions and authentication

| Role | Permissions |
| --- | --- |
| support | Account/team/workspace status and PB support-reference evidence |
| billing | Support permissions, pricing/policy inspection and billing recovery |
| operations | Infrastructure/runtime/reporting status, relay/tunnel operations and control recovery |
| admin | All operator APIs, account/team suspensions, pricing/policy changes, key and release trust operations |

Set `PAPERBOAT_OPERATOR_EMAIL` to the first operator's email in the server
environment. The email-first API is `POST /v1/operator/email` with
`{"email":"..."}`; it returns `setup_password` for the configured email until
the first operator is created, `password` for an existing active operator, and
404 otherwise. Submit `{"email":"...","password":"..."}` to
`POST /v1/operator/setup` once, or to `POST /v1/operator/login` afterward.
The first setup is permanently consumed in the database, including across
restarts and configuration changes. The password is stored as an Argon2id hash.

Sign-in sets a separate HTTP-only operator cookie and returns a CSRF token.
Send that cookie and `X-CSRF-Token` for mutating `/v1/admin` requests.
`GET /v1/operator/me` checks the operator session; `POST /v1/operator/logout`
revokes it. Operator sessions last eight hours. Five failed passwords lock an
account for fifteen minutes. The current backend has no API to add additional
operators; that belongs to the admin panel work.

## Reads and status

Use `limit` (1–100, default 50) and `after` for list pagination. List cursors are
opaque resource IDs, not evidence of authorization. Account search accepts only
an exact `email`. Billing/control lists accept allowlisted `state` filters.

- `/v1/admin/overview`: account, billing, operation and infrastructure counts.
- `/v1/admin/accounts` and `/{account_id}`: safe identity/status/version and the
  personal billing workspace ID.
- `/v1/admin/teams` and `/{team_id}`: ownership/generation, suspension and bounded
  membership pages, plus the team's billing workspace ID.
- `/v1/admin/billing/workspaces` and `/{workspace_id}`: subscription state, paid
  quantity, period and cancellation status; no provider credentials or checkout URLs.
- `/v1/admin/billing/workspace-operations` and `/{operation_id}`: recovery targets.
- `/v1/admin/control-operations` and `/{operation_id}`: control recovery targets.
- `/v1/admin/infrastructure?kind=host|relay|tunnel`: revision, desired/observed
  state and freshness. A stale observation never reports ready. A durable tunnel
  configuration without a runtime observation reports unknown, not healthy.
- `/v1/admin/tunnels/{tunnel_id}/status`: separate connector-session, edge-assignment,
  DNS and certificate state counts, unobserved counts and observation timestamps.
  No layer alone proves that a tunnel is fully ready.
- `/v1/admin/status`: backend readiness, observed subsystem state, provider
  configuration booleans and a small allowlist of deployment settings. Configured
  providers are not claimed live; readiness is not an external provider probe.
- `/v1/admin/reporting`: server Sentry enablement, release/link readiness,
  process-local dropped records, seven-day evidence retention and honest unknowns
  for SaaS receipt, source-map upload and other component processes.
- `/v1/admin/support/{reference}?owner_id=...`: one PB reference, optional exact
  account filter, bounded timelines grouped by owner and Sentry deep links.
- `/v1/admin/audit/{resource_id}?resource_type=...`: bounded audit identity/action
  metadata for an exact resource, without raw event metadata or private content.

Sensitive reads and their audit event share a transaction. The HTTP boundary
also records request intent/outcome and holds the response until its outcome
audit succeeds. Responses are `Cache-Control: no-store` and bounded to 2 MiB.
Audit failures prevent disclosure or execution; no raw request bodies, provider
errors, tokens, keys, terminal/file contents or ENV values enter these views.

## Mutations and retries

Every operator mutation requires session/Origin/CSRF checks plus:

- `Operator-Target`: exact resource ID from the path. Product updates use product
  code; global billing policy uses `billing-policy`; usage-key provisioning uses
  body `key_id`; release requests/imports use body `release_id`.
- `Operator-Reason`: an opaque ticket/evidence reference, 1–128 ASCII letters,
  digits, `_`, `-`, `.`, or `:`. Never send private content as a reason.
- `Idempotency-Key`: a stable 8–128 character opaque token for the logical change. Retain it when
  retrying an interrupted/uncertain request; do not change the request body.
- The current resource `revision`/`version` where required. Fetch it first;
  stale updates return a conflict and require reviewing current state.

Account ban/unban revokes existing customer credentials on ban and never
resurrects them on unban. Operator identities are separate, so banning a
customer account does not change an operator account.
Relay drain stops new scheduling; resume requires a new heartbeat before ready.
Tunnel disable/enable uses the normal configuration-generation and operation
pipeline, so acceptance is distinct from runtime convergence. Team suspension
preserves membership and encrypted data while fencing access; billing provider
subscriptions are not implicitly cancelled by a suspension.

Existing billing recovery endpoints retain their evidence-based restrictions:
retire only proven not-applied expired operations, reconcile checkout against the
actual provider session, and supersede portal operations under the owning billing
rules. Control recovery requeues eligible dead-letter operations. Key lifecycle
and signed release trust operations keep their underlying integrity checks.

Mutation effects, durable retry receipts and success audit records commit
atomically. If an HTTP outcome cannot be audited after a committed effect, the
API returns `operator_result_unavailable`: inspect state and retry with the same
key. A `started` audit with no final outcome identifies an interrupted request;
it does not prove failure or success. Never issue a different key just to bypass
a conflict or an uncertain provider operation.

## Pricing and runtime billing policy

Pricing uses the authoritative Stripe price and the existing `billing_products`
catalog. Admin APIs register an existing verified monthly or yearly recurring Stripe price, set its
display name/allowance and enable or disable it for new checkout. Create prices
in Stripe first; this API does not invent an amount or currency. Changing a price
requires a new product code, preserving the old price mapping for existing
subscriptions and webhook recovery. Deactivation blocks new purchase, not
recognition of existing subscriptions. This is not an automatic customer plan
migration, refund or provider invoice adjustment.

`GET/PUT /v1/admin/config/billing` manages the authoritative shared policy for
personal/team bandwidth allowances. `null` means unlimited; zero means no
allowance. Deployment defaults seed it only once. Changes apply to new accounting
periods; recorded usage periods retain their committed allowance. Per-plan count
limits are not part of this policy: they are deployment configuration
(`billing.limits`): machines and concurrent previews plus tunnels 100 Free, 1000 Pro,
Teams advertised as unlimited with hidden ceilings of 10000; console browser
terminals 10, 100 and a hidden Teams ceiling of 1000. Secrets, provider accounts,
signing trust roots and network listener configuration remain deployment-owned, not
arbitrary editable JSON in the admin API.

## Activation and evidence

Apply the forward migrations before rolling out this server. Old customer
sessions cannot authenticate as operators. Sentry remains disabled until
its deployment credentials and explicit enable flag are configured. Follow
[support-references.md](support-references.md) for evidence retention/deletion,
release/source-map setup and the live Sentry qualification checklist.

Before production use, qualify the operator login and session lifecycle, Sentry
receipt/grouping/source maps and relevant live provider/runtime convergence.
Database and controlled HTTP-provider integration tests do not establish these
external gates. No frontend, deployment or live SaaS qualification is implied by
the backend implementation.

## Managed regions and runtime nodes

The admin creates region records with `POST /v1/admin/regions/{region_id}/create`,
then enrolls an existing relay/edge machine through
`POST /v1/admin/nodes/{node_id}/enroll`. Creation/enrollment uses `revision: 0`;
updates and lifecycle actions use the current returned revision. Region actions
are `update`, `enable`, `disable`; node actions are `update`, `drain`, `resume`,
`retire`, `rotate`, `verify-ingress`. These configuration, credential, and
public ingress verification actions require the full
admin role. Operations staff may read `/v1/admin/regions` and `/v1/admin/nodes`.
Lists use bounded `limit`/`after`; nodes optionally filter by `region`.

Enrollment records the region, independent failure domain, edge pool, advertised
hostname/ports, roles/transports and capacity. Supply public usage/peer identity keys
where required by the role. The initial usage verification key ID is `managed:<node_id>`;
usage and peer identity keys are immutable in node updates. Use the existing distinct-key
provisioning/revocation APIs for signing-key lifecycle. Public key bytes use JSON's standard base64 encoding;
no private key is uploaded. The response contains a node-scoped runtime credential,
which the deployment installs into that node's existing control-plane credential
configuration. List responses never contain it. Exact enrollment/rotation retries
return the original result; replay credentials remain encrypted at rest. Rotation
invalidates the previous credential immediately; retirement prevents new runtime
requests. Keep the same retry key if a result is lost.

New nodes are unready until actual runtime registration and fresh observations.
The runtime cannot change operator-approved pool/endpoints through registration.
Disabling a region fences its managed nodes' credentials and selection eligibility,
and drains its nodes. Re-enabling a region does not silently resume nodes: explicitly
resume them and require fresh runtime observations. Node responses include `node_generation`, `observed_at` and `registry_expires_at`.
Configuration updates fence the old process epoch/generation; deploy the new configuration
and use the returned generation for relay startup. Managed nodes use only the `/nodes`
mutation APIs; the older `/relays` controls reject them so two revision owners cannot
compete. Configuration acceptance does not
prove public DNS, certificates, firewall rules, or end-to-end traffic are ready.
These APIs enroll/manage actual machines; they do not purchase or create cloud VMs.
Self-hosted installation ownership and account pairing remain separate.

## Backup and restore evidence

`GET /v1/admin/backups` returns the configured logical database identity, newest backup
and restore reports, last successful backup, freshness thresholds, archive retention,
and whether the latest backup has a successful matching restore test.
`GET /v1/admin/backups/runs` exposes bounded immutable run history. Both require
operations/admin. States distinguish `not_configured`, `unknown`, `current`, `stale`,
`expired`, and `failed`; a successful archive alone never marks a restore verified.
A late report cannot replace a newer failure. Storage durability/offsite replication
remains explicitly deployment-owned and unverified by the control plane.

The deployment producer sends bounded reports to
`POST /v1/operations/backups/reports` using a dedicated bearer credential configured
with `PAPERBOAT_BACKUP_REPORT_TOKEN_FILE`. That credential can only report for
`PAPERBOAT_BACKUP_DATABASE_ID`, and grants no staff/database/user API authority.
Evidence contains run IDs, timestamps, outcome/error code, byte count, SHA-256 and
retention, never archive content, paths, SQL output or connection credentials.
Reports cannot be overwritten; a retry must preserve its ID and content. Restore
reports bind the exact successful backup's ID, byte count and digest. Ingestion and
its system audit commit together. Evidence is retained for 90 days, bounded to
10,000 records; the producer's own archive retention is separate.

Build `cmd/paperboat-backup` for the Linux deployment host and install PostgreSQL
client tools matching the source database. Configure protected libpq service/password
files (`PGSERVICEFILE`, `PGPASSFILE`), a dedicated mode-0700 archive directory on the
chosen protected backup storage, and the same reporting token via a secret file.
The producer accepts service names, never passwords/connection URLs in arguments.
Example commands below use non-secret service names and paths:

```sh
paperboat-backup backup --database primary --source-service paperboat-backup --maintenance-service paperboat-verify --storage /var/lib/paperboat-backup --report-url https://api.example.test/v1/operations/backups/reports
paperboat-backup verify --database primary --maintenance-service paperboat-verify --backup-run RUN_ID --storage /var/lib/paperboat-backup --report-url https://api.example.test/v1/operations/backups/reports
paperboat-backup report-retry --database primary --maintenance-service paperboat-verify --storage /var/lib/paperboat-backup --report-url https://api.example.test/v1/operations/backups/reports
paperboat-backup cleanup --database primary --maintenance-service paperboat-verify --storage /var/lib/paperboat-backup --report-url https://api.example.test/v1/operations/backups/reports
```

`backup` writes a bounded custom-format `pg_dump` archive, validates archive structure,
computes its digest, then atomically persists the artifact and report. `verify` restores
only into a newly created, random `pb_backup_verify_*` database, and drops that exact
database afterward, including on cancellation. Use an isolated PostgreSQL verification
cluster for its maintenance service; it needs create/drop database authority. Verification
never restores over a production database. The custom archive does not include cluster
global roles, external secrets, or other services; retain their deployment configuration
and recovery material separately. PostgreSQL behavior follows the official
[pg_dump](https://www.postgresql.org/docs/17/app-pgdump.html) and
[pg_restore](https://www.postgresql.org/docs/17/app-pgrestore.html) contracts.

Each PostgreSQL command has a deadline; archive size and spool count are bounded.
The default compressed archive limit is 10 GiB and retention is 30 days; deployment
must select bounds and storage capacity for its database. Failed reporting leaves a
durable spool: `report-retry` replays backup reports before their restore reports.
Interrupted restore cleanup retains a journal and is retried on the next invocation.
`cleanup` removes only the producer's generated files after retention and protects
undelivered evidence and its referenced archives. Pending reports older than the
server's 90-day evidence window need operator investigation; they are not silently
removed. Schedule backup, restore drills, retry and cleanup on the deployment host.
Default monitoring thresholds are 26 hours for backups and eight days for restore
tests; configure `PAPERBOAT_BACKUP_MAX_AGE_SECONDS` and
`PAPERBOAT_BACKUP_RESTORE_MAX_AGE_SECONDS` to the actual schedule. No scheduler,
backup storage, or restore service is implicitly deployed by the API.

## Sentry issue browsing

The existing `/v1/admin/support/{reference}` remains the single-reference timeline.
`GET /v1/admin/support` adds a paginated local reference feed with owner grouping;
its cursor is opaque and retains groups sharing the same reference.
`GET /v1/admin/sentry/issues` lists issues across the configured projects and
includes the project slug in every issue;
`GET /v1/admin/sentry/issues/{issue_id}` provides bounded events, counts, status and
allowlisted diagnostic tags. `GET /v1/admin/sentry/references/{reference}?owner_id=...`
resolves the latest locally recorded event for the exact selected owner and verifies
its reference tag. Use an explicitly empty `owner_id` for unattributed evidence.
These support views require a current support, billing, or admin role.

Operational investigation uses only fixed queries and safe projections:

- `GET /v1/admin/sentry/logs?period=24h&component=paperboat-daemon` returns identifiers, time, severity,
  trace/reference correlation and fixed Paperboat tags. It never returns log messages.
- `GET /v1/admin/sentry/references/{reference}/signals?period=24h` searches errors,
  logs and spans across every configured project. A PB reference is untrusted search
  input and never grants access; staff authorization is checked independently.
- `GET /v1/admin/sentry/traces/{trace_id}?period=24h` returns at most 200 sanitized
  trace items and rejects the entire response if any item belongs to another project.
- `GET /v1/admin/sentry/metrics/catalog` lists only exact names from the compiled
  server, native runtime and tunnel metric catalogs; provider-supplied names outside
  that set are discarded.
  `GET /v1/admin/sentry/metrics?metric=paperboat.operation.duration&component=paperboat-daemon&period=24h`
  returns the fixed p95 duration series; `paperboat.operation.count` returns its fixed
  sum series. Cataloged cumulative snapshots use a fixed `max` gauge aggregation;
  exported counters end in `_snapshot`, and histogram sums/counts end in
  `_sum_snapshot`/`_count_snapshot`. Snapshot queries return the maximum reported
  gauge for the selected component and bucket; they are not summed across processes.
  Operation counters use sum and operation duration uses p95. These trace-connected metrics are supplementary samples, not durable
  accounting or a replacement for the component's authoritative diagnostics metrics.

Periods are limited to `1h`, `6h`, `24h`, or `7d`; result limits are at most 100.
Components are limited to `paperboat-server`, `paperboat-cli`, `paperboat-daemon`,
`paperboat-relay`, and `paperboat-tunnel`. Reference correlation reports
`available` or `unsupported` independently for errors, logs and spans, so an account
without one dataset still returns its audited evidence from supported datasets.
Each operation has one five-second deadline spanning project resolution, all provider
requests and committed audit. Experimental Sentry capabilities return
`sentry_capability_unsupported` when the configured provider does not expose them.

Metric series omit buckets with no observations, including Sentry's synthetic
zero-filled buckets. A measured zero is preserved when its sample count is positive;
missing periods are unknown, not evidence of zero latency, load or failures.

`GET /v1/admin/sentry/outcomes?project=paperboat-server&period=24h` reads Sentry's
provider-reported ingestion outcomes for one configured project. The only supported
windows are hourly buckets over 24 hours and daily buckets over seven days. Categories,
outcomes, and reasons are projected into fixed vocabularies; unknown provider labels are
merged into `other`, and arbitrary provider labels never reach the response. Use
`accepted`, `filtered`, `rate_limited`, `invalid`, `abuse`, `client_discard`, and
`cardinality_limited` to investigate ingestion and SDK delivery loss. These aggregates
do not report a purchased quota, remaining quota, subscription usage, or billing totals.
The view is capped at 100 provider groups and fails closed on malformed or oversized data.

Configure `PAPERBOAT_SENTRY_ORGANIZATION`, `PAPERBOAT_SENTRY_PROJECTS` as a comma-separated
list of one to eight unique project slugs, optional
`PAPERBOAT_SENTRY_API_BASE_URL` (default `https://sentry.io`), and
`PAPERBOAT_SENTRY_API_TOKEN_FILE` together. Use a read-only token restricted to the
intended project; this API does not mutate Sentry issue state. The token stays on
the backend. This API access is separate from the DSN and
`PAPERBOAT_SENTRY_ENABLED` capture setting. Reporting status exposes the two states
separately; an event ID/SDK queue result is never reported as confirmed delivery.

The adapter resolves configured slugs lazily and sends only their numeric IDs to
organization-wide Sentry queries. It rejects every returned issue, log, span, trace
item or event that escapes that allowlist. It also rejects redirects,
and bounds deadlines, response bytes, pagination and events. Sentry rate limiting
returns 429; outages return unavailable, not an empty healthy result. The admin API
omits arbitrary issue titles, exception messages, raw requests, user data, breadcrumbs,
contexts, stack locals and other provider payloads. Detailed debugging remains in
Sentry under its own access controls. Live delivery/project permissions and release
artifacts still require qualification after deployment credentials are installed.

## Sentry monitor and detector state

Operations staff can read provider-owned alert state without opening Sentry:

- `GET /v1/admin/sentry/monitors?project=paperboat-server` lists bounded cron monitor state.
- `GET /v1/admin/sentry/monitors/{slug}/checkins?project=paperboat-server` lists up to 100 sanitized check-ins.
- `GET /v1/admin/sentry/detectors?project=paperboat-server` lists bounded metric detector state.

The project parameter is required and must name a configured allowlisted project. Responses
include fixed identifiers, status, type, thresholds, schedule margins and timestamps only;
they omit monitor configuration, detector queries, notification recipients, owner emails and
provider URLs. Provider pagination cursors are opaque and bounded. These reads require fresh
operations or admin authority and succeed only after the audit record commits.

The admin-only client update policy lives at `GET/PUT /v1/admin/client-update-policy`.
Use the separate operator session and CSRF token, allowed Origin,
`Operator-Target: client-update-policy`, an opaque `Operator-Reason` evidence reference,
and an `Idempotency-Key`. Read the current revision before sending
`{"expected_revision":0,"minimum_version":"2026.10.8.1","reason":"Required protocol support","enforce_at":"2026-10-09T00:00:00Z"}`.
The reason is public, nonsecret text of at most 256 UTF-8 bytes; do not include account
information, credentials, or incident payloads. A date is optional: without one,
enforcement begins immediately. Empty `minimum_version`, empty `reason`, and omitted
or null `enforce_at` clear enforcement, including when the release origin is unavailable.
Floors must be real numeric `YYYY.M.D.build` date versions available on every
supported platform currently published from the trusted release directory. A global
floor cannot exceed the oldest published platform asset version: publishing a newer
candidate for just one platform does not make that floor safe for the others.

Policy state, its mutation audit, and the original response receipt commit atomically.
`revision_conflict` requires a fresh read and new intentional request.
`idempotency_conflict` means the same actor/key was reused with different input or evidence.
After an uncertain network or `operator_result_unavailable` response, retry the exact
body, key, and evidence reference: identical retries return the original policy response
for seven days. Receipts expire after seven days and are cleaned on the next mutation.
The public nonsecret `GET /v1/client/update-policy` returns the same policy in `data`,
with revision zero and an empty floor before the first mutation.

This floor controls CLI protocol/support availability, not artifact selection or trust.
Signed TUF `minimum_permitted_version` remains authoritative for updater artifact
activation and recovery; setting or clearing this support policy cannot bypass signature,
integrity, freshness, or rollback verification. Only requests marked
`X-Paperboat-Client: paperboat` and `X-Paperboat-Distribution: official` are checked
using `User-Agent: paperboat/<version>`. Local/custom builds and requests without
the official distribution marker are exempt. These headers describe compatibility;
they do not authenticate the caller.
After the enforcement date, feature requests below the floor or with malformed versions
receive HTTP 426 `update_required`, with `minimum_version` and public `reason` details.
Login, token refresh, logout, policy reads, update/release downloads, support diagnostics,
and DELETE/cancel/stop/revoke/disconnect/release cleanup remain usable. Browser sessions,
unmarked machine agents, runtime heartbeats, and already established channels are unaffected.
Policy read outages fail new marked feature requests with an actionable 503 while those
escape paths remain available; authentication and authorization still apply to each route.

Fresh operator setup is covered by migration 055: operator IDs use the production
opaque UUID generator; schema and actor audits resolve authoritative operator rows
instead of the retired ID prefix. Role, CSRF, Origin, session revocation, and unknown
actor rejection remain enforced.

# Operator backend

Paperboat operators use the server's `/v1/admin` APIs, or the `admin/` web
panel (a separate Next.js app on its own origin that relays these APIs). Operator accounts and sessions are stored separately from
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
revokes it. Operator sessions last eight hours. Five failed passwords (or
second-factor codes, together) lock an account for fifteen minutes and end its
pending sessions.

### Second factor (required)

The password step alone gives only a pending session (15 minutes) that the API
refuses everywhere except the MFA routes; `login` and `setup` answer
`"mfa": "enroll"` or `"verify"`. Every operator must use an authenticator app
(RFC 6238 TOTP: 30-second steps, 6 digits, SHA-1, one step of clock skew;
prior art: Teleport `lib/auth/password.go` checkOTP and its used-token store).

- First sign-in: `POST /v1/operator/mfa/enroll` returns the key and an
  `otpauth://` URI; `POST /v1/operator/mfa/enroll/confirm` with `{"code":"…"}`
  confirms it, returns ten single-use recovery codes (shown once; only SHA-256
  hashes are stored) and completes the session.
- Later sign-ins: `POST /v1/operator/mfa/verify` with a TOTP code or an unused
  recovery code. A code works once (a replayed step is refused).
- `POST /v1/operator/mfa/recovery-codes` with a fresh TOTP code (from a complete
  session) replaces the recovery codes; the old ones stop working.
- The TOTP secret is encrypted with `PAPERBOAT_ENCRYPTION_KEY`; without a key the
  MFA routes answer `mfa_unavailable` and nobody can finish signing in.
- Every enrolment, verification, failure, recovery-code use and replacement is
  audited (`operator.mfa.*`).
- Lost authenticator and no recovery codes: an operator cannot recover
  themselves. With database access, delete the operator's `operator_totp` row
  (`DELETE FROM paperboat.operator_totp WHERE operator_id = '…'`); the next
  password sign-in enrols a new authenticator. Do this only after confirming the
  person out of band.

### Managing operators (admin only)

The panel's Operators page (and these routes) let an admin run the staff list. Every write is audited
(`operator.operator_*` receipts plus the `operator.request` trail) and needs the usual `Operator-Target`,
`Operator-Reason` and `Idempotency-Key` headers.

- `GET /v1/admin/operators` lists operators with role, status, whether an authenticator is enrolled, whether
  setup is still pending and how many sessions are live. `GET /v1/admin/operators/{id}/sessions` lists one
  operator's live sessions.
- `POST /v1/admin/operators` (target `operators`, body `{"email","role"}`) creates an operator with no
  password and returns a single-use setup token valid for 72 hours, **once**. Give it to the person out of
  band. They choose "I have a setup token" on the panel's sign-in page (the email step never reveals who has
  an invitation), enter their email, the token and a password (`POST /v1/operator/setup` with `token`),
  then enrol an authenticator. The receipt does not keep the token: if it is lost, invite the same email again
  with a new Idempotency-Key and a fresh token replaces the old one.
- `POST /v1/admin/operators/{id}/disable` ends all the operator's sessions at once and blocks every sign-in;
  `…/enable` allows sign-in again (old sessions stay ended). You cannot disable yourself or the last active admin.
- `POST /v1/admin/operators/{id}/mfa-reset` deletes the operator's authenticator and recovery codes, clears any lockout and ends
  their sessions; their next password sign-in enrols a new authenticator. Use it only after confirming the
  person out of band. You cannot reset your own.
- `POST /v1/admin/operators/{id}/sessions/revoke` with `{"session_id":"<id from the list>"}` or `"all"`.
  You cannot revoke your own (sign out).

## Reads and status

Use `limit` (1–100, default 50) and `after` for list pagination. List cursors are
opaque resource IDs, not evidence of authorization. Every list page also carries
`total` (rows matching the filters, capped at 100000 with `total_capped: true`) so
a panel can show "a–b of N". Billing/control lists accept allowlisted `state`
filters. Accounts take `q` (email, name or id contains) and `status`; teams take
`q` and `suspended`.

- `/v1/admin/overview`: account, billing, operation and infrastructure counts.
- `/v1/admin/accounts` and `/{account_id}`: safe identity/status/version and the
  personal billing workspace ID.
- `/v1/admin/machines` and `/{machine_id}` (support, billing, operations): owner
  account/team, platform, state, freshness (`ready`/`stale`), versions and
  capabilities. Filters `q`, `state`, `platform`, `stale`, `deleted`. Removed
  machines are hidden unless `deleted=true`. No workspace paths or file contents.
- `/v1/admin/audit` (admin): search the whole audit log, newest first. Filters
  `actor`, `event_type`, `resource_type`, `resource_id`, `outcome`, `since`, `until`
  (RFC 3339); `after` is the sequence number of the last row seen.
- `/v1/admin/analytics/status` and `/overview?days=7|30|90` (admin): product
  analytics read from PostHog's Query API by the server (visits, page views,
  console users, funnel, top pages, referrers, countries, devices). Aggregates
  only, cached for ten minutes. Configure `PAPERBOAT_POSTHOG_HOST` (https origin),
  `PAPERBOAT_POSTHOG_PROJECT_ID` and `PAPERBOAT_POSTHOG_PERSONAL_API_KEY` (or
  `_FILE`; a personal key with `query:read` only). Unset: status says not
  configured and overview answers `analytics_not_configured`. Retention and
  console country/device views need events the sites do not send yet.
- `/v1/admin/traffic?period=today|7d|30d|month` and `/v1/admin/traffic/nodes?period=&kind=relay|edge` (operations): how
  many bytes went direct (machine to machine, as the hosts report them), through our relays and edges and through
  customers' own, with a time series, per-category coverage (reporting of expected machines or nodes) and a per-node
  table of our own nodes. Hour totals (`traffic_hourly`) are folded into day totals (`traffic_daily`) after
  `retention.traffic_hourly` (8 days) and the days pruned after `retention.traffic_daily` (400 days). Counts only: no
  connection, address or content. "month" is the calendar month in UTC.
- `/v1/admin/infrastructure/capacity` (operations): every edge, relay and control-plane
  server with CPU (now and a five minute average), memory, connections, version,
  last heartbeat, location, and bytes today and this month (UTC) against the
  per-node monthly allowance (`node_capacity.monthly_traffic_bytes`, default 20 TB;
  an edge counts bytes in plus out, so a proxied download counts twice). Warnings:
  edge sustained CPU at or above `edge_cpu_warn_percent` (50), relay memory at or
  above `relay_rss_warn_bytes` (1 GiB), month traffic at or above
  `traffic_warn_fraction` (80%) of the allowance, and a stale heartbeat. Edges and
  relays report this in their heartbeat; **deploy the server before the edges and
  relays**, since an older server rejects the new field. Day totals live in
  `node_capacity_daily` and are pruned after `retention.node_capacity_daily` (90
  days).
- `/v1/admin/fleet/versions`, `/rollouts` and `/offline` (admin, operations): the Fleet page. Versions counts
  machines, users and teams per pb version and OS (the version each machine reports in its heartbeat), the oldest
  version still online and the update failure reasons machines report. Rollouts takes each promoted release from
  the signed release decisions and counts updated, still older, failed or rolled back, and older machines last seen
  before the rollout began (offline for the whole window, so they could not have updated); its chart reads
  `fleet_version_daily`, one row per day, OS and version, rewritten every 10 minutes and pruned after
  `retention.fleet_daily` (400 days). Offline lists machines not seen within `user_machines.offline_after`, by how long
  unseen (under 1 day, 1 to 7, 7 to 30, over 30, never), with real totals and offset paging.
- `/v1/admin/teams` and `/{team_id}`: ownership/generation, suspension and bounded
  membership pages, plus the team's billing workspace ID.
- `/v1/admin/billing/workspaces` and `/{workspace_id}`: subscription state, paid
  quantity, period and cancellation status; no provider credentials or checkout URLs.
- `/v1/admin/billing/workspace-operations` and `/{operation_id}`: recovery targets.
- `/v1/admin/control-operations` and `/{operation_id}`: control recovery targets.
- `/v1/admin/transactional-mails` and `POST /{mail_id}/redrive`: mail that stayed undelivered after 8 attempts or 23 h (invitations, deletion confirmations). Redrive puts one back in the queue with fresh attempts; a sent mail is refused. Admin: Servers, Operations tab, "Failed mail" (Overview counts it as `transactional_mail`).
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
`POST /v1/admin/accounts/{id}/sessions/revoke` (body `{"revision":<account version>}`) ends the same
credentials (browser sessions and access grants, CLI sessions and tokens, API tokens, device grants,
machine access sessions) without suspending: use it when an account may be compromised but the
customer should simply sign in again. Hosts are woken, so open SSH sessions end within seconds.

Plan grants (support comps): `POST /v1/admin/billing/workspaces/{workspace_id}/grant` with target `personal:<account>`
or `team:<team>` and `{"revision","product_code","days"[,"quantity"]}` (days 1 to 366; seats 1 to 128 for a team plan) gives
the workspace an active plan with no Stripe subscription; `.../revoke` ends it. The grant is the workspace's own
subscription row, so the plan limits, team entitlement and bandwidth allowance follow the product like a paid plan, it ends
by itself at `expires_at`, and the Stripe webhook never touches it (it matches workspaces by their Stripe ids). A grant
is refused (409) while the workspace has a live Stripe subscription or a checkout in flight; a later paid checkout replaces it.
While a grant runs (`grant_until`), a Stripe event for the workspace is applied only if its subscription is live and paid
(active, read from Stripe at the time, not from the event), so a retried or replayed event of any age for an old
subscription cannot overwrite the grant.
Revoke only ends a grant; a paid plan is cancelled in Stripe. The audit event records product, days and seats.

Entitlements (extra bandwidth and limit overrides): `GET /v1/admin/billing/workspaces/{workspace_id}/entitlements` reads the
workspace's bandwidth (plan allowance, granted bytes, usage), every counted limit (machines, previews and tunnels, browser
terminals, public TCP tunnels, viewers per shared terminal) with the plan's value and any override, a team's seats, and the
grants. `POST .../entitlements` with target `personal:<account>` or `team:<team>` and `{"entitlement","amount"[,"ends_at"]}`
grants `bandwidth` (bytes, running grants add up and add to the period's allowance; an unlimited allowance stays unlimited) or
overrides one limit kind (a value of at least 1, higher or lower than the plan's; a newer override of the same kind replaces
the older one; `seats` only for teams). `ends_at` is optional and must be in the future; the grant stops counting at that
instant by itself. `POST .../entitlements/revoke` with `{"grant_id"}` ends one grant now. Enforcement (machine, preview,
tunnel, terminal and seat admission, the bandwidth gate, and the usage the customer sees as "granted") reads plan plus grant
from one place, `internal/planlimits`; an override that lowers a limit below current use deletes nothing, it only refuses new
use. A seats override sets what invitations may fill; it does not grow the team's public TCP pool (override `public_tcp` too) and directory sync still buys seats from the bought count. SDK limits are the same for every plan and cannot be overridden. Each write needs the operator's reference and an
idempotency key and is audited as `operator.entitlement_grant` or `operator.entitlement_revoke` in the same transaction.
Bans (`POST /v1/admin/accounts/{id}/ban`, `.../unban`, `POST /v1/admin/teams/{id}/ban`, `.../unban`; admin only, typed id in the panel,
`revision` = the version or generation just read, the reference in `Operator-Reason` is the ban's recorded reason). An account ban, in one
transaction: the account is suspended and every browser session, CLI login, API/recipe token and machine access session ends (existing
`revokeUserAuthority`); its peer certificates are revoked so its machines lose network access (the status change also advances the relay
revocation generation); every team it owns is suspended with it (members lose access; each gets its own ban row, `cause_ban_id` = the
account's); a `bans` row keeps the reference, the WorkOS subject and the canonical e-mail (case-insensitive, `+suffix` removed), and
sign-in or sign-up with that subject, that e-mail or that account answers 403 `account_banned` and creates nothing. Optional body
`subscriptions`: `keep` (default), `period_end` (Stripe `cancel_at_period_end`) or `immediate` (cancel now): queued in the deletion
outbox (`account_deletion_jobs`, kinds `stripe_cancel_at_period_end` / `stripe_subscription`) for the account's personal and its owned
teams' subscriptions, run after commit with retries. Optional `block_ips` (up to 20 addresses or ranges): blocked with the same
reference. The data stays. The edge answers the takedown page (403 `publication_blocked`) for the hostnames (and custom domains) of the banned account's and
banned teams' previews and tunnels (`blocked[]` in the ingress snapshot, within one 5 s poll; a name beyond the edge's 4096-entry bound is withheld and answers "does not exist").
A ban waits for a sign-in in flight (same canonical e-mail lock) and then revokes the session it created; banning an account that is already banned is a conflict (unban first). Unban lifts the ban rows (the account's
and the team bans it caused), restores sign-in, and does not undo cancelled subscriptions or network blocks; the owner's machines join
again with a new `pb login`. A team ban (`team_ban`) suspends the team, records the reference and takes `subscriptions` the same way.
The admin panel shows "Banned" and the reference on the user and team pages and lists, and filters on it.

IP blocks (`GET/POST /v1/admin/ip-blocks`, `POST /v1/admin/ip-blocks/{id}/remove`; admin only): a block is an address or CIDR (IPv4
not wider than /8, IPv6 not wider than /16) with the operator's reference and an optional end (`ends_at`). While one runs, sign-up,
sign-in (WorkOS callback) and CLI login (device authorize and token poll, API-token sessions) from inside it answer 403 `ip_blocked`;
existing sessions are not cut by address. Operator login is deliberately not blocked by address, so an operator can always lift a block; blocked attempts leave no audit row (the recent-addresses list shows only addresses of sign-ins that happened). The address is the trusted client address the server derives behind its proxy
(`PAPERBOAT_TRUSTED_PROXY_CIDRS`; the rightmost `X-Forwarded-For` entry inside the trusted ranges, never a client-supplied header).
`GET /v1/admin/accounts/{id}/ips` (admin only) lists the addresses of the account's recent sign-ins, sign-ups and CLI logins, read from
`audit_events.client_ip`, so they exist for as long as the audit log keeps events (the plan's audit retention); each is marked when
a running block contains it. Blocks and ranges are audited (`operator.ip_block_add`, `operator.ip_block_remove`).
Relay drain stops new scheduling; resume requires a new heartbeat before ready.

Draining a managed node has two actions. **Drain** (`drain`) stops new connections: the node leaves placement and the
wildcard DNS at once, machines that hold a carrier on it keep it until the drain deadline (10 minutes), then use the
region's other nodes (or the backup region). **Drain and move** (`drain-move`) also wakes every machine attached to
the node so it re-attaches now, and for a relay lists the accounts it serves for handover; the node's carriers end
after 90 seconds (longer than the wildcard record's TTL, so a visitor holding the old address still reaches a carrier
until it expires). Both are audited (`operator.infrastructure.node.drain`, `...drain-move`), take `{"revision": n}`
and the usual `Operator-Target`, `Operator-Reason` and `Idempotency-Key` headers. Node responses carry `carriers`,
the number of machines holding a live carrier on the node: retire a node only when it is 0. They also carry `load_percent` (absent when the node reported nothing in 2 minutes), `in_rotation` (false while the load rule holds a ready node out: an edge out of the wildcard DNS names, a relay out of the relay candidate list; see preview-tunnel-control-plane.md) and `ingress_verified`. `resume` cancels a drain
and its handovers; the node is ready again after its next heartbeat.
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

Paperboat's own relays and edges are installed exactly like a self-hoster's: `pbh install --region <id>` on the host,
then claimed. A node claimed from the admin panel (Self-hosting → Enrol a node) is **global**: it serves every account,
and its install region is an operator region. The claim creates that region (enabled, display name = id) if it does not
exist; rename it with `POST /v1/admin/regions/{region_id}/update`. A node claimed from a customer's console is
self-hosted and is none of this. There is no separate enrolment or credential for managed nodes: the claim writes the
node's runtime credential, and the serving set, wildcard DNS, drains and ingress verification read global installations.

Region actions are `create`, `update`, `enable`, `disable`; node actions are `drain`, `drain-move`, `resume`, `retire`,
`verify-ingress` (with the node's current `revision`) and `site` (`{"site":"Mumbai"}`, no revision). They require the
full admin role. Operations staff may read `/v1/admin/regions` and `/v1/admin/nodes`. Lists use bounded
`limit`/`after`; nodes optionally filter by `region`.

New nodes are unready until actual runtime registration and fresh observations. Disabling a region refuses its nodes'
heartbeats and selection, and drains them. Re-enabling does not silently resume nodes: explicitly resume them. To move
a node to another region, remove the installation and install it again with the new `--region`. These APIs manage
actual machines; they do not purchase or create cloud VMs.

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
`paperboat-relay`, and `paperboat-edge`. Reference correlation reports
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

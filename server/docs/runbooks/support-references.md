# Support references and Sentry

A user supplies **one PB reference**, `support_` followed by a canonical lowercase UUIDv4.
It identifies an operation, including retries, not one HTTP request or one Sentry event.
`Support-Reference` propagates on PB control API requests/responses. Error envelopes use
`error.support_reference`. Existing request IDs, resource IDs, correlation IDs and billing
idempotency keys retain their internal meanings; they are not additional support tokens.
Never inject this header into tunneled application traffic.

## Configuration

Reporting is disabled by default. Server environment variables:

- `PAPERBOAT_SENTRY_ENABLED=true` explicitly enables Sentry.
- `PAPERBOAT_SENTRY_DSN` (or `_FILE`) supplies the project DSN over HTTPS.
- `PAPERBOAT_SENTRY_RELEASE` identifies the exact deployed artifact, such as a Git revision.
- `PAPERBOAT_SENTRY_ISSUE_URL` optionally supplies the trusted HTTPS issues page, without
  credentials, query or fragment (for example `https://example.sentry.io/issues/`).

Environment is inherited from `PAPERBOAT_ENV`. No API authentication token is needed to
capture errors. Keep Sentry administration/source-map upload tokens in deployment/build
secret storage; never expose them to endpoint/browser configuration. Disabling reporting
requires no Sentry connection and preserves server evidence and reference lookup.
CLI/daemon and browser reporting are opt-in; see their repository configuration docs.
CLI local evidence uses the running daemon’s existing safe diagnostic ring. If that daemon
is unavailable too, preserve the safe terminal/JSON output: no retained local report is
guaranteed, and PB does not create a second unbounded spool.
Enabling reporting now defaults to structured operational logs and application metrics,
with 10% trace sampling. `PAPERBOAT_SENTRY_LOGS_ENABLED=false`,
`PAPERBOAT_SENTRY_METRICS_ENABLED=false`, and
`PAPERBOAT_SENTRY_TRACES_SAMPLE_RATE=0` independently disable these signals.
The rate must be a finite number from 0 to 1. The overall enable flag remains the
consent/deployment switch; selecting products in Sentry does not turn it on.
There is no automatic raw log forwarding, session replay or user payload capture.
Dashboard instrumentation is outside this milestone.

## APIs and authority

`GET /v1/admin/support/{reference}?owner_id=<user ID>` requires a current authenticated
operator session with `support` or `admin` role. Customer browser sessions, team roles and paired CLI credentials
do not grant this authority. An optional owner qualifies the lookup. With only the reference, staff receive matching
records grouped by authenticated owner, including a separate unattributed group.
Client-supplied reference collisions never merge ownership; the response flags truncation. A reference never grants access. The response
contains at most 128 earliest records across owners (32 per owner/reference), safe source identity, release, stage/code, internal
request/event IDs and optional Sentry search links. Sentry search can locate client reports
that never reached PB; client-originated telemetry is untrusted diagnostic evidence.
Do not use it to authorize actions or infer tenant ownership.

`GET /v1/support/{reference}` requires browser or paired CLI authentication and returns only
the authenticated user's report availability/reporting status. It cannot expose stacks,
other owners, Sentry event IDs or links. Both endpoints are `no-store`. Every successful
lookup, including a miss, requires a durable audit write. Audit/storage failure returns 503
without evidence. Revoked staff roles are evaluated by the separate operator session authentication
on each request. WorkOS MFA policy remains a deployment responsibility; this integration
does not claim to establish MFA assurance.

## What is recorded

The server captures 5xx failure codes and panic source stacks at the failure boundary,
plus unexpected HTTP/control-sync listener and required-worker failures.
Expected 4xx rejections are not error events; operational logs/metrics record their rejected outcome. Panics retain their original goroutine stack;
raw panic values are not logged or reported. The SDK event is reconstructed from approved
reference/component/stage/code/release fields and code frame identity, removing absolute
paths, source context, locals, hostnames, requests, headers, bodies, cookies, URLs, user
profiles, raw error messages and arbitrary breadcrumbs. The response explanation remains
separate from internal source evidence. Sentry grouping includes the stack, so unrelated
`internal_error` locations are not merged solely because they share an HTTP error code.

The core response never waits for Sentry or support storage. Each process admits at most
120 failure records/minute into a queue of 128; a worker uses a two-second storage deadline
and a two-second Sentry HTTP timeout. Its lifecycle is independent of product workers;
shutdown drains with a two-second budget plus at most one in-flight two-second send,
then closes SDK resources before the database. The SDK transport queue is bounded to 32 envelopes. Excess evidence is dropped, not silently called sent.
The first 32 stored failures per owner/reference are retained; later retries do not replace
the initiating failure. No automatic raw-log upload or persistent outbound payload spool
is introduced. SDK capture is labeled `unconfirmed`, never `sent`; disabled and filtered
states are explicit. A missing record can mean offline/local operation, filtering, capacity
loss, storage outage, or expiration; keep the local safe diagnostic export when applicable.


## Operational coverage and signal semantics

| Boundary | Evidence exported |
| --- | --- |
| Server control HTTP | Fixed product family (identity, accounts, teams, billing, admin, machines, terminal/SSH, environment, configuration, files, tunnels/previews, releases, edge/network, support), outcome and duration; safe PB reference and trace linkage |
| WorkOS, Stripe, GitHub and other configured provider clients | Dependency span and outcome/latency, without outgoing diagnostic headers or provider URLs/responses |
| PostgreSQL | Query outcome and duration as child spans where an operation context exists; otherwise bounded independent operations; pool acquired/idle/total, acquire wait/cancellation snapshots; no SQL or arguments |
| Background workers | Named start/termination/failure/cancellation and supervised panic evidence; DNS/certificate, preview lease, connector/configuration and health events from existing typed producers; team retention/entitlement/billing reconciliation results |
| Existing server metrics | All registered bounded metrics and platform counters: identity/connect, terminal operations/retries/snapshots, credentials/revocations, node/route/usage, DNS/certificates, connectors, streams/bytes/queues and request duration |
| Process/exporter | Goroutines, heap bytes, local report drop and cleanup failure counts, plus per-signal configuration/status |
| Native CLI/daemon | Fixed command outcomes, owned control HTTP, approved lifecycle/diagnostic failure/recovery and existing runtime metric snapshots; see native operations runbook |
| Relay/tunnel | Service and owned control dependencies, admission/session/carrier outcomes, capacity/health/backlog and existing cumulative traffic/usage metrics; see their operator runbooks |

Errors, logs, transactions and application metrics are native Sentry SDK signals,
not logs or counters disguised as exceptions. Logs have a fixed message and approved
fields. Sampling traces does not suppress public PB references. Standard `sentry-trace`
parentage is accepted on owned control requests; no baggage is imported and no trace
headers are injected into encrypted peers or tunneled customer traffic. A relay sees
transport operations and counts, not application content inside encrypted packets.

Detailed operation logs/traces/metrics have independent process rate bounds: 600 logs,
300 span starts and 600 detailed metric observations per minute. Cumulative snapshot
exports have a separate 6,000-point minute admission budget. A combined snapshot
rotates through at most 100 series every five seconds, plus startup and shutdown,
with at most 2,048 candidate series. The current complete 779-series vocabulary
fits eight batches (a 40-second cycle); active cardinality determines actual
refresh intervals. `snapshot_deferred` reports observations deferred to later
rotations. Detailed operation limits do not displace these snapshots. The existing registries retain
the authoritative unsampled totals. Counters export as cumulative gauges with `_snapshot`;
histogram sum/count use `_sum_snapshot`/`_count_snapshot`; gauges keep their names.
Detailed `paperboat.operation.count` and duration are bounded observations, not billing
or authoritative traffic totals. Metric dimensions exclude references, traces, account,
resource and machine identifiers. Per-component snapshot queries show the maximum process
value per time bucket, not a sum across replicas. Missing samples are unknown, never zero
health or proof of no failures. Provider receipt, retention and quota exhaustion must be
checked against the deployed Sentry project.

Server support records retain only bounded `operation`, `stage`, producer-owned `code`,
classified `cause`, named Go `error_type`/`error_chain`, bounded `errno`, outcome, severity,
and request correlation. An HTTP response status is stored numerically in `http_status`,
while its cause uses the same finite classifications as other failures. Raw `Error()` text
is not stored or exported. The support reference is stable across the request; HTTP status
and PostgreSQL/network causes remain useful when Sentry is disabled. A worker failure is recorded on the first iteration and again only
when its failure class changes or a successful iteration recovers. Cleanup counts include
each failed cleanup pass; its support record and log are emitted on failure and recovery
transitions rather than once per retry.

`GET /v1/admin/reporting` reports process configuration, report drop counts, and cleanup
failure counts under `signals` even when Sentry is disabled; the
[operator runbook](operator-admin.md) documents multi-project errors/logs/traces/metric
APIs and partial-capability responses. Existing billing, account/team, infrastructure
and backup APIs remain the authoritative product/operational state. Diagnostic events
are not audit records or billing evidence. A disabled Sentry integration leaves those
APIs and local diagnostic sources usable.

## Retention, deletion and rollout

Server evidence expires after seven days. Lookups exclude expired rows immediately and a
minute worker deletes up to 1,000 expired rows per pass. This does not delete Sentry events.
Before enabling reporting, the operator must select the Sentry project's retention and
access policy and document it for the deployment. For early deletion, use the Sentry
project's supported event/issue deletion controls; deleting PB evidence alone is not an
upstream deletion request. No terminal/file/ENV content belongs in either system.

Apply the server's normal migration process (baseline migration 1) before running this version.
Configure the precise release on every artifact. Before enabling production reporting,
exercise a synthetic failure, look it up by the displayed PB reference, confirm receipt
in Sentry and readable frames/source maps for that exact release, and check project volume
and retention settings. Local SDK envelope tests do not establish SaaS acceptance.

The design borrows support markers from [Tailscale](https://tailscale.com/docs/account/bug-report),
explicit shareable diagnostics from [NetBird](https://docs.netbird.io/help/troubleshooting-client),
and indexed request investigation from [Cloudflare](https://developers.cloudflare.com/fundamentals/reference/cloudflare-ray-id/).
These are reference-ID patterns, not claims that those products use Sentry internally.
Sentry handles issue grouping and stack inspection; PB owns operation correlation and access.

### Hosted fleet attribution and metric meaning

Hosted server, relay, and tunnel processes accept `PAPERBOAT_SENTRY_REGION` and
`PAPERBOAT_SENTRY_INSTANCE` together (server JSON: `reporting.region` and
`reporting.instance`). Region is a public deployment label matching
`[a-z][a-z0-9-]{0,23}`; instance is a stable slot from `slot-01` through `slot-64`.
Assign distinct slots to simultaneously running processes within each
component/region. Reuse slots after replacement; do not generate boot IDs or
copy hostnames, node IDs, customer IDs, or machine identifiers. Both labels may
be omitted; partial or invalid configuration fails. These operator-controlled
labels accompany errors, safe logs, traces, and metrics. Native CLI/daemon
reporting deliberately has no persistent fleet identity.

`GET /v1/admin/sentry/metrics` accepts an optional **pair** of `region` and
`instance` filters for hosted components. It returns `scope=deployment_slot`
when selected, otherwise `scope=component_population`. The response explicitly
names its semantics: operation counters are sums of exported observations,
operation durations are p95 of exported observations, and gauges (including
cumulative counter/histogram snapshots) are interval maxima across matching
observations and dimensions. Population maxima are not fleet totals, slot
maxima are not the last value, and exported operation counts can be lower than
actual work when reporting drops observations. Do not sum cumulative snapshot
observations or subtract maxima across process restarts.

`interval_seconds` and `last_observed_bucket` describe the aggregation window.
`freshness=unknown` is deliberate: this provider API does not return the last
observation time within each bucket. Empty buckets remain absent, never zero or
carried-forward replica values. Historical buckets do not establish that a
replica is alive now; use the independent watchdog/check-in status for that.
A zero is returned only when the provider reports a real sample. The endpoint
does not claim exact current fleet totals or use top-N grouping that could
silently omit replicas. Select each desired public deployment slot explicitly.

### Backup and restore operational health

The process-owned health worker reads the latest authoritative backup and restore
evidence at startup and every minute, with a two-second database-query deadline.
It exports `paperboat.backup.state` and `paperboat.restore.state` as fixed integer
gauges: **0 unknown, 1 current, 2 stale, 3 failed, 4 expired, 5 not_configured,
6 unavailable**. `paperboat.backup.age_seconds` and
`paperboat.restore.age_seconds` exist only when evidence was actually read.
Latest evidence is ordered by completion time, so replaying older success cannot
hide newer failure. Configured backup/restore maximum ages and backup retention
control freshness; these are evidence claims, not independent storage-durability
verification. No database identifier, run identifier, credential, or failure text
crosses this metric boundary. Existing authenticated backup evidence routes keep
their authorization and audit requirements.

Set `PAPERBOAT_SENTRY_HEALTH_MONITOR` (`reporting.health_monitor`) to an explicitly
provisioned one-minute cron monitor to detect a missed or hung health worker.
This requires enabled Sentry reporting. Each run checks in successfully only when
both backup and restore evidence are current; unknown, missing configuration,
stale, failed, expired, unavailable, or canceled checks do not claim health.
Without this optional monitor, the state gauges still export when metrics are
enabled. Gauge bucket maxima are historical observations, so consult current
check-in status for alert/recovery state.

### Background reconciliation progress

The mounted server background workers report actual reconciliation iterations,
including successfully checked empty batches. Merely running a goroutine or
receiving a timer tick never advances progress. Registration happens before
workers start, so a worker that never enters its loop remains unknown and then
stale. The registry has 20 fixed worker names and a hard 32-entry bound; optional
unmounted jobs are absent. Retriable loop errors remain failed until an actual
successful iteration, and cancellation never becomes success.

`paperboat.worker.state` has fixed values **0 unknown, 1 current, 2 running,
3 failed, 4 stale, 5 canceled**, with a fixed `worker` enum label.
`paperboat.worker.progress_age_seconds` exists only after a real iteration
finished (successfully or unsuccessfully). A first running iteration is warming,
not known healthy. A running retry retains the previous failed state until it
completes successfully. The authenticated metrics endpoint accepts `worker` for
these two metrics, optionally together with the hosted region/instance pair.
As with other gauges, historical interval maxima are not current state; the
operational-health cron provides the current combined health/recovery signal.

An idle worker becomes stale after five of its actual configured intervals plus
one minute. A continuously running iteration, or a registered worker that never
started, becomes stale after `PAPERBOAT_WORKER_STALL_TIMEOUT` (JSON
`reporting.worker_stall_timeout`, a duration encoded in nanoseconds). The default
is **30 minutes**, configurable from one minute through 24 hours. This is an
explicit observation threshold, not a new job cancellation deadline. Some DNS
batches perform 100 sequential observations; certificate issuance and
propagation can take longer. Configure this threshold for the deployment's
validated worst complete iteration rather than assuming a request timeout is a
whole-batch timeout.

The existing operational-health cron combines backup/restore freshness with
all mounted workers. It skips check-in creation while initial workers are still
within their startup allowance and no worker is known failed; it never sends a
fake OK during warming. Configure that monitor's startup/missed-check-in margin
to cover this allowance. Once warming completes or exceeds the allowance, runs
report OK only when every worker is healthy and both backup/restore states are
current. Known worker failures are not suppressed by another warming worker.
This uses one monitor, not one paid schedule per worker. At most 44 health metric
observations are produced per minute alongside the independently bounded metric
snapshot collector. The progress wrapper covers lifecycle exits/panics, while
actual loop hooks cover machines, configuration, previews, connector rotation
and drain, DNS, team jobs, edge reconciliation, diagnostic cleanup, and mounted
certificate workers. Per-item durable retry outcomes remain the owning
subsystem's operational metrics; successful batch completion means the batch
finished, not that every external dependency succeeded.

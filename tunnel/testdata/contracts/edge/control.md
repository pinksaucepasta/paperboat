# Edge Control Contract 1.0

The control plane is authoritative for environment, connector generation, route intent,
entitlement, and node assignment. The edge owns live connector observations, attachment,
forwarding, and byte counters. Messages are authenticated with the exact credential class
declared by their schema and use a unique operation ID.

Connector admission consumes a single-use `connector_admission` credential atomically with
recording `(environment_id, machine_id, connector_id, connector_generation, edge_pool)`. The generation
must equal current desired state. A retry of the same operation and canonical request returns
the recorded decision. Reuse with different data, a stale generation, wrong node/pool, or a
revoked environment fails before a connector is accepted. Replacing a connector advances
generation, drains the old connection, detaches its routes, and rejects late traffic from it.

Admission responses include the assigned `edge_node_id`, an authenticated HTTP/3 and
HTTP/2 CONNECT endpoint, and at least one operation-bound route handoff. The connector
configures only those handoffs and never derives endpoint or public-host values. Readiness
is reported only after the native carrier and every route are active. Reconnects resume the
generation only while admission remains unexpired and unrevoked. A new generation requires
a fresh single-use admission.

Route attachment requires an admitted live connector with matching environment, generation,
edge node, route revision, and protocol. Preview host ownership is exclusive. Duplicate
attachment is idempotent; another environment receives `route_conflict` without disclosing
the owner. Stale detach cannot remove a newer route. Draining nodes accept no new assignment
and retain existing streams until their deadline before explicit termination.

Usage is reported per `(edge_node_id, counter_epoch, environment_id, route_id, direction, authority_id)`
as an absolute monotonically increasing byte counter, never a delta. A process restart uses
a new random counter epoch and begins at zero. The server persists the greatest counter per
epoch and computes deltas transactionally; duplicate or lower observations add no usage.
Reassignment starts a new route ownership revision and may overlap old reports without
double-counting because route, node, and epoch identities remain distinct. Reports carry
observed interval bounds and an operation ID. An uncertain delivery is retried unchanged.

`authority_id` is covered by the edge signature. Restricted streams use their
persisted browser grant reference; public streams use the exact publication and
generation. The server binds that authority to one personal or team workspace
at admission, including the exact node, environment, route and revision. Sharing
alone never changes the payer. A report retry cannot change its authority.

The operational delivery window is seven days. Metered streams reconnect after
at most seven days, and the minimal admission-to-payer mapping survives that
stream lifetime plus the delivery window (fourteen days after admission expiry).
Configured non-null workspace allowances deny new admission when their current
period is exhausted; an unset allowance imposes no quota. Existing stream pacing
and connection protections continue to apply.
Mapping admission and producer counter identities are bounded at 4096 per edge
node, matching the existing durable report queue. Capacity exhaustion applies
backpressure; retained billable reports are not silently discarded. Scoped epochs
also rotate by seven-day window so pruning delivered counters cannot reset an
existing server absolute-counter identity.

Signed reports older than seven days receive a durable `expired_not_billed`
receipt with observed bytes and zero billing debit, even if their payer mapping
has expired. The counter still advances, preventing later cumulative reports
from charging those bytes again. Exact retries return the original disposition.
The producer removes a pending report only after the server acknowledgement;
reconnection therefore drains expired reports and restores fresh reporting.
An ordinary accepted report returns `accounted`, with persisted
`quota_exhausted` feedback. On exhaustion the edge closes active streams using
that immutable authority. Other grants/workspaces sharing the route remain
unaffected. Streams admitted after the reported interval ignore delayed old
feedback, allowing recovery after an allowance or period change. This is
post-usage enforcement, not a hard byte ceiling: in-flight traffic, queued reports
and unavailable control connectivity can produce overshoot before acknowledgement.
 Self-hosted nodes retain their
existing no-debit boundary. Native direct traffic introduces no new byte charge.

All decisions return a stable request ID. Logs and metrics may contain environment, node,
generation, route revision, result, and bounded byte counts, but never credentials, public
signed URLs, target content, headers, or provider-specific secrets.

No candidate or health observation grants resource access. Native carrier admission,
authenticated regional assignment, and ready-route DNS publication are the only supported
runtime path.

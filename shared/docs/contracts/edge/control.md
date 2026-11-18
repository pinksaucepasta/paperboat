# Edge Control Contract 1.0

The control plane is authoritative for environment, connector generation, route intent,
entitlement, and node assignment. The edge owns live connector observations, attachment,
forwarding, and byte counters. Messages are authenticated with the exact credential class
declared by their schema and use a unique operation ID.

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
A workspace whose configured allowance (its fair-use amount) is used up in the current
period is never refused: its bindings are admitted and recorded like any other, and the
ingress snapshot lists its hostnames in `throttled` (hostname, workspace, period end) next
to their decisions. The edge paces all routes of one workspace through one shared token
bucket at the throttled rate (`edge_ingress_limits.throttled`, delivered in the heartbeat)
until the period end or until the server stops listing the workspace. An unset allowance
imposes no throttle. Existing stream pacing and connection protections continue to apply.
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
An ordinary accepted report returns `accounted`. Usage never closes a stream: past
the fair-use amount the edge slows the workspace (above), so a delayed or lost
report only delays the throttle. Self-hosted nodes retain their existing no-debit
boundary and are never throttled. Native direct traffic introduces no new byte charge.

All decisions return a stable request ID. Logs and metrics may contain environment, node,
generation, route revision, result, and bounded byte counts, but never credentials, public
signed URLs, target content, headers, or provider-specific secrets.

No candidate or health observation grants resource access. Native carrier admission,
authenticated regional assignment, and ready-route DNS publication are the only supported
runtime path.

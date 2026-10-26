# Control-Plane Alerts

The server `/metrics` endpoint is localhost-only. Each helper also exposes metrics only on
its configured numeric loopback listener. Scrape both through trusted host-local
collectors; never expose or route either endpoint publicly.

## Required Alerts

| Signal | Warning | Critical | First response |
| --- | --- | --- | --- |
| `control_operation_oldest_age_seconds` | 60 seconds | 300 seconds | Check worker health, lease expiry, and provider errors. |
| `control_operation_dead_letter_depth` | greater than 0 | greater than 5 | Inspect the operation and use the authenticated recovery workflow only after resolving the cause. |
| `control_reconciliation_oldest_age_seconds` | 60 seconds | 180 seconds | Check database contention, stale desired versions, and tunnel health. |
| `control_stale_node_depth` | greater than 0 for two control intervals | greater than 0 for five intervals | Confirm heartbeat loss, connector fencing, and route reassignment. |
| `billing_*_uncertain_depth` | greater than 0 | increasing for 15 minutes | Follow `billing-uncertain-operations.md`; verify provider state before retrying. |
| `provider_*_errors_total` | error ratio above 2% for 5 minutes | above 10% for 5 minutes | Check provider status, request latency, credentials, and rate limits. |
| `provider_*_latency_ms_max` | above configured request timeout's 75% | reaches configured timeout | Correlate with errors and operation uncertainty before changing retry policy. |
| `user_machine_bootstrap_failure_depth` | greater than 0 | increasing across two release intervals | Inspect the typed install stage, signed artifact compatibility, administrator denial, and rollback evidence. |
| `user_machine_heartbeat_oldest_age_seconds` | above the configured stale-heartbeat interval | above five intervals | Correlate worker boot scope, renewal, DNS, connector recovery, and host reachability before re-enrollment. |
| `user_machine_availability_drift_depth` | greater than 0 for two heartbeat intervals | increasing for five intervals | Compare desired/observed versions and inspect root host-service diagnostics. |
| `user_machine_privileged_service_error_depth` | greater than 0 | increasing after retry | Inspect logind/pmset errors and restore the captured power baseline before manual changes. |
| `user_machine_unsupported_host_scope_depth` | greater than 0 | any newly bootstrapped machine | Stop rollout; only system scope is a successful privileged bootstrap. |
| `paperboat_runtime_restart_total` | repeated restarts within 5 minutes | sustained restart loop | Inspect boot ID/generation, service exit status, and bounded logs. |
| `paperboat_runtime_renewal_failures_total` | increasing for two credential refresh attempts | credential expiry without recovery | Check proof freshness, active helper key, revocation, and server signing-key health. |
| `paperboat_runtime_connector_recovery_seconds` | above 60 seconds | sustained unavailability after network restoration | Check interface/DNS wakeups, admission, QUIC, and TCP/TLS fallback. |
| `paperboat_runtime_update_rollbacks_total` | greater than 0 | repeated for the same release | Stop rollout and inspect paired signatures, journal stage, worker health, and preserved previous versions. |

Connector incidents are scoped by `(machine_id, connector_id, connector_generation)`.
`runtime` is the terminal-host connector; each preview uses its preview key as connector
ID. Diagnose and fence the affected instance without revoking healthy sibling previews.
Node loss advances every connector instance assigned to that node and invalidates late
route observations from their prior generations.

For preview restart loops, compare the local absolute expiry with the server preview
record. Expired, removed, or mismatched records must not be re-registered. A matching
active record may resume with only its remaining lifetime. Do not delete connector rows
or manufacture a new preview key as incident recovery.

Every alert record must include the deployment, request or operation ID when
available, first observation time, current depth/age, and recovery decision.
Do not include credentials, signed URLs, request bodies, terminal data, or
provider secrets.

Close an incident only after the metric returns to baseline and the durable
operation, reconciliation, route, or billing record agrees with provider and
tunnel observations.

For preview, tunnel, DNS, certificate, connector, private-access, and update
incidents, continue with
[`preview-tunnel-control-plane.md`](preview-tunnel-control-plane.md).

// Package retention bounds the tables that grow with traffic (PLAN 3b, storage): nothing deleted from them, so
// every request, credential and report stayed forever (374 MB of test traffic on 2026-10-23; terabytes at 100k
// users). Each policy deletes rows past its window (config.Retention, one place) in batches of Batch rows, each its
// own short statement (FOR UPDATE SKIP LOCKED, so it never waits on a live row), and a pass loops until every policy
// is caught up or Budget is spent, so the pruning keeps up with any write rate.
package retention

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

type policy struct {
	name string
	// sql deletes at most $1 rows; $2, when the policy has one, is its window in seconds or its per-owner cap.
	sql   string
	param func(config.Retention) []any
}

func seconds(d func(config.Retention) time.Duration) func(config.Retention) []any {
	return func(c config.Retention) []any { return []any{d(c).Seconds()} }
}

func count(n func(config.Retention) int) func(config.Retention) []any {
	return func(c config.Retention) []any { return []any{n(c)} }
}

func none(config.Retention) []any { return nil }

// batchOf deletes the rows of table whose ctid the candidates subquery selects (bounded by $1, locked rows skipped).
func batchOf(table, candidates string) string {
	return `DELETE FROM paperboat.` + table + ` WHERE ctid = ANY(ARRAY(SELECT ctid FROM paperboat.` + table + ` WHERE ` + candidates +
		` LIMIT $1 FOR UPDATE SKIP LOCKED))`
}

// policies run in this order: a parent whose foreign keys do not cascade comes after the children that hold it.
var policies = []policy{
	// A CLI recording replay's peer network admission, after its ticket expired.
	{"recording_native_admissions", batchOf("recording_native_admissions", `expires_at < now()-make_interval(secs=>$2)`),
		seconds(func(c config.Retention) time.Duration { return c.ExpiredCredentials })},
	// A recording's index row (the bytes live, and are deleted, on its machine).
	{"session_recordings", batchOf("session_recordings", `started_at < now()-make_interval(secs=>$2)`),
		seconds(func(c config.Retention) time.Duration { return c.Recordings })},
	{"control_config_credentials", batchOf("control_config_credentials", `expires_at < now()-make_interval(secs=>$2)`),
		seconds(func(c config.Retention) time.Duration { return c.ExpiredCredentials })},
	// A revoked grant still waiting for its provider revocation keeps the ciphertext the revocation needs.
	{"control_config_repository_access_operations", batchOf("control_config_repository_access_operations",
		`expires_at < now()-make_interval(secs=>$2) AND NOT (revoked_at IS NOT NULL AND provider_revoked_at IS NULL AND access_ciphertext IS NOT NULL)`),
		seconds(func(c config.Retention) time.Duration { return c.ExpiredCredentials })},
	{"client_refresh_redemptions", batchOf("client_refresh_redemptions", `expires_at < now()-make_interval(secs=>$2)`),
		seconds(func(c config.Retention) time.Duration { return c.ExpiredCredentials })},
	{"helper_identity_renewals", batchOf("helper_identity_renewals", `expires_at < now()-make_interval(secs=>$2)`),
		seconds(func(c config.Retention) time.Duration { return c.ExpiredCredentials })},
	{"cli_access_tokens", batchOf("cli_access_tokens", `expires_at < now()-make_interval(secs=>$2)`),
		seconds(func(c config.Retention) time.Duration { return c.ExpiredCredentials })},
	// A rotated refresh token stays until its own expiry: a replay of it is how a stolen token is detected.
	{"cli_refresh_tokens", batchOf("cli_refresh_tokens", `expires_at < now()-make_interval(secs=>$2)`),
		seconds(func(c config.Retention) time.Duration { return c.ExpiredCredentials })},
	{"browser_machine_credentials", batchOf("browser_machine_credentials", `expires_at < now()-make_interval(secs=>$2)`),
		seconds(func(c config.Retention) time.Duration { return c.ExpiredCredentials })},
	// Its native admissions cascade with it.
	{"inspector_credentials", batchOf("inspector_credentials", `expires_at < now()-make_interval(secs=>$2)`),
		seconds(func(c config.Retention) time.Duration { return c.ExpiredCredentials })},
	// A consumed edge ticket is kept while its attachment is attached (and a window after), so the edge can resume
	// that attachment's control channel; then it goes like any other.
	{"browser_terminal_tickets", batchOf("browser_terminal_tickets", `expires_at < now()-make_interval(secs=>$2)
 AND NOT EXISTS (SELECT 1 FROM paperboat.native_usage_authorities usage WHERE usage.access_session_id=browser_terminal_tickets.edge_attachment_id
  AND usage.resource_kind='terminal' AND usage.attached_until > now()-make_interval(secs=>$2))`),
		seconds(func(c config.Retention) time.Duration { return c.ExpiredCredentials })},
	// Only after the certificate expired: a holder that lost access stays on its machines' revoked lists
	// until then (managedssh.TrustedUserCAs revoked key IDs), so deleting it earlier would let its certificate in again.
	{"managed_ssh_certificate_holders", batchOf("managed_ssh_certificate_holders", `valid_before < now()-make_interval(secs=>$2)`),
		seconds(func(c config.Retention) time.Duration { return c.ExpiredCredentials })},
	{"managed_ssh_operations", batchOf("managed_ssh_operations", `created_at < now()-make_interval(secs=>$2)`),
		seconds(func(c config.Retention) time.Duration { return c.IdempotencyRecords })},
	// Replay records of retried requests: a retry past the window is a new request. The route record is written and
	// completed in one transaction, so an unfinished one is never a live row; vault and capability records are
	// complete when stored.
	{"control_route_operations", batchOf("control_route_operations", `result_revision IS NOT NULL AND created_at < now()-make_interval(secs=>$2)`),
		seconds(func(c config.Retention) time.Duration { return c.IdempotencyRecords })},
	{"environment_vault_operations", batchOf("environment_vault_operations", `created_at < now()-make_interval(secs=>$2)`),
		seconds(func(c config.Retention) time.Duration { return c.IdempotencyRecords })},
	{"user_machine_capability_operations", batchOf("user_machine_capability_operations", `created_at < now()-make_interval(secs=>$2)`),
		seconds(func(c config.Retention) time.Duration { return c.IdempotencyRecords })},
	// A finished attempt; started and uncertain ones are what the reconciliation depth metric counts.
	{"control_reconciliation_attempts", batchOf("control_reconciliation_attempts",
		`state IN ('succeeded','failed','cancelled') AND finished_at < now()-make_interval(secs=>$2)`),
		seconds(func(c config.Retention) time.Duration { return c.FinishedOperations })},
	// A publication whose records were withdrawn and verified so (no desired address, no provider record): the owner
	// is gone; a hostname that comes back gets a fresh row.
	{"edge_dns_publications", batchOf("edge_dns_publications",
		`state='verified' AND desired_addresses='[]'::jsonb AND provider_records='[]'::jsonb AND updated_at < now()-make_interval(secs=>$2)`),
		seconds(func(c config.Retention) time.Duration { return c.EndedPublications })},
	// An ended preview; its create operation record, endpoint, domains, logs and lazy activations cascade, so it runs
	// before operations.
	{"preview_leases", batchOf("preview_leases", `terminal_state <> 'active' AND stopped_at < now()-make_interval(secs=>$2)`),
		seconds(func(c config.Retention) time.Duration { return c.EndedPublications })},
	// A deleted tunnel (routes, domains, generations and logs cascade), a deleted route of a live tunnel, and a deleted
	// domain once its hostname quarantine is over.
	{"tunnels", batchOf("tunnels", `desired_state = 'deleted' AND deleted_at < now()-make_interval(secs=>$2)`),
		seconds(func(c config.Retention) time.Duration { return c.EndedPublications })},
	{"tunnel_routes", batchOf("tunnel_routes", `desired_state = 'deleted' AND deleted_at < now()-make_interval(secs=>$2)`),
		seconds(func(c config.Retention) time.Duration { return c.EndedPublications })},
	// A live tunnel's old configuration generations: superseded or rejected, past their retained_until, not the newest
	// two (the next generation needs its predecessor). Nothing else references a generation since the connector stack
	// was deleted (migration 201).
	{"tunnel_config_generations", batchOf("tunnel_config_generations", `activation_state IN ('superseded','rejected') AND retained_until < now()
 AND generation < (SELECT t.generation-1 FROM paperboat.tunnels t WHERE t.id=tunnel_config_generations.tunnel_id)`), none},
	{"tunnel_domains", batchOf("tunnel_domains",
		`deleted_at < now()-make_interval(secs=>$2) AND (quarantine_until IS NULL OR quarantine_until < now())`),
		seconds(func(c config.Retention) time.Duration { return c.EndedPublications })},
	{"preview_domains", batchOf("preview_domains",
		`deleted_at < now()-make_interval(secs=>$2) AND (quarantine_until IS NULL OR quarantine_until < now())`),
		seconds(func(c config.Retention) time.Duration { return c.EndedPublications })},
	// A grant past its expiry is dead (a consumed one replays only until then). One a pairing still names stays: the
	// pairing's setup check needs the link, so the pairing policy below goes first and the grant on a later pass.
	{"control_helper_enrollments", batchOf("control_helper_enrollments", `expires_at < now()-make_interval(secs=>$2)
 AND NOT EXISTS(SELECT 1 FROM paperboat.user_machine_pairings p WHERE p.authenticated_setup_helper_enrollment_id=control_helper_enrollments.id)`),
		seconds(func(c config.Retention) time.Duration { return c.ExpiredCredentials })},
	// A pairing nobody can retry any more; its reset receipts cascade. Failed and installed pairings follow below.
	{"user_machine_pairings", batchOf("user_machine_pairings",
		`installation_state IN ('cancelled','expired','denied','revoked','deleted') AND expires_at < now()-make_interval(secs=>$2)
 AND updated_at < now()-make_interval(secs=>$2)
 AND (installation_config_consumed_at IS NULL OR installation_config_consumed_at < now()-make_interval(secs=>$2))`),
		seconds(func(c config.Retention) time.Duration { return c.FinishedEnrollments })},
	// A failed pairing nobody retried for a month (the console's retry reads it until then).
	{"user_machine_pairings_failed", batchOf("user_machine_pairings",
		`installation_state = 'failed_retryable' AND updated_at < now()-make_interval(secs=>$2) AND expires_at < now()-make_interval(secs=>$2)
 AND (installation_config_consumed_at IS NULL OR installation_config_consumed_at < now()-make_interval(secs=>$2))`),
		seconds(func(c config.Retention) time.Duration { return c.AbandonedEnrollments })},
	// A finished install whose machine is gone or revoked, or that a newer pairing of the same machine replaced (the
	// live machine's own row is its newest; "disconnected" and "auth_consumed" are allowed states nothing writes yet).
	{"user_machine_pairings_replaced", batchOf("user_machine_pairings",
		`installation_state IN ('ready','disconnected','auth_consumed') AND expires_at < now()-make_interval(secs=>$2)
 AND updated_at < now()-make_interval(secs=>$2)
 AND (installation_config_consumed_at IS NULL OR installation_config_consumed_at < now()-make_interval(secs=>$2))
 AND (NOT EXISTS(SELECT 1 FROM paperboat.live_user_machines m WHERE m.id=user_machine_pairings.user_machine_id)
  OR EXISTS(SELECT 1 FROM paperboat.user_machine_pairings n WHERE n.user_machine_id=user_machine_pairings.user_machine_id
   AND (n.created_at,n.id) > (user_machine_pairings.created_at,user_machine_pairings.id)
   AND n.installation_state IN ('approved','material_issued','installing','connecting','ready')))`),
		seconds(func(c config.Retention) time.Duration { return c.FinishedEnrollments })},
	// A receipt replays a reset only while its enrollment still waits for bootstrap; the pairing policy above takes the
	// receipts of pairings it deletes, this one those of a pairing that lives on (a paired machine).
	{"user_machine_pairing_resets", batchOf("user_machine_pairing_resets", `completed_at < now()-make_interval(secs=>$2)
 AND NOT EXISTS(SELECT 1 FROM paperboat.user_machine_pairings p WHERE p.id=user_machine_pairing_resets.enrollment_id
  AND p.installation_state='awaiting_bootstrap')`),
		seconds(func(c config.Retention) time.Duration { return c.FinishedEnrollments })},
	{"account_deletion_jobs", batchOf("account_deletion_jobs",
		`state IN ('done','failed') AND coalesce(completed_at,created_at) < now()-make_interval(secs=>$2)`),
		seconds(func(c config.Retention) time.Duration { return c.AccountDeletionJobs })},
	// A terminal session its owner or the closed-session expiry deleted (the row stays as a tombstone until the host's
	// delete_history operation finished); its operations, grants and tickets cascade.
	{"user_machine_terminal_sessions", batchOf("user_machine_terminal_sessions", `deleted_at < now()-make_interval(secs=>$2)
 AND NOT EXISTS(SELECT 1 FROM paperboat.user_machine_terminal_session_operations o
  WHERE o.terminal_session_id=user_machine_terminal_sessions.id AND o.state='pending')`),
		seconds(func(c config.Retention) time.Duration { return c.FinishedOperations })},
	// A detached route replaced by a newer route of its environment and kind (reactivation takes the newest); the
	// carrier attachments of the route cascade, and a removed environment takes all its routes with it.
	{"control_routes", batchOf("control_routes", `desired_state = 'detached' AND updated_at < now()-make_interval(secs=>$2)
 AND EXISTS(SELECT 1 FROM paperboat.control_routes n WHERE n.environment_id=control_routes.environment_id AND n.kind=control_routes.kind
  AND (n.created_at, n.id) > (control_routes.created_at, control_routes.id))`),
		seconds(func(c config.Retention) time.Duration { return c.FinishedOperations })},
	// A finished operation still referenced stays until its referrer goes.
	{"operations", batchOf("operations", `completed_at < now()-make_interval(secs=>$2)
 AND NOT EXISTS(SELECT 1 FROM paperboat.preview_lease_create_operations x WHERE x.account_id=operations.account_id AND x.operation_id=operations.id)`),
		seconds(func(c config.Retention) time.Duration { return c.FinishedOperations })},
	// Its recoveries cascade; reconciliation attempts keep their row without it.
	{"control_operations", batchOf("control_operations", `completed_at < now()-make_interval(secs=>$2)`),
		seconds(func(c config.Retention) time.Duration { return c.FinishedOperations })},
	// An invitation's latest_mail_id is cleared with it.
	{"transactional_mail_outbox", batchOf("transactional_mail_outbox", `state <> 'queued' AND created_at < now()-make_interval(secs=>$2)`),
		seconds(func(c config.Retention) time.Duration { return c.FinishedOperations })},
	// A certificate no longer served (its edge distributions cascade).
	{"tunnel_certificate_records", batchOf("tunnel_certificate_records",
		`state IN ('superseded','revoked','expired','failed') AND updated_at < now()-make_interval(secs=>$2)`),
		seconds(func(c config.Retention) time.Duration { return c.FinishedOperations })},
	{"managed_ssh_client_keys", batchOf("managed_ssh_client_keys", `state = 'revoked' AND revoked_at < now()-make_interval(secs=>$2)`),
		seconds(func(c config.Retention) time.Duration { return c.EndedSessions })},
	// A peer certificate long expired: first its revocation and issuance records (their keys restrict it), then the
	// certificate itself unless an identity still names it.
	{"peer_endpoint_certificate_revocations", `DELETE FROM paperboat.peer_endpoint_certificate_revocations WHERE certificate_fingerprint = ANY(ARRAY(
 SELECT c.fingerprint FROM paperboat.peer_endpoint_certificates c WHERE c.expires_at < now()-make_interval(secs=>$2)
  AND EXISTS(SELECT 1 FROM paperboat.peer_endpoint_certificate_revocations r WHERE r.certificate_fingerprint=c.fingerprint) LIMIT $1))`,
		seconds(func(c config.Retention) time.Duration { return c.EndedSessions })},
	{"peer_endpoint_certificate_operations", `DELETE FROM paperboat.peer_endpoint_certificate_operations WHERE certificate_fingerprint = ANY(ARRAY(
 SELECT c.fingerprint FROM paperboat.peer_endpoint_certificates c WHERE c.expires_at < now()-make_interval(secs=>$2)
  AND EXISTS(SELECT 1 FROM paperboat.peer_endpoint_certificate_operations o WHERE o.certificate_fingerprint=c.fingerprint) LIMIT $1))`,
		seconds(func(c config.Retention) time.Duration { return c.EndedSessions })},
	{"peer_endpoint_certificates", batchOf("peer_endpoint_certificates", `expires_at < now()-make_interval(secs=>$2)
 AND NOT EXISTS(SELECT 1 FROM paperboat.peer_network_identities n WHERE n.quic_certificate_fingerprint=peer_endpoint_certificates.fingerprint)
 AND NOT EXISTS(SELECT 1 FROM paperboat.peer_endpoint_certificate_revocations r WHERE r.certificate_fingerprint=peer_endpoint_certificates.fingerprint)
 AND NOT EXISTS(SELECT 1 FROM paperboat.peer_endpoint_certificate_operations o WHERE o.certificate_fingerprint=peer_endpoint_certificates.fingerprint)`),
		seconds(func(c config.Retention) time.Duration { return c.EndedSessions })},
	// A mesh log entry replaced by a newer entry of its endpoint, and the latest entry of an endpoint removed long
	// ago (an absent endpoint reads the same with or without it).
	{"peer_mesh_log_superseded", batchOf("peer_mesh_log", `recorded_at < now()-make_interval(secs=>$2)
 AND EXISTS(SELECT 1 FROM paperboat.peer_mesh_log n WHERE n.account_id=peer_mesh_log.account_id AND n.endpoint_id=peer_mesh_log.endpoint_id
  AND n.mesh_seq>peer_mesh_log.mesh_seq AND n.recorded_at < now()-make_interval(secs=>$2))`),
		seconds(func(c config.Retention) time.Duration { return c.SupersededMeshEntries })},
	{"peer_mesh_log_removed", batchOf("peer_mesh_log", `op='remove' AND recorded_at < now()-make_interval(secs=>$2)
 AND NOT EXISTS(SELECT 1 FROM paperboat.peer_mesh_log n WHERE n.account_id=peer_mesh_log.account_id AND n.endpoint_id=peer_mesh_log.endpoint_id
  AND n.mesh_seq>peer_mesh_log.mesh_seq)`),
		seconds(func(c config.Retention) time.Duration { return c.EndedSessions })},
	{"user_machine_access_sessions", batchOf("user_machine_access_sessions", `expires_at < now()-make_interval(secs=>$2)`),
		seconds(func(c config.Retention) time.Duration { return c.EndedSessions })},
	{"sessions", batchOf("sessions", `expires_at < now()-make_interval(secs=>$2)`),
		seconds(func(c config.Retention) time.Duration { return c.EndedSessions })},
	{"device_grants", batchOf("device_grants", `expires_at < now()-make_interval(secs=>$2)`),
		seconds(func(c config.Retention) time.Duration { return c.EndedSessions })},
	// After the device grants that name it (their foreign key does not cascade).
	{"cli_client_sessions", batchOf("cli_client_sessions", `revoked_at < now()-make_interval(secs=>$2)
 AND NOT EXISTS(SELECT 1 FROM paperboat.device_grants g WHERE g.cli_client_session_id=cli_client_sessions.id)
 AND NOT EXISTS(SELECT 1 FROM paperboat.device_token_redemptions r WHERE r.cli_client_session_id=cli_client_sessions.id)`),
		seconds(func(c config.Retention) time.Duration { return c.EndedSessions })},
	{"control_usage_receipts", batchOf("control_usage_receipts", `acknowledged_at < now()-make_interval(secs=>$2)`),
		seconds(func(c config.Retention) time.Duration { return c.AcknowledgedUsageReceipts })},
	{"tunnel_log_entries", batchOf("tunnel_log_entries", `occurred_at < now()-make_interval(secs=>$2)`),
		seconds(func(c config.Retention) time.Duration { return c.TunnelLogs })},
	{"team_machine_requests", batchOf("team_machine_requests", `decided_at < now()-make_interval(secs=>$2)`),
		seconds(func(c config.Retention) time.Duration { return c.DecidedMachineRequests })},
	// The receipt outbox row goes with its request (ON DELETE CASCADE). A request still pending or approved is kept
	// until its own expiry has passed by the window; a request in transfer (consumed) until it stopped changing.
	{"team_inbox_requests", batchOf("team_inbox_requests",
		`(stored_prefix IS NULL OR stored_deleted_at IS NOT NULL) AND ((status IN ('declined','expired','revoked','consumed','completed') AND updated_at < now()-make_interval(secs=>$2))
 OR (status IN ('pending','approved') AND expires_at < now()-make_interval(secs=>$2)))`),
		seconds(func(c config.Retention) time.Duration { return c.InboxRequests })},
	{"stripe_events", batchOf("stripe_events", `processed_at < now()-make_interval(secs=>$2)`),
		seconds(func(c config.Retention) time.Duration { return c.StripeEvents })},
	// A node's day totals and the latest-sample row of a node that stopped reporting.
	{"node_capacity_daily", batchOf("node_capacity_daily", `day < (now()-make_interval(secs=>$2))::date`),
		seconds(func(c config.Retention) time.Duration { return c.NodeCapacityDaily })},
	{"node_capacity", batchOf("node_capacity", `reported_at < now()-make_interval(secs=>$2)`),
		seconds(func(c config.Retention) time.Duration { return c.NodeCapacityDaily })},
	// An announcement ended or retracted, or an open-ended notice that started that long ago (the bell shows the newest
	// few); its receipts cascade. An open-ended banner stays until it is retracted.
	{"announcements", batchOf("announcements", `coalesce(retracted_at, ends_at, CASE WHEN kind='notice' THEN starts_at END) < now()-make_interval(secs=>$2)`),
		seconds(func(c config.Retention) time.Duration { return c.EndedAnnouncements })},
	{"support_reports_expired", batchOf("support_report_occurrences", `expires_at < now()`), none},
	// A group's expires_at is its newest occurrence's, so it goes after the occurrences it explains.
	{"support_report_groups_expired", batchOf("support_report_groups", `expires_at < now()`), none},
	// Past an account's newest SupportReportsPerAccount occurrences.
	{"support_reports_per_account", `DELETE FROM paperboat.support_report_occurrences WHERE id IN (SELECT id FROM (
 SELECT id,row_number() OVER (PARTITION BY owner_id ORDER BY created_at DESC,id DESC) n FROM paperboat.support_report_occurrences) r WHERE n > $2 LIMIT $1)`,
		count(func(c config.Retention) int { return c.SupportReportsPerAccount })},
}

// Result is the rows one pass deleted, per policy.
type Result map[string]int64

// Run makes one pass: each policy deletes batches until it is caught up or the pass's budget is spent.
func Run(ctx context.Context, store *db.DB, cfg config.Retention) (Result, error) {
	deadline := time.Now().Add(cfg.Budget)
	out := Result{}
	var failures error
	for _, p := range policies {
		for time.Now().Before(deadline) {
			tag, err := store.Pool().Exec(ctx, p.sql, append([]any{cfg.Batch}, p.param(cfg)...)...)
			if err != nil {
				failures = errors.Join(failures, fmt.Errorf("retention %s: %w", p.name, err))
				break
			}
			out[p.name] += tag.RowsAffected()
			if tag.RowsAffected() < int64(cfg.Batch) {
				break
			}
		}
	}
	return out, failures
}

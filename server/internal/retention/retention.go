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
	{"managed_ssh_operations", batchOf("managed_ssh_operations", `created_at < now()-make_interval(secs=>$2)`),
		seconds(func(c config.Retention) time.Duration { return c.IdempotencyRecords })},
	// A finished operation still referenced (a live preview's carrier attachment cascades from it; tunnel connector
	// rows restrict it) stays until its referrer goes.
	{"operations", batchOf("operations", `completed_at < now()-make_interval(secs=>$2)
 AND NOT EXISTS(SELECT 1 FROM paperboat.preview_lease_carrier_attachments x WHERE x.account_id=operations.account_id AND x.operation_id=operations.id)
 AND NOT EXISTS(SELECT 1 FROM paperboat.preview_lease_create_operations x WHERE x.account_id=operations.account_id AND x.operation_id=operations.id)
 AND NOT EXISTS(SELECT 1 FROM paperboat.tunnel_connector_activations x WHERE x.operation_id=operations.id)
 AND NOT EXISTS(SELECT 1 FROM paperboat.tunnel_connector_credential_generations x WHERE x.source_operation_id=operations.id)
 AND NOT EXISTS(SELECT 1 FROM paperboat.tunnel_connector_enrollments x WHERE x.operation_id=operations.id)
 AND NOT EXISTS(SELECT 1 FROM paperboat.tunnel_connector_rotation_targets x WHERE x.operation_id=operations.id)`),
		seconds(func(c config.Retention) time.Duration { return c.FinishedOperations })},
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
	{"tunnel_connector_sessions", batchOf("tunnel_connector_sessions", `state IN ('disconnected','expired') AND retained_until < now()`),
		none},
	{"team_machine_requests", batchOf("team_machine_requests", `decided_at < now()-make_interval(secs=>$2)`),
		seconds(func(c config.Retention) time.Duration { return c.DecidedMachineRequests })},
	{"stripe_events", batchOf("stripe_events", `processed_at < now()-make_interval(secs=>$2)`),
		seconds(func(c config.Retention) time.Duration { return c.StripeEvents })},
	{"support_reports_expired", batchOf("support_reports", `expires_at < now()`), none},
	// Past an account's newest SupportReportsPerAccount reports.
	{"support_reports_per_account", `DELETE FROM paperboat.support_reports WHERE id IN (SELECT id FROM (
 SELECT id,row_number() OVER (PARTITION BY owner_id ORDER BY created_at DESC,id DESC) n FROM paperboat.support_reports) r WHERE n > $2 LIMIT $1)`,
		count(func(c config.Retention) int { return c.SupportReportsPerAccount })},
	{"control_config_sync_status_history", `DELETE FROM paperboat.control_config_sync_status_history WHERE (environment_id,sync_revision) IN (
 SELECT environment_id,sync_revision FROM (SELECT environment_id,sync_revision,
  row_number() OVER (PARTITION BY environment_id ORDER BY sync_revision DESC) n FROM paperboat.control_config_sync_status_history) h WHERE n > $2 LIMIT $1)`,
		count(func(c config.Retention) int { return c.ConfigSyncHistoryPerEnvironment })},
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

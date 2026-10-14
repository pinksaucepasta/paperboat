package controlplane

import (
	"context"
	"github.com/pinksaucepasta/paperboat-server/internal/db/dbsqlc"
	"github.com/pinksaucepasta/paperboat-server/internal/workspace"
)

const configInventoryStatusQuery = `-- name: ListOwnedControlConfigSyncStatus :many
SELECT
  environment.id AS environment_id,
  machine.id AS machine_id,
  environment.workspace_id,
  COALESCE(machine.alias, environment.id)::text AS alias,
  environment.desired_state AS environment_state,
  assignment.id AS assignment_id,
  assignment.repository_id,
	assignment.push_repository_id,
	assignment.automatic_updates,
	assignment.approved_pull_revision,
	assignment.adopted_team_id,
	assignment.adopted_default_version,
	assignment.mode,
  assignment.consent_state,
  assignment.warning_revision,
  assignment.version AS assignment_version,
  repository.display_name AS repository_name,
  repository.state AS repository_state,
  COALESCE(machine.installation_generation, 0)::bigint AS installation_generation,
  status.state AS sync_state,
  status.assignment_id AS status_assignment_id,
  status.repository_id AS status_repository_id,
  status.machine_id AS status_machine_id,
  status.installation_generation AS status_installation_generation,
  status.policy_revision,
  status.sync_revision,
  status.remote_revision,
  status.manifest_health,
  status.manifest_revision,
  status.managed_path_count,
  status.pending_clean_path_count,
  status.last_applied_revision,
  status.last_published_revision,
  COALESCE(status.skipped, '[]'::jsonb)::jsonb AS skipped,
  COALESCE(status.conflicts, '[]'::jsonb)::jsonb AS conflicts,
  COALESCE(status.review, '[]'::jsonb)::jsonb AS review,
  status.error_code,
  COALESCE(status.recovery_actions, '[]'::jsonb)::jsonb AS recovery_actions,
  status.last_attempt_at,
  status.last_successful_at,
  status.machine_updated_at,
  status.observed_at
FROM control_environments environment
LEFT JOIN control_config_assignments assignment
  ON assignment.environment_id = environment.id
LEFT JOIN control_config_repositories repository
  ON repository.id = assignment.repository_id
LEFT JOIN control_config_sync_statuses status
  ON status.environment_id = environment.id
LEFT JOIN user_machines machine
  ON machine.environment_id = environment.id AND machine.deleted_at IS NULL
WHERE environment.owner_user_id = $1
  AND environment.desired_state <> 'revoked'`

func (s *ConfigStatusService) inventoryStatusRows(ctx context.Context, account string, opts ConfigStatusInventoryOptions) ([]dbsqlc.ListOwnedControlConfigSyncStatusRow, int, string, error) {
	if opts.Limit < 1 || opts.Limit > 200 || opts.Offset < 0 {
		return nil, 0, "", ErrConfigStatusInvalid
	}
	scoped := false
	var team any
	if scope, ok := workspace.FromContext(ctx); ok {
		if err := workspace.Validate(ctx, s.store, account, scope); err != nil {
			return nil, 0, "", err
		}
		scoped = true
		if scope.TeamID != "" {
			team = scope.TeamID
		}
	}
	query := `WITH raw AS (` + configInventoryStatusQuery + `), projected AS(SELECT raw.*,CASE
 WHEN $7::boolean THEN 'disabled'
 WHEN assignment_id IS NULL OR repository_id IS NULL THEN 'disabled'
 WHEN repository_state<>'active' OR environment_state<>'active' THEN 'revoked'
 WHEN consent_state<>'accepted' THEN 'consent_required'
 WHEN machine_id IS NULL OR machine_id='' OR sync_state IS NULL OR coalesce(policy_revision,'')='' OR status_assignment_id IS DISTINCT FROM assignment_id OR status_repository_id IS DISTINCT FROM repository_id OR status_machine_id IS DISTINCT FROM machine_id OR status_installation_generation IS DISTINCT FROM installation_generation THEN 'pending'
 WHEN $8::bigint>0 AND (observed_at IS NULL OR observed_at<$9::timestamptz) THEN 'offline'
 ELSE sync_state END inventory_state FROM raw)
 SELECT * FROM projected WHERE (NOT $2::boolean OR ($3::text IS NULL AND adopted_team_id IS NULL) OR adopted_team_id=$3)
 AND ($4='' OR position(lower($4) in lower(alias||' '||environment_id||' '||coalesce(repository_name,'')))>0)
 AND ($5='' OR inventory_state=$5)
 AND ($6='' OR $6='mine' OR $6=$1) AND ($10='' OR environment_id=$10)`
	args := []any{account, scoped, team, opts.Q, opts.State, opts.Owner, s.policy.Mode == "disabled", int64(s.policy.StaleHeartbeatAfter), s.clock().Add(-s.policy.StaleHeartbeatAfter), opts.EnvironmentID}
	summaryRows, err := s.store.SQL().QueryContext(ctx, "SELECT inventory_state,count(*) FROM ("+query+") states GROUP BY inventory_state", args...)
	if err != nil {
		return nil, 0, "", err
	}
	total := 0
	summary := "disabled"
	for summaryRows.Next() {
		var state string
		var count int
		if err = summaryRows.Scan(&state, &count); err != nil {
			summaryRows.Close()
			return nil, 0, "", err
		}
		total += count
		summary = higherConfigAccountState(summary, state)
	}
	err = summaryRows.Err()
	summaryRows.Close()
	if err != nil {
		return nil, 0, "", err
	}
	rows, err := s.store.SQL().QueryContext(ctx, query+" ORDER BY lower(alias),environment_id LIMIT $11 OFFSET $12", append(args, opts.Limit, opts.Offset)...)
	if err != nil {
		return nil, 0, "", err
	}
	defer rows.Close()
	out := []dbsqlc.ListOwnedControlConfigSyncStatusRow{}
	for rows.Next() {
		var item dbsqlc.ListOwnedControlConfigSyncStatusRow
		var state string
		if err := rows.Scan(
			&item.EnvironmentID,
			&item.MachineID,
			&item.WorkspaceID,
			&item.Alias,
			&item.EnvironmentState,
			&item.AssignmentID,
			&item.RepositoryID,
			&item.PushRepositoryID,
			&item.AutomaticUpdates,
			&item.ApprovedPullRevision,
			&item.AdoptedTeamID,
			&item.AdoptedDefaultVersion,
			&item.Mode,
			&item.ConsentState,
			&item.WarningRevision,
			&item.AssignmentVersion,
			&item.RepositoryName,
			&item.RepositoryState,
			&item.InstallationGeneration,
			&item.SyncState,
			&item.StatusAssignmentID,
			&item.StatusRepositoryID,
			&item.StatusMachineID,
			&item.StatusInstallationGeneration,
			&item.PolicyRevision,
			&item.SyncRevision,
			&item.RemoteRevision,
			&item.ManifestHealth,
			&item.ManifestRevision,
			&item.ManagedPathCount,
			&item.PendingCleanPathCount,
			&item.LastAppliedRevision,
			&item.LastPublishedRevision,
			&item.Skipped,
			&item.Conflicts,
			&item.Review,
			&item.ErrorCode,
			&item.RecoveryActions,
			&item.LastAttemptAt,
			&item.LastSuccessfulAt,
			&item.MachineUpdatedAt,
			&item.ObservedAt,
			&state); err != nil {
			return nil, 0, "", err
		}
		out = append(out, item)
	}
	return out, total, summary, rows.Err()
}

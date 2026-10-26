package controlplane

import (
	"context"
	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
	"github.com/pinksaucepasta/paperboat/server/internal/workspace"
	"slices"
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
 AND status.assignment_id = assignment.id AND assignment.revoked_at IS NULL
LEFT JOIN user_machines machine
  ON machine.environment_id = environment.id AND machine.deleted_at IS NULL
WHERE environment.owner_user_id = $1
  AND environment.desired_state <> 'revoked'`

// ConfigStatusStates are the states one machine's config sync can be in.
var ConfigStatusStates = []string{"disabled", "revoked", "consent_required", "pending", "offline", "conflict", "error", "syncing", "healthy", "watching", "restoring", "warning", "sync_uncertain"}

// configStatusAttention are the states the Config sync page shows first ("needs attention").
var configStatusAttention = []string{"error", "conflict", "warning", "consent_required", "sync_uncertain"}

// ConfigStatusFilters are the state filter's values: one state, or a group. configured is every state but
// disabled (machines that sync config), attention is configStatusAttention, settled is configured without it.
var ConfigStatusFilters = append([]string{"configured", "attention", "settled"}, ConfigStatusStates...)

// configStatusFilter is the states a state filter selects; nil selects all.
func configStatusFilter(filter string) ([]string, error) {
	switch {
	case filter == "":
		return nil, nil
	case filter == "attention":
		return configStatusAttention, nil
	case filter == "configured" || filter == "settled":
		var out []string
		for _, state := range ConfigStatusStates {
			if state != "disabled" && (filter == "configured" || !slices.Contains(configStatusAttention, state)) {
				out = append(out, state)
			}
		}
		return out, nil
	case slices.Contains(ConfigStatusStates, filter):
		return []string{filter}, nil
	}
	return nil, ErrConfigStatusInvalid
}

// configStatusPage is one page of the status list: its rows (one past the limit when another follows) with each
// row's sort key (PostgreSQL's lower(alias), the cursor's key), the filtered total and the highest state.
type configStatusPage struct {
	rows    []dbsqlc.ListOwnedControlConfigSyncStatusRow
	keys    []string
	total   int
	counts  map[string]int
	summary string
}

func (s *ConfigStatusService) inventoryStatusRows(ctx context.Context, account string, opts ConfigStatusInventoryOptions) (configStatusPage, error) {
	if opts.Limit < 1 || opts.Limit > 200 || opts.Offset < 0 {
		return configStatusPage{}, ErrConfigStatusInvalid
	}
	scoped := false
	var team any
	if scope, ok := workspace.FromContext(ctx); ok {
		if err := workspace.Validate(ctx, s.store, account, scope); err != nil {
			return configStatusPage{}, err
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
 AND ($5::text[] IS NULL OR inventory_state=ANY($5::text[]))
 AND ($6='' OR $6='mine' OR $6=$1) AND ($10='' OR environment_id=$10)`
	states, err := configStatusFilter(opts.State)
	if err != nil {
		return configStatusPage{}, err
	}
	args := []any{account, scoped, team, opts.Q, states, opts.Owner, s.policy.Mode == "disabled", int64(s.policy.StaleHeartbeatAfter), s.clock().Add(-s.policy.StaleHeartbeatAfter), opts.EnvironmentID}
	summaryRows, err := s.store.SQL().QueryContext(ctx, "SELECT inventory_state,count(*) FROM ("+query+") states GROUP BY inventory_state", args...)
	if err != nil {
		return configStatusPage{}, err
	}
	total := 0
	summary := "disabled"
	counts := map[string]int{}
	for summaryRows.Next() {
		var state string
		var count int
		if err = summaryRows.Scan(&state, &count); err != nil {
			summaryRows.Close()
			return configStatusPage{}, err
		}
		total += count
		counts[state] = count
		summary = higherConfigAccountState(summary, state)
	}
	err = summaryRows.Err()
	summaryRows.Close()
	if err != nil {
		return configStatusPage{}, err
	}
	// The page reads one row past the limit so it knows whether another follows; the cursor (the last row's
	// position) narrows only the page, never the total.
	var afterKey, afterID any
	if opts.After != nil {
		afterKey, afterID = opts.After.Alias, opts.After.EnvironmentID
	}
	rows, err := s.store.SQL().QueryContext(ctx, "SELECT page.*,lower(page.alias) FROM ("+query+`) page
 WHERE $13::text IS NULL OR (lower(alias),environment_id)>($13::text,$14::text)
 ORDER BY lower(alias),environment_id LIMIT $11 OFFSET $12`, append(args, opts.Limit+1, opts.Offset, afterKey, afterID)...)
	if err != nil {
		return configStatusPage{}, err
	}
	defer rows.Close()
	out := []dbsqlc.ListOwnedControlConfigSyncStatusRow{}
	var keys []string
	for rows.Next() {
		var item dbsqlc.ListOwnedControlConfigSyncStatusRow
		var state, key string
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
			&state, &key); err != nil {
			return configStatusPage{}, err
		}
		out = append(out, item)
		keys = append(keys, key)
	}
	return configStatusPage{rows: out, keys: keys, total: total, counts: counts, summary: summary}, rows.Err()
}

package controlplane

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
	"github.com/pinksaucepasta/paperboat/server/internal/mint"
	"github.com/pinksaucepasta/paperboat/server/internal/workspace"
)

// AuthorizeCompare fences a private live read against the current reported conflict.
// It creates no resolution and stores no private payload.
func (s *ConfigConflictService) AuthorizeCompare(ctx context.Context, account, environment string, binding mint.ConfigCompareBinding) (string, error) {
	if account == "" || environment == "" || binding.AssignmentVersion < 1 || !validConfigConflictResolution(ConfigConflictResolution{Path: binding.ConfigPath, ConflictRevision: binding.ConflictRevision, ExpectedRemoteRevision: binding.ExpectedRemoteRevision, Scope: "path", Action: "keep_remote"}) {
		return "", ErrConfigConflictResolutionInvalid
	}
	if scope, selected := workspace.FromContext(ctx); selected {
		if err := workspace.Validate(ctx, s.store, account, scope); err != nil {
			return "", err
		}
		var adopted sql.NullString
		if err := s.store.SQL().QueryRowContext(ctx, "SELECT adopted_team_id FROM control_config_assignments WHERE environment_id=$1", environment).Scan(&adopted); err != nil {
			return "", ErrAssignmentForbidden
		}
		if scope.TeamID == "" && adopted.Valid || scope.TeamID != "" && (!adopted.Valid || adopted.String != scope.TeamID) {
			return "", ErrAssignmentForbidden
		}
	}
	current, err := s.store.Queries().GetOwnedControlConfigConflictContext(ctx, dbsqlc.GetOwnedControlConfigConflictContextParams{EnvironmentID: environment, OwnerUserID: sql.NullString{String: account, Valid: true}})
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrConfigConflictResolutionStale
	}
	if err != nil {
		return "", err
	}
	if current.AssignmentVersion != binding.AssignmentVersion || binding.AssignmentID != "" && current.AssignmentID != binding.AssignmentID || !current.RemoteRevision.Valid || current.RemoteRevision.String != binding.ExpectedRemoteRevision {
		return "", ErrConfigConflictResolutionStale
	}
	var currentBinding bool
	if err = s.store.SQL().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM control_config_assignments a JOIN user_machines m ON m.id=a.machine_id JOIN control_config_sync_statuses status ON status.environment_id=a.environment_id WHERE a.id=$1 AND a.consent_state='accepted' AND m.user_id=$2 AND m.revoked_at IS NULL AND m.deleted_at IS NULL AND status.machine_id=m.id AND status.assignment_id=a.id AND status.repository_id=a.repository_id AND status.installation_generation=m.installation_generation)`, current.AssignmentID, account).Scan(&currentBinding); err != nil {
		return "", err
	}
	if !currentBinding {
		return "", ErrConfigConflictResolutionStale
	}
	var conflicts []ConfigStatusPath
	if json.Unmarshal(current.Conflicts, &conflicts) != nil {
		return "", ErrConfigConflictResolutionStale
	}
	found := false
	for _, conflict := range conflicts {
		if conflict.Path == binding.ConfigPath && conflict.Revision == binding.ConflictRevision {
			found = true
			break
		}
	}
	if !found {
		return "", ErrConfigConflictResolutionStale
	}
	return current.AssignmentID, nil
}

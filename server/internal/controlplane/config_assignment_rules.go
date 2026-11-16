package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/pinksaucepasta/paperboat/shared/policy"
)

// ConfigMachineRuleView is the workspace policy config_sync rule that picks a machine's
// repository when the owner made no explicit choice. TeamID is empty for the personal policy.
// RepositoryID is the owner's connected repository with that URL and branch; empty means
// `pb config sync apply` connects it for the owner (State connect_on_apply).
type ConfigMachineRuleView struct {
	Matched      bool   `json:"matched"`
	TeamID       string `json:"team_id,omitempty"`
	Target       string `json:"target,omitempty"`
	Repository   string `json:"repository,omitempty"`
	Branch       string `json:"branch,omitempty"`
	RepositoryID string `json:"repository_id,omitempty"`
	State        string `json:"state"`
}

// MachineRule resolves the config_sync rule for one of the user's machines: the policies of the
// teams the machine is shared into (by team ID), then the owner's personal policy; the first
// rule whose target is machine:* or one of the machine's tags in that policy wins.
// Prior art: Tailscale policy nodeAttrs (attributes chosen by tag in the policy file).
func (s *ConfigAssignmentService) MachineRule(ctx context.Context, userID, machineID string) (ConfigMachineRuleView, error) {
	if _, err := s.ownedMachineEnvironment(ctx, userID, machineID); err != nil {
		return ConfigMachineRuleView{}, err
	}
	rows, err := s.store.SQL().QueryContext(ctx, `
SELECT coalesce(p.team_id,''), p.source,
       coalesce((SELECT string_agg(t.tag,' ') FROM workspace_policy_tags t WHERE t.policy_id=p.id AND t.machine_id=$2),'')
FROM workspace_policies p
LEFT JOIN teams team ON team.team_id=p.team_id AND team.deleted_at IS NULL
WHERE p.owner_account=$1
   OR (team.team_id IS NOT NULL AND EXISTS(SELECT 1 FROM team_resource_bindings b
       WHERE b.team_id=p.team_id AND b.resource_kind='machine' AND b.resource_id=$2 AND b.active))
ORDER BY p.team_id IS NULL, p.team_id`, userID, machineID)
	if err != nil {
		return ConfigMachineRuleView{}, err
	}
	defer rows.Close()
	view := ConfigMachineRuleView{State: "none"}
	for rows.Next() && !view.Matched {
		var teamID, source, tags string
		if err := rows.Scan(&teamID, &source, &tags); err != nil {
			return ConfigMachineRuleView{}, err
		}
		d, errs := policy.Parse([]byte(source))
		if len(errs) > 0 {
			continue // stored policies passed validation; an unparsable one picks nothing
		}
		if rule, target, ok := d.ConfigSyncFor(strings.Fields(tags)); ok {
			view = ConfigMachineRuleView{Matched: true, TeamID: teamID, Target: target, Repository: rule.Repository, Branch: rule.Branch, State: "connect_on_apply"}
		}
	}
	if err := rows.Err(); err != nil {
		return ConfigMachineRuleView{}, err
	}
	if !view.Matched {
		return view, nil
	}
	if view.Branch == "" {
		view.Branch = "main"
	}
	err = s.store.SQL().QueryRowContext(ctx, `SELECT id FROM control_config_repositories
WHERE owner_user_id=$1 AND state='active' AND (external_ref=$2 OR clone_url=$2) AND default_branch=$3
ORDER BY created_at, id LIMIT 1`, userID, view.Repository, view.Branch).Scan(&view.RepositoryID)
	if errors.Is(err, sql.ErrNoRows) {
		return view, nil
	}
	if err != nil {
		return ConfigMachineRuleView{}, err
	}
	view.State = "connected"
	return view, nil
}

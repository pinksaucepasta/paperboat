package usermachines

import (
	"context"
	"database/sql"
	"errors"
)

// CanManageInbox says whether the actor may read and delete a machine's Paperboat Inbox: its owner,
// or an owner or admin of the team that owns it, in the selected workspace. File names are private,
// so everyone else (and a machine that does not exist) is ErrNotFound. The Inbox itself is never
// stored here: the machine answers live.
func (s *Service) CanManageInbox(ctx context.Context, userID, userMachineID string) error {
	_, teamID := workspaceTeamArgument(ctx)
	var found bool
	err := s.db.SQL().QueryRowContext(ctx, `SELECT true FROM user_machines m
 WHERE m.id=$1 AND m.deleted_at IS NULL AND machine_management_allowed($2,m.id)
  AND ($3::text IS NULL OR m.owner_team_id=$3)`, userMachineID, userID, teamID).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// InboxMachine is a machine whose Inbox the actor may manage, for the workspace Inbox list.
type InboxMachine struct {
	ID, Alias, State string
	Online           bool
	Receives         bool // file_receive is configured
}

// InboxMachines lists up to limit machines whose Inbox the actor may manage (CanManageInbox's rule) in the
// selected workspace, in id order after after (or only the machine only); more reports whether others follow.
// Candidates (own machines and the actor's teams' machines) come by index before the exact check runs.
func (s *Service) InboxMachines(ctx context.Context, userID, after, only string, limit int) ([]InboxMachine, bool, error) {
	_, teamID := workspaceTeamArgument(ctx)
	rows, err := s.db.SQL().QueryContext(ctx, `SELECT m.id,m.alias,m.state,m.online,m.configured_capabilities @> ARRAY['file_receive']
 FROM user_machines m
 WHERE (m.user_id=$1 OR m.owner_team_id IN (SELECT team_id FROM team_members WHERE account_id=$1 AND active))
  AND m.deleted_at IS NULL AND m.revoked_at IS NULL AND ($2::text IS NULL OR m.owner_team_id=$2)
  AND ($3='' OR m.id=$3) AND ($4='' OR m.id>$4) AND machine_management_allowed($1,m.id)
 ORDER BY m.id LIMIT $5`, userID, teamID, only, after, limit+1)
	if err != nil {
		return nil, false, err
	}
	out := []InboxMachine{}
	for rows.Next() {
		var m InboxMachine
		if err := rows.Scan(&m.ID, &m.Alias, &m.State, &m.Online, &m.Receives); err != nil {
			return nil, false, errors.Join(err, rows.Close())
		}
		out = append(out, m)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, false, err
	}
	if len(out) > limit {
		return out[:limit], true, nil
	}
	return out, false, nil
}

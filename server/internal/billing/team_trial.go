package billing

import (
	"context"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// ConfigureTeamTrial sets the Team trial length; 0 creates teams without a trial.
func (s *WorkspaceService) ConfigureTeamTrial(days int) {
	s.teamTrialDays = max(days, 0)
}

// teamTrialDaysTx gives an account one Team trial: none once it has created a paid team.
func (s *WorkspaceService) teamTrialDaysTx(ctx context.Context, tx *db.Tx, actor string) (int, error) {
	if s.teamTrialDays == 0 {
		return 0, nil
	}
	var used bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM billing_team_orders WHERE actor_user_id=$1 AND state='paid')`, actor).Scan(&used)
	if err != nil || used {
		return 0, err
	}
	return s.teamTrialDays, nil
}

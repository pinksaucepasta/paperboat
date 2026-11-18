package usermachines

import (
	"context"
	"database/sql"
	"errors"
	"github.com/pinksaucepasta/paperboat/server/internal/planlimits"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/accountingauthority"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
)

// ConfigureRuntimeQuota makes console browser terminals accounted against the paying workspace.
// Nil leaves them unmetered. A terminal is never refused for bandwidth: past the workspace's fair-use
// amount the edge carries it at the throttled rate.
func (s *Service) ConfigureRuntimeQuota(quota accountingauthority.QuotaChecker) {
	s.runtimeQuota = quota
}

// admitRuntimeIngress records the payer of one edge browser-terminal session under its usage
// authority, so the edge's usage reports for it debit the right workspace. A team machine is paid by the team, counted
// for the acting member; a personal machine is paid by its owner, also when a guest connects.
func (s *Service) admitRuntimeIngress(ctx context.Context, route dbsqlc.ControlRoute, machine dbsqlc.UserMachine, actor, accessSessionID string, expires time.Time) error {
	if s.runtimeQuota == nil {
		return nil
	}
	binding := accountingauthority.Binding{
		ID: accessSessionID, Node: route.AppliedNodeID.String, Environment: machine.EnvironmentID, Route: route.ID,
		Revision: route.AppliedRevision, User: machine.UserID, IssuedAt: s.now().UTC(), ExpiresAt: expires,
	}
	if machine.OwnerTeamID.Valid {
		binding.User, binding.Team = actor, machine.OwnerTeamID.String
	}
	err := accountingauthority.Record(ctx, s.db, binding, s.runtimeQuota)
	if errors.Is(err, accountingauthority.ErrThrottled) {
		return nil
	}
	if errors.Is(err, accountingauthority.ErrUnavailable) {
		return ErrProvisioningUnavailable
	}
	return err
}

// AdmitBrowserTerminalIngress records one console browser terminal
// attachment that the edge serves over the machine's runtime route: the attachment ID is the
// usage authority the edge reports under, nodeID the admitting edge. Payer as for descriptors.
func (s *Service) AdmitBrowserTerminalIngress(ctx context.Context, attachmentID, machineID, actor, nodeID string, expires time.Time) error {
	if s.runtimeQuota == nil {
		return nil
	}
	var machine dbsqlc.UserMachine
	err := s.db.SQL().QueryRowContext(ctx, `SELECT id,user_id,owner_team_id,environment_id FROM paperboat.user_machines WHERE id=$1 AND deleted_at IS NULL`, machineID).Scan(&machine.ID, &machine.UserID, &machine.OwnerTeamID, &machine.EnvironmentID)
	if err != nil {
		return err
	}
	route, err := s.db.Queries().GetActiveHelperRouteForEnvironment(ctx, machine.EnvironmentID)
	if err != nil {
		return ErrProvisioningUnavailable
	}
	route.AppliedNodeID = sql.NullString{String: nodeID, Valid: nodeID != ""}
	if actor == "" {
		actor = machine.UserID
	}
	return s.admitRuntimeIngress(ctx, route, machine, actor, attachmentID, expires)
}

// CheckBrowserTerminalLimit refuses opening one more console browser terminal past the plan's
// open browser terminals limit of the machine's workspace (planlimits).
func (s *Service) CheckBrowserTerminalLimit(ctx context.Context, machineID, terminalSessionID string) error {
	return s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		var scope planlimits.Scope
		err := tx.QueryRow(ctx, `SELECT user_id,coalesce(owner_team_id,'') FROM paperboat.user_machines WHERE id=$1 AND deleted_at IS NULL`, machineID).Scan(&scope.Account, &scope.Team)
		if errors.Is(err, sql.ErrNoRows) {
			return nil // the caller's own lookup answers the missing machine
		}
		if err != nil {
			return err
		}
		return planlimits.CheckBrowserTerminalTx(ctx, tx, scope, terminalSessionID)
	})
}

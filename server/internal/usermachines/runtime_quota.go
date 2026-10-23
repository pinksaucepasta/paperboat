package usermachines

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/accountingauthority"
	"github.com/pinksaucepasta/paperboat-server/internal/db/dbsqlc"
)

// ErrBandwidthQuotaExhausted refuses a session through the edge's runtime ingress because the
// paying workspace's relay and edge allowance is used up. Direct machine-to-machine traffic is
// never metered, so only the edge path is refused.
var ErrBandwidthQuotaExhausted = errors.New("workspace bandwidth allowance is exhausted")

// BandwidthQuotaError is ErrBandwidthQuotaExhausted with the console page where the payer adds
// bandwidth or upgrades; BillingURL is empty when the deployment has no console origin.
type BandwidthQuotaError struct{ BillingURL string }

func (e *BandwidthQuotaError) Error() string        { return ErrBandwidthQuotaExhausted.Error() }
func (e *BandwidthQuotaError) Is(target error) bool { return target == ErrBandwidthQuotaExhausted }

// ConfigureRuntimeQuota makes edge runtime sessions (exec, ssh, terminal, file transfer)
// admission-checked and accounted against the paying workspace. Nil leaves them unmetered.
// billingURL is the console's Billing page, returned with a refusal; it may be empty.
func (s *Service) ConfigureRuntimeQuota(quota accountingauthority.QuotaChecker, billingURL string) {
	s.runtimeQuota, s.runtimeBillingURL = quota, billingURL
}

// admitRuntimeIngress records the payer of one edge runtime session under its access session
// ID, so the edge's usage reports for that session debit the right workspace, and refuses the
// session when that workspace is out of allowance. A team machine is paid by the team, counted
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
	if errors.Is(err, accountingauthority.ErrQuotaExhausted) {
		return &BandwidthQuotaError{BillingURL: s.runtimeBillingURL}
	}
	if errors.Is(err, accountingauthority.ErrUnavailable) {
		return ErrProvisioningUnavailable
	}
	return err
}

// AdmitBrowserTerminalIngress records and admission-checks one console browser terminal
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

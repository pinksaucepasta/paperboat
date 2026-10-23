package usermachines

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/audit"
	"github.com/pinksaucepasta/paperboat/server/internal/auth"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
	"github.com/pinksaucepasta/paperboat/server/internal/supportreport"
)

// PLAN F19-07: every machine access (terminal, exec, managed SSH, files) and every refusal is in
// the audit log of the workspace that decided it. The database derives that workspace from the
// event's capability and workspace selector (machine_access_decision), so a member's access to a
// machine shared into a team is in the team's log. Refusals carry the reason `pb policy explain`
// uses and the policy version that decided.
const (
	accessStartedEvent = "machine.access_started"
	accessDeniedEvent  = "machine.access_denied"
	accessResource     = "machine_access"
	// One refusal per account, machine, capability and reason per minute: retries do not flood the log.
	accessDeniedWindow = time.Minute
	accessAuditTimeout = 5 * time.Second
)

func accessMetadata(ctx context.Context, capability string) map[string]any {
	set, team := workspaceTeamArgument(ctx)
	metadata := map[string]any{"capability": capability, "scope_set": set}
	if team.Valid {
		metadata["scope_team"] = team.String
	}
	return metadata
}

// recordAccessStartedTx records an access session in the transaction that persists it, in the
// workspace the session was authorized in (in.TeamID, or personal).
func (s *Service) recordAccessStartedTx(ctx context.Context, tx *db.Tx, in dbsqlc.CreateUserMachineAccessSessionParams) error {
	metadata := map[string]any{"capability": in.Capabilities[0], "scope_set": true}
	if in.TeamID.Valid {
		metadata["scope_team"] = in.TeamID.String
	}
	metadata["capabilities"] = in.Capabilities
	metadata["access_session_id"] = in.ID
	return s.audit.WriteTx(ctx, tx, audit.Event{ActorUserID: in.UserID, ActorType: audit.ActorUser, EventType: accessStartedEvent,
		ResourceType: accessResource, ResourceID: in.UserMachineID, IdempotencyKey: in.ID, Metadata: metadata})
}

// machineForCapability is getMachineForCapability that records a refusal.
func (s *Service) machineForCapability(ctx context.Context, q *dbsqlc.Queries, in dbsqlc.GetUserMachineForCapabilityParams) (dbsqlc.UserMachine, error) {
	row, err := getMachineForCapability(ctx, q, in)
	if errors.Is(err, sql.ErrNoRows) || errors.Is(err, auth.ErrOutsideTokenScope) {
		s.recordAccessDenied(ctx, q, in, errors.Is(err, auth.ErrOutsideTokenScope))
	}
	return row, err
}

// recordAccessDenied writes the refusal on its own connection: the caller's transaction ends with
// the refusal's error and would take the event with it. A failed write is reported, never returned
// in place of the refusal.
func (s *Service) recordAccessDenied(ctx context.Context, q *dbsqlc.Queries, in dbsqlc.GetUserMachineForCapabilityParams, outsideTokenScope bool) {
	if s.audit == nil {
		return
	}
	set, team := workspaceTeamArgument(ctx)
	decision, err := q.MachineAccessDecision(ctx, dbsqlc.MachineAccessDecisionParams{Actor: in.UserID, Machine: in.ID, Capability: in.Capability, ScopeSet: set, ScopeTeam: team})
	if err == nil {
		reason := decision.Reason
		if outsideTokenScope {
			reason = "outside_token_scope"
		}
		if reason == "" {
			// Allowed now: the refusal raced a change that granted access.
			return
		}
		metadata := accessMetadata(ctx, in.Capability)
		metadata["reason"] = reason
		if decision.PolicyVersion > 0 {
			metadata["policy_version"] = decision.PolicyVersion
		}
		window := strconv.FormatInt(time.Now().Unix()/int64(accessDeniedWindow/time.Second), 10)
		write, cancel := context.WithTimeout(context.WithoutCancel(ctx), accessAuditTimeout)
		defer cancel()
		err = s.audit.Write(write, audit.Event{ActorUserID: in.UserID, ActorType: audit.ActorUser, EventType: accessDeniedEvent,
			ResourceType: accessResource, ResourceID: in.ID, Result: audit.ResultDenied, Reason: reason,
			IdempotencyKey: in.UserID + ":" + in.Capability + ":" + reason + ":" + window, Metadata: metadata})
	}
	if err != nil {
		supportreport.CaptureFromContext(ctx, "paperboat-server", "http_machines", "service_error", "machine_access_denied_audit_failed", err)
	}
}

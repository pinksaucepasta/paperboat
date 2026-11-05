package usermachines

import (
	"context"
	"database/sql"
	"errors"

	"github.com/pinksaucepasta/paperboat/server/internal/audit"
	"github.com/pinksaucepasta/paperboat/server/internal/auth"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/planlimits"
	"github.com/pinksaucepasta/paperboat/server/internal/teaminbox"
	"github.com/pinksaucepasta/paperboat/server/internal/workspace"
)

func (s *Service) recordSDKAuthorization(ctx context.Context, account, session, target, operation, capability string, err error) error {
	outcome := "authorized"
	if err != nil {
		var limit *planlimits.SDKLimitError
		outcome = "uncertain"
		if errors.As(err, &limit) || errors.Is(err, auth.ErrOutsideTokenScope) || errors.Is(err, ErrNotFound) || errors.Is(err, ErrInvalidTransferIntent) || errors.Is(err, ErrExecOperationInvalid) || errors.Is(err, workspace.ErrForbidden) || errors.Is(err, teaminbox.ErrPending) || errors.Is(err, ErrMachineCapabilityUnavailable) || errors.Is(err, ErrMachineOffline) {
			outcome = "denied"
		}
	}
	write, cancel := context.WithTimeout(context.WithoutCancel(ctx), accessAuditTimeout)
	defer cancel()
	return auth.RecordSDKAuthorization(write, s.audit, account, session, target, operation, capability, outcome)
}

func validSDKOperationEnd(e OfflineAuditEvent) bool {
	return (e.Capability == "exec" || e.Capability == "file_transfer" || e.Capability == "private_access") &&
		offlineAuditIdentifier.MatchString(e.SessionID) && offlineAuditIdentifier.MatchString(e.OperationID) &&
		(e.Outcome == "succeeded" || e.Outcome == "failed" || e.Outcome == "canceled" || e.Outcome == "uncertain") &&
		e.Origin == "" && e.Reason == "" && e.Generation == 0 && e.BytesIn == 0 && e.BytesOut == 0
}

// recordSDKOperationEndTx accepts only a current host's exact persisted token-session operation.
// The host names the peer's network endpoint, a daemon generation of the session (peersessions owner_endpoint_id).
func (s *Service) recordSDKOperationEndTx(ctx context.Context, tx *db.Tx, machine string, e OfflineAuditEvent) error {
	var token, team, capability, cliSession string
	err := tx.QueryRow(ctx, `SELECT c.api_token_id,coalesce(a.team_id,''),a.capabilities[1],c.id
 FROM paperboat.user_machine_access_sessions a JOIN paperboat.cli_client_sessions c ON c.id=a.cli_client_session_id AND c.user_id=a.user_id
 WHERE a.id=$1 AND a.user_machine_id=$2 AND a.user_id=$3 AND a.operation_id=$5
 AND a.cli_client_session_id=(SELECT n.owner_endpoint_id FROM paperboat.peer_network_identities n WHERE n.user_id=$3 AND n.endpoint_id=$4 AND n.role='cli')
 AND c.api_token_id IS NOT NULL AND a.credential_kind='control_plane_signed' FOR UPDATE OF a`,
		e.SessionID, machine, e.PeerAccountID, e.PeerEndpointID, e.OperationID).Scan(&token, &team, &capability, &cliSession)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrOfflineAuditInvalid
	}
	if err != nil {
		return err
	}
	expected := e.Capability
	if expected == "file_transfer" {
		expected = "files"
	}
	if capability != expected {
		return ErrOfflineAuditInvalid
	}
	if _, err := tx.Exec(ctx, `UPDATE paperboat.user_machine_access_sessions SET state='revoked',revoked_at=coalesce(revoked_at,now()),revocation_reason=coalesce(revocation_reason,'operation_ended'),updated_at=now() WHERE id=$1 AND state='active'`, e.SessionID); err != nil {
		return err
	}
	ws := "personal"
	if team != "" {
		ws = team
	}
	metadata := map[string]any{"api_token_id": token, "cli_client_session_id": cliSession, "workspace": ws,
		"operation_id": e.OperationID, "access_session_id": e.SessionID, "capability": capability, "outcome": e.Outcome,
		"scope_set": true, "occurred_at": e.OccurredAt.UTC(), "reporter": "destination"}
	if team != "" {
		metadata["scope_team"] = team
	}
	return s.audit.WriteTx(ctx, tx, audit.Event{ActorUserID: e.PeerAccountID, ActorTokenID: token, ActorType: audit.ActorUser,
		EventType: "sdk.operation_ended", ResourceType: accessResource, ResourceID: machine, IdempotencyKey: "offline:" + e.EventID, Metadata: metadata})
}

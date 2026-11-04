package auth

import (
	"context"

	"github.com/pinksaucepasta/paperboat/server/internal/audit"
	"github.com/pinksaucepasta/paperboat/server/internal/mint"
	"github.com/pinksaucepasta/paperboat/server/internal/workspace"
)

// RecordSDKAuthorization records only authority identifiers, never operation payloads.
func RecordSDKAuthorization(ctx context.Context, writer *audit.Writer, account, session, target, operation, capability, outcome string) error {
	token, ok := APITokenFromContext(ctx)
	if !ok {
		return nil
	}
	if !mint.ValidOperationID(operation) {
		operation = ""
	}
	scope, _ := workspace.FromContext(ctx)
	metadata := map[string]any{"api_token_id": token, "cli_client_session_id": session, "workspace": scope.ID,
		"operation_id": operation, "capability": capability, "outcome": outcome, "scope_set": true}
	if scope.TeamID != "" {
		metadata["scope_team"] = scope.TeamID
	}
	result, reason := audit.ResultSuccess, ""
	if outcome == "denied" {
		result, reason = audit.ResultDenied, "authorization_denied"
	}
	return writer.Write(ctx, audit.Event{ActorUserID: account, ActorTokenID: token, ActorType: audit.ActorUser,
		EventType: "sdk.authorization", ResourceType: "machine_access", ResourceID: target, Result: result, Reason: reason, Metadata: metadata})
}

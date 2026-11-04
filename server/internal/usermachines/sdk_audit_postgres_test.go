package usermachines

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestSDKSessionAuditAttribution(t *testing.T) {
	f, _, signer := newWorkspaceDescriptorFixture(t)
	ctx, token, session := sdkSession(t, f, "exec")
	descriptor, err := f.service.ExecDescriptor(ctx, f.account, "", f.machine, session, "operation_sdk_audit", true)
	if err != nil {
		t.Fatal(err)
	}
	credential, _ := descriptor.Auth["token"].(string)
	claims, err := signer.VerifyCredential(credential, f.service.issuer, "exec_operation", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	const privatePayload = "PRIVATE_COMMAND_OR_FILE_CONTENT"
	if _, err := f.service.ExecDescriptor(ctx, f.account, privatePayload, f.machine, session, "operation_sdk_audit_denied", true); err == nil {
		t.Fatal("forged source accepted")
	}
	f.service.controlSigner = nil
	if _, err := f.service.ExecDescriptor(ctx, f.account, "", f.machine, session, "operation_sdk_audit_uncertain", true); err == nil {
		t.Fatal("missing signer accepted")
	}
	f.service.controlSigner = signer
	event := OfflineAuditEvent{EventID: "sdk_audit_end_" + f.suffix, Kind: "sdk_operation_ended", Capability: "exec", PeerEndpointID: session, PeerAccountID: f.account, OperationID: claims.OperationID, SessionID: claims.AssignmentID, Outcome: "uncertain", OccurredAt: time.Now().UTC()}
	receipt, err := f.service.RecordOfflineAuditBacklog(ctx, f.machine, 1, []OfflineAuditEvent{event})
	if err != nil || receipt.Accepted != 1 {
		t.Fatalf("operation outcome: %v", err)
	}
	if again, err := f.service.RecordOfflineAuditBacklog(ctx, f.machine, 1, []OfflineAuditEvent{event}); err != nil || again.Duplicates != 1 {
		t.Fatal("destination outcome was not idempotent")
	}
	forged := event
	forged.EventID += "_forged"
	forged.OperationID = "operation_substituted"
	if _, err := f.service.RecordOfflineAuditBacklog(ctx, f.machine, 1, []OfflineAuditEvent{forged}); err == nil {
		t.Fatal("substituted operation outcome accepted")
	}
	rows, err := f.store.SQL().QueryContext(ctx, `SELECT event_type,actor_user_id,actor_token_id,result,metadata FROM paperboat.audit_events WHERE resource_id=$1 AND event_type IN ('sdk.authorization','sdk.operation_ended') ORDER BY created_at,id`, f.machine)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	outcomes := map[string]bool{}
	for rows.Next() {
		var kind, actor, actorToken, result string
		var raw []byte
		if err := rows.Scan(&kind, &actor, &actorToken, &result, &raw); err != nil {
			t.Fatal(err)
		}
		var metadata map[string]any
		if err := json.Unmarshal(raw, &metadata); err != nil {
			t.Fatal(err)
		}
		if actor != f.account || actorToken != token || metadata["cli_client_session_id"] != session || metadata["workspace"] != "personal" || metadata["operation_id"] == "" || metadata["capability"] != "exec" {
			t.Fatal("audit lost creator/token/session/workspace/target/operation attribution")
		}
		if strings.Contains(string(raw), privatePayload) || strings.Contains(string(raw), credential) {
			t.Fatal("private payload or credential reached audit metadata")
		}
		outcome, _ := metadata["outcome"].(string)
		outcomes[kind+":"+outcome] = true
		if outcome == "denied" && result != "denied" {
			t.Fatal("denial recorded as success")
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"sdk.authorization:authorized", "sdk.authorization:denied", "sdk.authorization:uncertain", "sdk.operation_ended:uncertain"} {
		if !outcomes[expected] {
			t.Fatalf("missing audit outcome %s", expected)
		}
	}
}

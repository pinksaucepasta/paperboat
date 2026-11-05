package usermachines

import (
	"bytes"
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
	// The host reports the peer's network endpoint: the token session's daemon generation.
	endpoint := session + "_0123456789abcdef0123456789abcdef"
	fingerprint := bytes.Repeat([]byte{7}, 32)
	if _, err := f.store.SQL().ExecContext(ctx, `INSERT INTO paperboat.peer_endpoint_certificates(fingerprint,user_id,endpoint_id,role,generation,serial,certificate,quic_public_key,issued_at,expires_at,key_id,signer_public_key) VALUES($1,$2,$3,'cli',1,1,$4,$5,now()-interval '1 minute',now()+interval '1 hour','sdk-audit',$5)`, fingerprint, f.account, session, bytes.Repeat([]byte{1}, 140), make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.SQL().ExecContext(ctx, `INSERT INTO paperboat.peer_network_identities(user_id,endpoint_id,cli_client_session_id,role,endpoint_generation,machine_generation,key_generation,wireguard_public_key,quic_certificate_fingerprint,virtual_address,created_at,updated_at) VALUES($1,$2,$3,'cli',1,0,1,$4,$4,('fd7a:115c:a1e0::'::inet + nextval('paperboat.peer_network_virtual_address_seq')),now(),now())`, f.account, endpoint, session, fingerprint); err != nil {
		t.Fatal(err)
	}
	event := OfflineAuditEvent{EventID: "sdk_audit_end_" + f.suffix, Kind: "sdk_operation_ended", Capability: "exec", PeerEndpointID: endpoint, PeerAccountID: f.account, OperationID: claims.OperationID, SessionID: claims.AssignmentID, Outcome: "uncertain", OccurredAt: time.Now().UTC()}
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

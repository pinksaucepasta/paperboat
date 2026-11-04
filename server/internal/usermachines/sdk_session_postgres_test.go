package usermachines

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/auth"
	"github.com/pinksaucepasta/paperboat/server/internal/workspace"
)

func sdkSession(t *testing.T, f workspaceMachineFixture, capability string) (context.Context, string, string) {
	t.Helper()
	token, session := "sdk_token_"+f.suffix, "sdk_session_"+f.suffix
	hash := sha256.Sum256([]byte(token))
	t.Cleanup(func() {
		for _, q := range []string{
			`DELETE FROM paperboat.cli_client_sessions WHERE api_token_id=$1`,
			`DELETE FROM paperboat.api_token_scope_rules WHERE token_id=$1`,
			`DELETE FROM paperboat.api_tokens WHERE id=$1`,
		} {
			if _, err := f.store.SQL().ExecContext(context.Background(), q, token); err != nil {
				t.Error(err)
			}
		}
	})
	for _, stmt := range []struct {
		q    string
		args []any
	}{
		{`INSERT INTO paperboat.api_tokens(id,account_id,name,secret_hash,scope,expires_at) VALUES($1,$2,$1,$3,'[]',now()+interval '1 hour')`, []any{token, f.account, hash[:]}},
		{`INSERT INTO paperboat.api_token_scope_rules(token_id,entry,target_kind,target_id,action,ports) VALUES($1,0,'machine',$2,$3,'[5432,5433)')`, []any{token, f.machine, capability}},
		{`INSERT INTO paperboat.cli_client_sessions(id,user_id,client_id,client_label,device_type,os,scopes,state,created_at,approved_at,api_token_id) VALUES($1,$2,'sdk','sdk','container','linux','{machines:connect}','active',now(),now(),$3)`, []any{session, f.account, token}},
	} {
		if _, err := f.store.SQL().ExecContext(f.ctx, stmt.q, stmt.args...); err != nil {
			t.Fatal(err)
		}
	}

	return auth.WithAPIToken(workspace.WithContext(f.ctx, workspace.Scope{ID: "personal"}), token), token, session
}

func TestSDKSessionExecSourceAuthorization(t *testing.T) {
	f, source, signer := newWorkspaceDescriptorFixture(t)
	ctx, token, session := sdkSession(t, f, "exec")
	descriptor, err := f.service.ExecDescriptor(ctx, f.account, "", f.machine, session, "operation_sdk_exec", true)
	if err != nil {
		t.Fatal(err)
	}
	credential, _ := descriptor.Auth["token"].(string)
	claims, err := signer.VerifyCredential(credential, f.service.issuer, "exec_operation", time.Now())
	if err != nil || claims.SourceMachineID != "" || claims.CLIClientSessionID != session || claims.MachineID != f.machine || claims.WorkspaceID != "personal" || claims.ActorAccountID != f.account {
		t.Fatal("exec credential lost exact session, target or workspace binding")
	}
	for _, tc := range []struct {
		name, source, target, peer string
		ctx                        context.Context
	}{
		{"forged source", source, f.machine, session, ctx},
		{"forged peer", "", f.machine, "foreign-peer", ctx},
		{"unauthorized target", "", source, session, ctx},
		{"workspace substitution", "", f.machine, session, workspace.WithContext(ctx, workspace.Scope{ID: f.teamOne, TeamID: f.teamOne})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, err := f.service.ExecDescriptor(tc.ctx, f.account, tc.source, tc.target, tc.peer, "operation_sdk_denied", true)
			if err == nil || d.Auth != nil {
				t.Fatal("unauthorized descriptor issued")
			}
		})
	}
	if _, err := f.store.SQL().ExecContext(ctx, `DELETE FROM paperboat.api_token_scope_rules WHERE token_id=$1`, token); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.ExecDescriptor(ctx, f.account, "", f.machine, session, "operation_sdk_scope", true); !errors.Is(err, auth.ErrOutsideTokenScope) {
		t.Fatalf("missing exec scope: %v", err)
	}
}

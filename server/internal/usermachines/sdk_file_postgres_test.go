package usermachines

import (
	"errors"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/auth"
)

func TestSDKSessionFileSourceAuthorization(t *testing.T) {
	f, source, signer := newWorkspaceDescriptorFixture(t)
	ctx, token, session := sdkSession(t, f, "files")
	intent := TransferIntent{Direction: "send", Mode: "automatic", DeclaredBytes: 7}
	descriptor, err := f.service.FileTransferDescriptorForRequest(ctx, f.account, "", f.machine, session, "", "", "", intent)
	if err != nil {
		t.Fatal(err)
	}
	credential, _ := descriptor.Auth["token"].(string)
	claims, err := signer.VerifyCredential(credential, f.service.issuer, "file_transfer", time.Now())
	if err != nil || claims.CLIClientSessionID != session || claims.SourceMachineID != "" || descriptor.SourceMachineID != "" || claims.MachineID != f.machine || claims.WorkspaceID != "personal" || claims.TransferDirection != "send" || claims.TransferMode != "automatic" || claims.MaxBytes != 7 || claims.OperationID == "" {
		t.Fatal("file credential lost source, target, workspace, direction or mode")
	}
	for _, tc := range []struct {
		name, source, target, peer, terminal string
		intent                               TransferIntent
	}{
		{"forged source", source, f.machine, session, "", intent},
		{"forged peer", "", f.machine, "foreign-peer", "", intent},
		{"unauthorized target", "", source, session, "", intent},
		{"terminal route substitution", "", f.machine, session, "foreign-terminal", intent},
		{"receive direction", "", f.machine, session, "", TransferIntent{Direction: "receive", Mode: "automatic", DeclaredBytes: 7}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, e := f.service.FileTransferDescriptorForRequest(ctx, f.account, tc.source, tc.target, tc.peer, tc.terminal, "", "", tc.intent)
			if e == nil || d.Auth != nil {
				t.Fatal("unauthorized file descriptor issued")
			}
		})
	}
	if _, err := f.store.SQL().ExecContext(ctx, `UPDATE paperboat.api_token_scope_rules SET action='exec' WHERE token_id=$1`, token); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.FileTransferDescriptorForRequest(ctx, f.account, "", f.machine, session, "", "", "", intent); !errors.Is(err, auth.ErrOutsideTokenScope) {
		t.Fatalf("exec-only file request: %v", err)
	}
}

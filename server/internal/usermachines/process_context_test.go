package usermachines

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/mint"
	"github.com/pinksaucepasta/paperboat/server/internal/workspace"
)

func TestProcessCredentialRetainsTerminalCreationWorkspace(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	suffix := fmt.Sprint(time.Now().UnixNano())
	owner, actor, machine, session, team := "pc_owner_"+suffix, "pc_actor_"+suffix, "pc_machine_"+suffix, "terminal_pc_"+suffix, "pc-team-"+suffix
	insert := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	insert(`INSERT INTO users(id,workos_subject,primary_email,status) SELECT x,x,x||'@invalid.test','active' FROM unnest($1::text[]) x`, []string{owner, actor})
	insert(`INSERT INTO teams(team_id,owner_account,generation) VALUES($1,$2,1)`, team, owner)
	insert(`INSERT INTO team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'admin',true)`, team, owner)
	insert(`INSERT INTO user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,online) VALUES($1,$2,$1,'process-context','linux','amd64','/fixture','online','occupied',true)`, machine, owner)
	insert(`INSERT INTO user_machine_terminal_sessions(id,user_machine_id,owner_account,owner_team_id,name,launch_cwd,runtime_state) VALUES($1,$2,$3,$4,'process-context','/fixture','running')`, session, machine, owner, team)
	signer, err := mint.NewEphemeral(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	service := New(store, nil, Policy{}, nil)
	service.controlSigner = signer
	now := time.Now().UTC()
	input := mint.CredentialInput{Issuer: "https://api.example.test", Audience: "paperboat-machine", Subject: actor, UserID: actor, AccountID: actor, JTI: "process_context_token", IssuedAt: now, ExpiresAt: now.Add(time.Minute), CredentialClass: "terminal_operation", Scopes: []string{"terminal:control"}, MachineID: machine, EnvironmentID: machine, SessionID: session, CLIClientSessionID: "client", AssignmentID: "authorized_shared_session", ExpectedGeneration: 1}
	for _, selected := range []workspace.Scope{{}, {TeamID: "different_sharing_workspace"}} {
		token, err := service.signProcessCredential(workspace.WithContext(ctx, selected), input)
		if err != nil {
			t.Fatal(err)
		}
		claims, err := signer.VerifyCredential(token, input.Issuer, input.CredentialClass, now)
		if err != nil || claims.WorkspaceID != team || claims.ActorAccountID != actor {
			t.Fatalf("viewer changed terminal creation context: %v", err)
		}
	}
	input.Subject, input.UserID, input.AccountID = owner, owner, owner
	input.Scopes = []string{"terminal:operate"}
	token, err := service.signProcessCredential(ctx, input)
	if err != nil {
		t.Fatalf("active owner could not launch Team terminal: %v", err)
	}
	claims, err := signer.VerifyCredential(token, input.Issuer, input.CredentialClass, now)
	if err != nil || claims.WorkspaceID != team || claims.ActorAccountID != owner {
		t.Fatalf("owner terminal lost creation workspace: %v", err)
	}
	insert(`UPDATE team_members SET active=false WHERE team_id=$1 AND account_id=$2`, team, owner)
	if _, err := service.signProcessCredential(ctx, input); !errors.Is(err, workspace.ErrForbidden) {
		t.Fatalf("revoked owner membership was not rejected: %v", err)
	}
	input.MachineID = "other-machine"
	if _, err := service.signProcessCredential(ctx, input); err == nil {
		t.Fatal("a foreign machine received the terminal creation context")
	}
}

package usermachines

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/audit"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
	"github.com/pinksaucepasta/paperboat/server/internal/teams"
	"github.com/pinksaucepasta/paperboat/server/internal/workspace"
)

// Access sessions do not wait for the global team write lock (Lane A: concurrent exec setups queued ~430 ms
// apart behind it; Lane X S2). While another transaction holds the lock, ten concurrent personal access
// sessions and ten team-member sessions on a shared machine all start.
func TestPostgresPersonalAccessSessionsDoNotQueueOnTheTeamLock(t *testing.T) {
	store := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	suffix := fmt.Sprint(time.Now().UnixNano())
	owner, machine := "pa_owner_"+suffix, "pa_machine_"+suffix
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@invalid.test','active')`, owner); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machines(id,user_id,environment_id,platform,architecture,workspace_root,state,seat_state,online,configured_capabilities,observed_capabilities) VALUES($1,$2,$3,'linux','amd64','/workspace','online','occupied',true,ARRAY['terminal_host'],ARRAY['terminal_host'])`, machine, owner, "env_"+suffix); err != nil {
		t.Fatal(err)
	}
	service := New(store, audit.NewWriter(store), Policy{}, nil)
	session, err := service.CreateTerminalSession(ctx, owner, machine, "personal", "op-personal")
	if err != nil {
		t.Fatal(err)
	}

	// A team member's access to a machine shared with the team (as in the shared-access test).
	member, team := "pa_member_"+suffix, "pa-team-"+suffix
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@invalid.test','active')`, member); err != nil {
		t.Fatal(err)
	}
	authority := teams.NewService(store)
	current, err := authority.Create(ctx, owner, teams.CreateRequest{OperationID: "create-" + suffix, TeamID: team})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err = store.SQL().ExecContext(ctx, `INSERT INTO paperboat.billing_workspaces(id,team_id,state,product_code,quantity,period_start,period_end) VALUES($1,$2,'active','team-pro',2,$3,$4)`, "bw_"+team, team, now.Add(-time.Hour), now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if current, err = authority.Get(ctx, owner, team); err != nil {
		t.Fatal(err)
	}
	invite, err := authority.Invite(ctx, owner, team, teams.InviteRequest{OperationID: "invite-" + suffix, ExpectedGeneration: current.Generation, AccountID: member})
	if err != nil {
		t.Fatal(err)
	}
	if current, err = authority.Accept(ctx, member, invite.InvitationID, teams.AcceptRequest{OperationID: "accept-" + suffix}); err != nil {
		t.Fatal(err)
	}
	if current, err = authority.Machine(ctx, owner, team, teams.MachineRequest{OperationID: "share-" + suffix, ExpectedGeneration: current.Generation, MachineID: machine, Action: "share"}); err != nil {
		t.Fatal(err)
	}
	if _, err = authority.GrantMachine(ctx, owner, team, teams.MachineGrantRequest{OperationID: "grant-" + suffix, ExpectedGeneration: current.Generation, MachineID: machine, Audience: "selected_member", AccountID: member, Capabilities: []string{"terminal"}, Active: true}); err != nil {
		t.Fatal(err)
	}
	teamCtx := workspace.WithContext(ctx, workspace.Scope{ID: team, TeamID: team})

	held, release := make(chan struct{}), make(chan struct{})
	holder := make(chan error, 1)
	go func() {
		holder <- store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
			if err := teams.Lock(ctx, tx); err != nil {
				return err
			}
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	errs := make(chan error, 20)
	for i := range 10 {
		go func() {
			callCtx, cancel := context.WithTimeout(teamCtx, 5*time.Second)
			defer cancel()
			id := fmt.Sprintf("session_pt_%s_%d", suffix, i)
			errs <- service.createAccessSession(callCtx, dbsqlc.CreateUserMachineAccessSessionParams{ID: id, UserMachineID: machine, UserID: member, EnvironmentID: "env_" + suffix, CLIClientSessionID: "cli_" + member, HttpBaseUrl: "https://invalid.test", HelperTerminalSessionID: "jti_" + id, ExpiresAt: time.Now().Add(time.Minute), Capabilities: []string{"terminal"}, OperationID: session.ID}, machineCredentialExternal)
		}()
	}
	for i := range 10 {
		go func() {
			callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			id := fmt.Sprintf("session_pa_%s_%d", suffix, i)
			errs <- service.createAccessSession(callCtx, dbsqlc.CreateUserMachineAccessSessionParams{ID: id, UserMachineID: machine, UserID: owner, EnvironmentID: "env_" + suffix, CLIClientSessionID: "cli_" + owner, HttpBaseUrl: "https://invalid.test", HelperTerminalSessionID: "jti_" + id, ExpiresAt: time.Now().Add(time.Minute), Capabilities: []string{"exec"}, OperationID: session.ID}, machineCredentialExternal)
		}()
	}
	for range 20 {
		if err := <-errs; err != nil {
			t.Errorf("access session while the team lock is held: %v", err)
		}
	}
	close(release)
	if err := <-holder; err != nil {
		t.Fatal(err)
	}
}

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
	generation := func() (n int64) {
		_ = store.SQL().QueryRowContext(ctx, `SELECT coalesce((SELECT generation FROM paperboat.machine_topic_generations WHERE machine_id=$1 AND topic='machine_access'),0)`, machine).Scan(&n)
		return n
	}
	before := generation()
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
	// The 10 team-member sessions wake the host (their account may be new to its peer network); the 10 personal
	// exec sessions are admitted from the host's standing rule and must not.
	if after := generation(); after != before+10 {
		t.Errorf("machine_access topic generation %d -> %d: want exactly the 10 team sessions to wake the host, not the 10 personal exec sessions", before, after)
	}
	// Sessions of one machine must not conflict with each other: they used to replay ~4 times each on the machine's
	// topic row (bumped by an insert trigger), and 20 at once ran out of retries (40001 to the caller).
	if stats := store.TransactionStats().Serializable; stats.Retries > 2 || stats.Exhausted != 0 {
		t.Errorf("20 concurrent access sessions of one machine replayed %d times (%+v); they must not conflict", stats.Retries, stats)
	}
	close(release)
	if err := <-holder; err != nil {
		t.Fatal(err)
	}
}

// A personal exec or terminal session leaves the host alone; a capability that needs a per-session scope still wakes it (team access is covered by the network lifecycle test).
func TestPostgresAccessSessionWakesTheHostOnlyForCapabilitiesThatNeedAScope(t *testing.T) {
	store := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	suffix := fmt.Sprint(time.Now().UnixNano())
	owner, machine := "aw_owner_"+suffix, "aw_machine_"+suffix
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@invalid.test','active')`, owner); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machines(id,user_id,environment_id,platform,architecture,workspace_root,state,seat_state,online,configured_capabilities,observed_capabilities) VALUES($1,$2,$3,'linux','amd64','/workspace','online','occupied',true,ARRAY['terminal_host','file_receive'],ARRAY['terminal_host','file_receive'])`, machine, owner, "env_"+suffix); err != nil {
		t.Fatal(err)
	}
	service := New(store, audit.NewWriter(store), Policy{}, nil)
	generation := func() (n int64) {
		_ = store.SQL().QueryRowContext(ctx, `SELECT coalesce((SELECT generation FROM paperboat.machine_topic_generations WHERE machine_id=$1 AND topic='machine_access'),0)`, machine).Scan(&n)
		return n
	}
	for i, test := range []struct {
		capabilities []string
		wakes        bool
	}{{[]string{"exec"}, false}, {[]string{"terminal"}, false}, {[]string{"terminal", "exec"}, false}, {[]string{"files"}, true}, {[]string{"terminal", "files"}, true}} {
		before := generation()
		id := fmt.Sprintf("session_aw_%s_%d", suffix, i)
		if err := service.createAccessSession(ctx, dbsqlc.CreateUserMachineAccessSessionParams{ID: id, UserMachineID: machine, UserID: owner, EnvironmentID: "env_" + suffix, CLIClientSessionID: "cli_" + owner, HttpBaseUrl: "https://example.test", HelperTerminalSessionID: "jti_" + id, ExpiresAt: time.Now().Add(time.Minute), Capabilities: test.capabilities, OperationID: id}, machineCredentialExternal); err != nil {
			t.Fatalf("%v: %v", test.capabilities, err)
		}
		if woke := generation() != before; woke != test.wakes {
			t.Errorf("capabilities %v: host woken=%v, want %v", test.capabilities, woke, test.wakes)
		}
		// Ending a session always wakes the host, whatever it carried: the revoke must reach it at once.
		before = generation()
		if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.user_machine_access_sessions SET state='revoked', revoked_at=now(), updated_at=now(), revocation_reason='test' WHERE id=$1`, id); err != nil {
			t.Fatal(err)
		}
		if generation() == before {
			t.Errorf("capabilities %v: revoking the session did not wake the host", test.capabilities)
		}
	}
}

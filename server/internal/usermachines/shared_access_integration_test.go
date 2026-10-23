package usermachines

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/pinksaucepasta/paperboat-server/internal/teams/teamstest"
	"slices"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/audit"
	"github.com/pinksaucepasta/paperboat-server/internal/db/dbsqlc"
	"github.com/pinksaucepasta/paperboat-server/internal/teams"
)

func TestPostgresSharedMachineSessionIsolationAndRevocation(t *testing.T) {
	store := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	suffix := fmt.Sprint(time.Now().UnixNano())
	owner, member, team, machine := "sm_owner_"+suffix, "sm_member_"+suffix, "sm-team-"+suffix, "sm_machine_"+suffix
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users(id,workos_subject,primary_email,status) SELECT x,x,x||'@invalid.test','active' FROM unnest($1::text[]) x`, []string{owner, member}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machines(id,user_id,environment_id,platform,architecture,workspace_root,state,seat_state,online,configured_capabilities,observed_capabilities) VALUES($1,$2,$3,'linux','amd64','/workspace','online','occupied',true,ARRAY['terminal_host','file_receive'],ARRAY['terminal_host','file_receive'])`, machine, owner, "env_"+suffix); err != nil {
		t.Fatal(err)
	}
	authority := teams.NewService(store)
	current, err := authority.Create(ctx, owner, teams.CreateRequest{OperationID: "create", TeamID: team})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err = store.SQL().ExecContext(ctx, `INSERT INTO paperboat.billing_workspaces(id,team_id,state,product_code,quantity,period_start,period_end) VALUES($1,$2,'active','team-pro',2,$3,$4)`, "bw_"+team, team, now.Add(-time.Hour), now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	current, err = authority.Get(ctx, owner, team)
	if err != nil {
		t.Fatal(err)
	}
	invite, err := authority.Invite(ctx, owner, team, teams.InviteRequest{OperationID: "invite", ExpectedGeneration: current.Generation, AccountID: member})
	if err != nil {
		t.Fatal(err)
	}
	current, err = authority.Accept(ctx, member, invite.InvitationID, teams.AcceptRequest{OperationID: "accept"})
	if err != nil {
		t.Fatal(err)
	}
	service := New(store, audit.NewWriter(store), Policy{}, nil)
	if _, err = service.CreateTerminalSession(ctx, member, machine, "same-name", "before-share"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unshared session creation: %v", err)
	}
	current, err = authority.Machine(ctx, owner, team, teams.MachineRequest{OperationID: "share", ExpectedGeneration: current.Generation, MachineID: machine, Action: "share"})
	if err != nil {
		t.Fatal(err)
	}
	current, err = authority.GrantMachine(ctx, owner, team, teams.MachineGrantRequest{OperationID: "terminal-grant", ExpectedGeneration: current.Generation, MachineID: machine, Audience: "selected_member", AccountID: member, Capabilities: []string{"terminal"}, Active: true})
	if err != nil {
		t.Fatal(err)
	}
	current, err = authority.Get(ctx, owner, team)
	if err != nil {
		t.Fatal(err)
	}
	visible, total, err := service.List(ctx, member, 50, 0)
	if err != nil || total != 1 || len(visible) != 1 || visible[0].CanManage || len(visible[0].Permissions) != 1 || visible[0].Permissions[0] != "terminal" {
		t.Fatalf("shared machine visibility/permissions: total=%d count=%d err=%v", total, len(visible), err)
	}
	renamed, err := service.Rename(ctx, owner, machine, "shared-host")
	ownerPermissions := []string{"terminal", "exec", "managed_ssh", "files", "preview_manage", "tunnel_manage", "private_access"}
	if err != nil || !renamed.CanManage || !slices.Equal(renamed.Permissions, ownerPermissions) {
		t.Fatalf("rename management presentation: can_manage=%t permissions=%v error=%v", renamed.CanManage, renamed.Permissions, err)
	}
	first, err := service.CreateTerminalSession(ctx, owner, machine, "same-name", "owner-session")
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.CreateTerminalSession(ctx, member, machine, "same-name", "member-session")
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == second.ID {
		t.Fatal("members share implicit terminal")
	}
	titled, err := service.RenameTerminalSession(ctx, owner, machine, first.ID, "api-debugging")
	if err != nil || titled.ID != first.ID || titled.Name != "api-debugging" || titled.Title != "" || !titled.CreatedAt.Equal(first.CreatedAt) {
		t.Fatalf("owner rename: session=%+v err=%v", titled, err)
	}
	// The reserved session and owner-local namespace retain their authority.
	reservedID, conflictID := "sm_default_"+suffix, "sm_conflict_"+suffix
	if _, err = store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machine_terminal_sessions(id,user_machine_id,owner_account,name,is_default,launch_cwd) VALUES($1,$3,$4,'default',true,'/workspace'),($2,$3,$4,'occupied',false,'/workspace')`, reservedID, conflictID, machine, owner); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		if _, cleanupErr := store.SQL().ExecContext(cleanup, `DELETE FROM paperboat.user_machine_terminal_sessions WHERE id=ANY($1::text[])`, []string{reservedID, conflictID}); cleanupErr != nil {
			t.Error(cleanupErr)
		}
	}()
	if _, err = service.RenameTerminalSession(ctx, owner, machine, first.ID, "occupied"); !errors.Is(err, ErrTerminalSessionConflict) {
		t.Fatalf("same-owner name collision: %v", err)
	}
	if _, err = service.RenameTerminalSession(ctx, owner, machine, first.ID, "default"); !errors.Is(err, ErrTerminalSessionInvalidName) {
		t.Fatalf("reserved name accepted: %v", err)
	}
	if _, err = service.RenameTerminalSession(ctx, owner, machine, reservedID, "changed-default"); !errors.Is(err, ErrTerminalSessionNotFound) {
		t.Fatalf("reserved default renamed: %v", err)
	}
	var persistedName, persistedLaunch, persistedState string
	if err = store.SQL().QueryRowContext(ctx, `SELECT name,launch_cwd,runtime_state FROM paperboat.user_machine_terminal_sessions WHERE id=$1`, first.ID).Scan(&persistedName, &persistedLaunch, &persistedState); err != nil || persistedName != "api-debugging" || persistedLaunch != "/workspace" || persistedState != "unknown" {
		t.Fatalf("rename changed durable process identity: name=%q launch=%q state=%q error=%v", persistedName, persistedLaunch, persistedState, err)
	}
	if _, err = service.RenameTerminalSession(ctx, member, machine, first.ID, "stolen"); !errors.Is(err, ErrTerminalSessionNotFound) {
		t.Fatalf("foreign session rename: %v", err)
	}
	if _, err = service.RenameTerminalSession(ctx, member, machine, second.ID, "member-work"); err != nil {
		t.Fatalf("session owner rename: %v", err)
	}
	if _, err = service.CloseTerminalSession(ctx, member, machine, first.ID); !errors.Is(err, ErrTerminalSessionNotFound) {
		t.Fatalf("foreign session close: %v", err)
	}
	if _, err = service.DeleteTerminalSession(ctx, member, machine, first.ID); !errors.Is(err, ErrTerminalSessionNotFound) {
		t.Fatalf("foreign session delete: %v", err)
	}
	if _, err = service.terminalSession(ctx, member, machine, first.ID); !errors.Is(err, ErrTerminalSessionNotFound) {
		t.Fatalf("foreign attachment: %v", err)
	}
	sessions, err := service.ListTerminalSessions(ctx, member, machine)
	if err != nil || len(sessions) != 1 || sessions[0].ID != second.ID || sessions[0].Title != "" || sessions[0].Name != "member-work" || sessions[0].Machine != "shared-host" || sessions[0].StartedIn != "/workspace" || sessions[0].State != "unavailable" {
		t.Fatalf("session visibility: count=%d err=%v", len(sessions), err)
	}
	persist := func(actor, id, capability string) error {
		return service.createAccessSession(ctx, dbsqlc.CreateUserMachineAccessSessionParams{ID: id, UserMachineID: machine, UserID: actor, EnvironmentID: "env_" + suffix, CLIClientSessionID: "cli_" + actor, HttpBaseUrl: "https://invalid.test", HelperTerminalSessionID: "jti_" + id, ExpiresAt: time.Now().Add(time.Minute), Capabilities: []string{capability}, OperationID: second.ID}, machineCredentialExternal)
	}
	if err = persist(member, "denied_"+suffix, "exec"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ungranted exec issuance: %v", err)
	}
	ownAccess, sharedAccess := "own_"+suffix, "shared_"+suffix
	if err = persist(owner, ownAccess, "terminal"); err != nil {
		t.Fatal(err)
	}
	if err = persist(member, sharedAccess, "terminal"); err != nil {
		t.Fatal(err)
	}
	// PLAN F19-07: access starts are recorded in the workspace that authorized them (the member's
	// access to the owner's shared machine in the team, not in the owner's personal log); refusals
	// with their reason, once per minute.
	if err = persist(member, "denied_again_"+suffix, "exec"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("repeated ungranted exec issuance: %v", err)
	}
	type accessEvent struct{ event, workspace, result, reason string }
	var events []accessEvent
	rows, err := store.SQL().QueryContext(ctx, `SELECT event_type,coalesce(activity_workspace_id,''),result,coalesce(reason,'') FROM paperboat.audit_events
 WHERE resource_type='machine_access' AND resource_id=$1 ORDER BY cursor_sequence`, machine)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var e accessEvent
		if err = rows.Scan(&e.event, &e.workspace, &e.result, &e.reason); err != nil {
			t.Fatal(err)
		}
		events = append(events, e)
	}
	rows.Close()
	want := []accessEvent{
		{"machine.access_denied", "personal:" + member, "denied", "machine_not_in_workspace"},
		{"machine.access_denied", "team:" + team, "denied", "policy"},
		{"machine.access_started", "personal:" + owner, "success", ""},
		{"machine.access_started", "team:" + team, "success", ""},
	}
	if !slices.Equal(events, want) {
		t.Fatalf("machine access audit:\n got %v\nwant %v", events, want)
	}
	decide := func(actor, capability string) string {
		t.Helper()
		d, err := store.Queries().MachineAccessDecision(ctx, dbsqlc.MachineAccessDecisionParams{Actor: actor, Machine: machine, Capability: capability})
		if err != nil {
			t.Fatal(err)
		}
		return d.Workspace + " " + d.Reason
	}
	if _, err = store.SQL().ExecContext(ctx, `UPDATE paperboat.teams SET sso_required=true,sso_session_seconds=86400 WHERE team_id=$1`, team); err != nil {
		t.Fatal(err)
	}
	if got := decide(member, "terminal"); got != "team:"+team+" team_sso_required" {
		t.Fatalf("SSO-required refusal: %s", got)
	}
	if _, err = store.SQL().ExecContext(ctx, `UPDATE paperboat.teams SET sso_required=false WHERE team_id=$1`, team); err != nil {
		t.Fatal(err)
	}
	if got := decide(member, "terminal"); got != "team:"+team+" " {
		t.Fatalf("allowed member decision: %s", got)
	}
	if got := decide("sm_stranger_"+suffix, "terminal"); got != "team:"+team+" machine_not_in_workspace" {
		t.Fatalf("non-member refusal: %s", got)
	}
	secondTeam :="sm_team_second_" + suffix
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$2,1)`, []any{secondTeam, owner}},
		{`INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'member',true),($1,$3,1,'member',true)`, []any{secondTeam, owner, member}},
		{`INSERT INTO paperboat.team_resource_bindings(team_id,resource_kind,resource_id,owner_account) VALUES($1,'machine',$2,$3)`, []any{secondTeam, machine, owner}},
	} {
		if _, err = store.SQL().ExecContext(ctx, statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	teamstest.SetMachineAccess(t, store, secondTeam, machine, member, "terminal")
	if _, err = store.SQL().ExecContext(ctx, `INSERT INTO paperboat.billing_workspaces(id,team_id,state,product_code,quantity,period_start,period_end) VALUES($1,$2,'trialing','team-pro',2,$3,$4)`, "bw_"+secondTeam, secondTeam, now.Add(-time.Hour), now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err = persist(member, "ambiguous_"+suffix, "terminal"); !errors.Is(err, ErrAmbiguousTeamAccess) {
		t.Fatalf("multiply-authorized shared access: %v", err)
	}
	var ambiguousRows int
	if err = store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.user_machine_access_sessions WHERE id=$1`, "ambiguous_"+suffix).Scan(&ambiguousRows); err != nil || ambiguousRows != 0 {
		t.Fatalf("ambiguous access persisted payer: count=%d err=%v", ambiguousRows, err)
	}
	personalAccess := "personal_with_teams_" + suffix
	if err = persist(owner, personalAccess, "terminal"); err != nil {
		t.Fatalf("personal owner with team bindings: %v", err)
	}
	var personalTeam sql.NullString
	if err = store.SQL().QueryRowContext(ctx, `SELECT team_id FROM paperboat.user_machine_access_sessions WHERE id=$1`, personalAccess).Scan(&personalTeam); err != nil || personalTeam.Valid {
		t.Fatalf("personal owner assigned team payer: team=%v err=%v", personalTeam, err)
	}
	if _, err = store.SQL().ExecContext(ctx, `UPDATE paperboat.team_resource_bindings SET active=false,generation=generation+1 WHERE team_id=$1 AND resource_kind='machine' AND resource_id=$2`, secondTeam, machine); err != nil {
		t.Fatal(err)
	}
	current, err = authority.GrantMachine(ctx, owner, team, teams.MachineGrantRequest{OperationID: "revoke-terminal", ExpectedGeneration: current.Generation, MachineID: machine, Audience: "selected_member", AccountID: member, Capabilities: []string{"terminal"}, Active: false})
	if err != nil {
		t.Fatal(err)
	}
	var ownState, sharedState string
	if err = store.SQL().QueryRowContext(ctx, `SELECT a.state,b.state FROM paperboat.user_machine_access_sessions a,paperboat.user_machine_access_sessions b WHERE a.id=$1 AND b.id=$2`, ownAccess, sharedAccess).Scan(&ownState, &sharedState); err != nil {
		t.Fatal(err)
	}
	if ownState != "active" || sharedState != "revoked" {
		t.Fatalf("revocation affected unrelated access: own=%s shared=%s", ownState, sharedState)
	}
	if err = persist(member, "reconnect_"+suffix, "terminal"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked reconnect: %v", err)
	}
	if _, err = service.terminalSession(ctx, member, machine, second.ID); !errors.Is(err, ErrTerminalSessionNotFound) {
		t.Fatalf("revoked session visible: %v", err)
	}
	current, err = authority.Machine(ctx, owner, team, teams.MachineRequest{OperationID: "team-ownership", ExpectedGeneration: current.Generation, MachineID: machine, Action: "transfer_to_team", Confirmation: machine})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.RequestMaintenanceApproval(ctx, member, machine, "denied-update", "update", "2026.09.08.1", "test", time.Minute); !errors.Is(err, ErrNotFound) {
		t.Fatalf("use grant permits update administration: %v", err)
	}
	current, err = authority.Mutate(ctx, owner, team, teams.MutationRequest{OperationID: "admin", ExpectedGeneration: current.Generation, Action: "role", AccountID: member, Role: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	managed, err := store.Queries().GetUserMachineForUpdate(ctx, dbsqlc.GetUserMachineForUpdateParams{ID: machine, UserID: member})
	if err != nil {
		t.Fatal(err)
	}
	desired := MachineCapabilitySelection{Terminal: true, FileReceive: true}
	if _, err = service.SetMachineCapabilities(ctx, member, machine, "admin-capabilities", desired, managed.CapabilitiesDesiredVersion); err != nil {
		t.Fatal(err)
	}
	if _, err = service.SetAvailabilityPolicy(ctx, member, machine, "admin-availability", "allow_sleep", managed.AvailabilityDesiredVersion); err != nil {
		t.Fatal(err)
	}
	approval, err := service.RequestMaintenanceApproval(ctx, owner, machine, "shared-update", "update", "2026.09.08.1", "test", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.DecideMaintenanceApproval(ctx, member, machine, approval.ID, "approved"); err != nil {
		t.Fatal(err)
	}
	_, err = authority.Mutate(ctx, owner, team, teams.MutationRequest{OperationID: "remove-admin", ExpectedGeneration: current.Generation, Action: "remove", AccountID: member})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.SetMachineCapabilities(ctx, member, machine, "admin-capabilities", desired, managed.CapabilitiesDesiredVersion); !errors.Is(err, ErrNotFound) {
		t.Fatalf("removed administrator replayed capabilities: %v", err)
	}
	if _, err = service.SetAvailabilityPolicy(ctx, member, machine, "admin-availability", "allow_sleep", managed.AvailabilityDesiredVersion); !errors.Is(err, ErrNotFound) {
		t.Fatalf("removed administrator replayed availability: %v", err)
	}
	if _, err = store.Queries().ConsumeUserMachineMaintenanceApproval(ctx, dbsqlc.ConsumeUserMachineMaintenanceApprovalParams{ID: approval.ID, UserMachineID: machine}); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("removed administrator's pending authorization consumed: %v", err)
	}
}

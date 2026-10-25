package usermachines

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"github.com/pinksaucepasta/paperboat/server/internal/accessdescriptor"
	"github.com/pinksaucepasta/paperboat/server/internal/teams/teamstest"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/audit"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
	"github.com/pinksaucepasta/paperboat/server/internal/mint"
	"github.com/pinksaucepasta/paperboat/server/internal/workspace"
)

type workspaceMachineFixture struct {
	ctx     context.Context
	store   *db.DB
	service *Service
	account string
	machine string
	teamOne string
	teamTwo string
	suffix  string
}

func TestWorkspaceTerminalDescriptorDoesNotBorrowPersonalCapabilities(t *testing.T) {
	f, source, _ := newWorkspaceDescriptorFixture(t)
	team := workspace.WithContext(f.ctx, workspace.Scope{ID: f.teamOne, TeamID: f.teamOne})
	personal := workspace.WithContext(f.ctx, workspace.Scope{ID: "personal"})

	for _, test := range []struct {
		ctx       context.Context
		name      string
		wantFiles bool
	}{{team, "team-only", false}, {personal, "personal-full", true}} {
		session, err := f.service.CreateTerminalSession(test.ctx, f.account, f.machine, test.name, "descriptor_"+test.name+f.suffix)
		if err != nil {
			t.Fatal(err)
		}
		// No runtime operation executor is configured: remove the queued create
		// to exercise completed-session descriptor authorization directly.
		if _, err := f.store.SQL().ExecContext(f.ctx, `DELETE FROM paperboat.user_machine_terminal_session_operations WHERE terminal_session_id=$1`, session.ID); err != nil {
			t.Fatal(err)
		}
		descriptor, err := f.service.ConnectTerminalSession(test.ctx, f.account, source, f.machine, "cli_"+f.suffix, session.ID)
		if err != nil {
			t.Fatalf("%s descriptor: %v", test.name, err)
		}
		if !descriptor.Connectable || (descriptor.FileTransfer != nil) != test.wantFiles {
			t.Fatalf("%s descriptor readiness/files mismatch", test.name)
		}
		auth := descriptor.Terminal["auth"].(map[string]any)
		var capabilities string
		var teamID sql.NullString
		if err := f.store.SQL().QueryRowContext(f.ctx, `SELECT array_to_string(capabilities,','),team_id FROM paperboat.user_machine_access_sessions WHERE id=$1`, auth["access_session_id"]).Scan(&capabilities, &teamID); err != nil {
			t.Fatal(err)
		}
		if test.wantFiles {
			if capabilities != "terminal,files,private_access" || teamID.Valid {
				t.Fatalf("Personal capability/payer mismatch: %v %v", capabilities, teamID)
			}
		} else if capabilities != "terminal" || !teamID.Valid || teamID.String != f.teamOne {
			t.Fatalf("Team borrowed Personal capabilities/payer: %v %v", capabilities, teamID)
		}
	}
}

func newWorkspaceDescriptorFixture(t *testing.T) (workspaceMachineFixture, string, *mint.Provider) {
	t.Helper()
	f := newWorkspaceMachineFixture(t)
	source, env, edge := "ws_source_"+f.suffix, "env_"+f.suffix, "ws_edge_"+f.suffix
	publicKey := newIntegrationPublicIdentityKey(t)
	statements := []struct {
		query string
		args  []any
	}{
		{`UPDATE paperboat.user_machines SET configured_capabilities=ARRAY['terminal_host','file_receive','preview_launch'], observed_capabilities=ARRAY['terminal_host','file_receive','preview_launch'],public_identity_key=$2,worker_generation=1 WHERE id=$1`, []any{f.machine, publicKey}},
		{`INSERT INTO paperboat.user_machines(id,user_id,environment_id,platform,architecture,workspace_root,state,seat_state,online) VALUES($1,$2,$3,'linux','amd64','/workspace','online','occupied',true)`, []any{source, f.account, "env_source_" + f.suffix}},
		{`INSERT INTO paperboat.control_environments(id,workspace_id,owner_user_id,desired_state) VALUES($1,$2,$3,'active')`, []any{env, f.machine, f.account}},
		{`INSERT INTO paperboat.control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch,state,ready,last_heartbeat_at,roles,transports,registry_expires_at,region,failure_domain,capacity_limit,capacity_observed_at) VALUES($1,'development','1.0',$2,'ready',true,now(),ARRAY['edge'],ARRAY['http2','http3'],now()+interval '1 hour','test-region','test-domain',100,now())`, []any{edge, "epoch_" + f.suffix}},
		{`INSERT INTO paperboat.control_connector_generations(environment_id,machine_id,generation,edge_pool,edge_node_id,state) VALUES($1,$2,1,'development',$3,'admitted')`, []any{env, f.machine, edge}},
		{`INSERT INTO paperboat.control_routes(id,environment_id,kind,public_host,target_host,target_port,desired_revision,applied_revision,applied_node_id,applied_generation) VALUES($1,$2,'runtime_https_wss','workspace.invalid.test','127.0.0.1',8080,1,1,$3,1)`, []any{"route_" + f.suffix, env, edge}},
		{`INSERT INTO paperboat.runtime_carrier_attachments(route_id,machine_id,account_id,installation_generation,worker_generation,process_id,generation,edge_node_id,edge_process_epoch,public_identity_key,route_revision,session_id,preview_session_id,admission,expires_at,ready,observed_at,active) VALUES($1,$2,$3,1,1,$4,1,$5,$6,$7,1,$8,$9,'{}',now()+interval '1 hour',true,now(),true)`, []any{"route_" + f.suffix, f.machine, f.account, "process_" + f.suffix, edge, "epoch_" + f.suffix, publicKey, "session_" + uuid.NewString(), "session_" + uuid.NewString()}},
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		for _, q := range []string{`DELETE FROM paperboat.control_routes WHERE environment_id=$1`, `DELETE FROM paperboat.control_connector_generations WHERE environment_id=$1`, `DELETE FROM paperboat.control_environments WHERE id=$1`} {
			if _, err := f.store.SQL().ExecContext(ctx, q, env); err != nil {
				t.Errorf("descriptor cleanup: %v", err)
			}
		}
		if _, err := f.store.SQL().ExecContext(ctx, `DELETE FROM paperboat.control_tunnel_nodes WHERE id=$1`, edge); err != nil {
			t.Error(err)
		}
		if _, err := f.store.SQL().ExecContext(ctx, `DELETE FROM paperboat.user_machines WHERE id=$1`, source); err != nil {
			t.Error(err)
		}
	})
	teamstest.SetMachineAccess(t, f.store, f.teamOne, f.machine, "", "terminal", "exec")
	for _, statement := range statements {
		if _, err := f.store.SQL().ExecContext(f.ctx, statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	signer, err := mint.NewEphemeral(5 * time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	f.service.ConfigureAccess(fakeCredentialIssuer{}, "https://api.paperboat.test")
	f.service.ConfigureTerminalSessions(signer, nil)
	f.service.ConfigureFileTransfer(accessdescriptor.FileTransferPolicy{Revision: "file-transfer-v1", MaxBatchFiles: 10, AutomaticMaxBatchBytes: 2_000_000_000})
	f.service.ConfigureTransferModes("2026.10.27.0")
	return f, source, signer
}

func newWorkspaceMachineFixture(t *testing.T) workspaceMachineFixture {
	t.Helper()
	store := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	suffix := fmt.Sprint(time.Now().UnixNano())
	account, machine := "ws_owner_"+suffix, "ws_machine_"+suffix
	teamOne, teamTwo := "ws-one-"+suffix, "ws-two-"+suffix
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users(id,workos_subject,primary_email,status)
SELECT x,x,x||'@invalid.test','active' FROM unnest($1::text[]) x`, []string{account}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machines
(id,user_id,environment_id,platform,architecture,workspace_root,state,seat_state,online,configured_capabilities,observed_capabilities)
VALUES($1,$2,$3,'linux','amd64','/workspace','online','occupied',true,ARRAY['terminal_host','file_receive'],ARRAY['terminal_host','file_receive'])`, machine, account, "env_"+suffix); err != nil {
		t.Fatal(err)
	}
	for _, team := range []string{teamOne, teamTwo} {
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$2,1)`, team, account); err != nil {
			t.Fatal(err)
		}
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'member',true)`, team, account); err != nil {
			t.Fatal(err)
		}
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.team_resource_bindings(team_id,resource_kind,resource_id,owner_account,active,generation)
VALUES($1,'machine',$2,$3,true,1)`, team, machine, account); err != nil {
			t.Fatal(err)
		}
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.billing_workspaces(id,team_id,state,product_code,quantity,period_start,period_end)
VALUES($1,$2,'active','team-pro',2,$3,$4)`, "bw_"+team, team, time.Now().Add(-time.Hour), time.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	teamstest.SetMachineAccess(t, store, teamOne, machine, "", "terminal", "files")
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cleanupCancel()
		statements := []struct {
			query string
			args  []any
		}{
			{`DELETE FROM paperboat.user_machine_terminal_session_operations WHERE user_machine_id=$1`, []any{machine}},
			{`DELETE FROM paperboat.user_machine_access_sessions WHERE user_machine_id=$1`, []any{machine}},
			{`DELETE FROM paperboat.user_machine_terminal_sessions WHERE user_machine_id=$1`, []any{machine}},
			{`DELETE FROM paperboat.team_resource_bindings WHERE resource_kind='machine' AND resource_id=$1`, []any{machine}},
			{`DELETE FROM paperboat.billing_workspaces WHERE team_id=ANY($1::text[])`, []any{[]string{teamOne, teamTwo}}},
			{`DELETE FROM paperboat.team_members WHERE team_id=ANY($1::text[])`, []any{[]string{teamOne, teamTwo}}},
			{`DELETE FROM paperboat.teams WHERE team_id=ANY($1::text[])`, []any{[]string{teamOne, teamTwo}}},
			{`DELETE FROM paperboat.user_machines WHERE id=$1`, []any{machine}},
			{`DELETE FROM paperboat.users WHERE id=$1`, []any{account}},
		}
		for _, statement := range statements {
			if _, err := store.SQL().ExecContext(cleanupCtx, statement.query, statement.args...); err != nil {
				t.Errorf("workspace fixture cleanup: %v", err)
				return
			}
		}
	})
	return workspaceMachineFixture{
		ctx: ctx, store: store, service: New(store, audit.NewWriter(store), Policy{}, nil),
		account: account, machine: machine, teamOne: teamOne, teamTwo: teamTwo, suffix: suffix,
	}
}

func TestWorkspaceScopedMachineListGetAndAccess(t *testing.T) {
	f := newWorkspaceMachineFixture(t)
	personal := workspace.WithContext(f.ctx, workspace.Scope{ID: "personal"})
	teamOne := workspace.WithContext(f.ctx, workspace.Scope{ID: f.teamOne, TeamID: f.teamOne})
	teamTwo := workspace.WithContext(f.ctx, workspace.Scope{ID: f.teamTwo, TeamID: f.teamTwo})

	personalItems, personalCount, err := f.service.List(personal, f.account, 50, 0)
	if err != nil || personalCount != 1 || len(personalItems) != 1 || personalItems[0].ID != f.machine || !personalItems[0].CanManage {
		t.Fatalf("Personal list = (%d, %#v), %v", personalCount, personalItems, err)
	}
	teamItems, teamCount, err := f.service.List(teamOne, f.account, 50, 0)
	if err != nil || teamCount != 1 || len(teamItems) != 1 || teamItems[0].ID != f.machine || teamItems[0].CanManage {
		t.Fatalf("team one list = (%d, %#v), %v", teamCount, teamItems, err)
	}
	if _, err := f.service.Get(teamTwo, f.account, f.machine); !errors.Is(err, ErrNotFound) {
		t.Fatalf("team two fell back to another team's capability: %v", err)
	}
	if _, count, err := f.service.List(teamTwo, f.account, 50, 0); err != nil || count != 0 {
		t.Fatalf("team two list = %d, %v; want empty", count, err)
	}

	for _, entry := range []struct {
		ctx   context.Context
		owner string
		total int
	}{{personal, "mine", 1}, {teamOne, "mine", 1}, {teamOne, "shared", 0}, {teamTwo, "", 0}} {
		items, total, _, err := f.service.ListInventory(entry.ctx, f.account, InventoryOptions{Limit: 1, Owner: entry.owner})
		if err != nil || total != entry.total || len(items) != entry.total {
			t.Fatalf("filtered machine page owner=%s: %d %d %v", entry.owner, len(items), total, err)
		}
	}
	if items, total, _, err := f.service.ListInventory(personal, f.account, InventoryOptions{Limit: 1, Offset: 1}); err != nil || total != 1 || len(items) != 0 {
		t.Fatalf("machine offset page: %d %d %v", len(items), total, err)
	}

	access := func(ctx context.Context, id string) error {
		return f.service.createAccessSession(ctx, dbsqlc.CreateUserMachineAccessSessionParams{
			ID: id, UserMachineID: f.machine, UserID: f.account, EnvironmentID: "env_" + f.suffix,
			CLIClientSessionID: "cli_" + id, HttpBaseUrl: "https://invalid.test", HelperTerminalSessionID: "terminal_" + id,
			ExpiresAt: time.Now().Add(time.Minute), Capabilities: []string{"terminal"}, OperationID: "operation_" + id,
		}, machineCredentialExternal)
	}
	if err := access(teamOne, "ws_access_team_one_"+f.suffix); err != nil {
		t.Fatalf("team one access denied: %v", err)
	}
	var paidTeam sql.NullString
	if err := f.store.SQL().QueryRowContext(f.ctx, `SELECT team_id FROM paperboat.user_machine_access_sessions WHERE id=$1`, "ws_access_team_one_"+f.suffix).Scan(&paidTeam); err != nil || !paidTeam.Valid || paidTeam.String != f.teamOne {
		t.Fatalf("team access payer = %v, %v; want %q", paidTeam, err, f.teamOne)
	}
	if err := access(personal, "ws_access_personal_"+f.suffix); err != nil {
		t.Fatalf("Personal access denied: %v", err)
	}
	if err := f.store.SQL().QueryRowContext(f.ctx, `SELECT team_id FROM paperboat.user_machine_access_sessions WHERE id=$1`, "ws_access_personal_"+f.suffix).Scan(&paidTeam); err != nil || paidTeam.Valid {
		t.Fatalf("Personal access assigned a team payer: %v, %v", paidTeam, err)
	}
	if err := access(teamTwo, "ws_access_wrong_team_"+f.suffix); !errors.Is(err, ErrNotFound) {
		t.Fatalf("team two used team one's access grant: %v", err)
	}
	if _, err := f.store.SQL().ExecContext(f.ctx, `UPDATE paperboat.team_members SET active=false WHERE team_id=$1 AND account_id=$2`, f.teamOne, f.account); err != nil {
		t.Fatal(err)
	}
	if err := access(teamOne, "ws_access_revoked_team_"+f.suffix); !errors.Is(err, workspace.ErrForbidden) {
		t.Fatalf("revoked workspace fell back to Personal: %v", err)
	}

	// A shared personal host remains personally owned on each real inventory
	// page; team selection must never turn ownership into a sharing label.
	second := "ws_paged_" + f.suffix
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if _, err := f.store.SQL().ExecContext(ctx, `DELETE FROM paperboat.user_machines WHERE id=$1`, second); err != nil {
			t.Errorf("paged fixture cleanup: %v", err)
		}
	})
	if _, err := f.store.SQL().ExecContext(f.ctx, `UPDATE paperboat.user_machines SET alias='alpha' WHERE id=$1`, f.machine); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.SQL().ExecContext(f.ctx, `INSERT INTO paperboat.user_machines
 (id,user_id,environment_id,alias,platform,architecture,workspace_root)
 VALUES($1,$2,$1,'beta','linux','amd64','/workspace')`, second, f.account); err != nil {
		t.Fatal(err)
	}
	for offset, id := range []string{f.machine, second} {
		items, total, _, err := f.service.ListInventory(personal, f.account, InventoryOptions{Limit: 1, Offset: offset})
		if err != nil || total != 2 || len(items) != 1 || items[0].ID != id {
			t.Fatalf("personal page %d: incorrect authorized inventory", offset)
		}
		if items[0].Ownership != "personal" || items[0].OwnerAccount != f.account || items[0].OwnerTeamID != "" || !items[0].CanManage {
			t.Fatalf("personal page %d: incorrect owner/management authority", offset)
		}
	}
	if items, total, _, err := f.service.ListInventory(personal, f.account, InventoryOptions{Limit: 1, Offset: 2}); err != nil || total != 2 || len(items) != 0 {
		t.Fatal("exhausted personal page lost total or repeated an item")
	}
}

func TestWorkspaceScopedTerminalOwnership(t *testing.T) {
	f := newWorkspaceMachineFixture(t)
	teamOne := workspace.WithContext(f.ctx, workspace.Scope{ID: f.teamOne, TeamID: f.teamOne})
	teamTwo := workspace.WithContext(f.ctx, workspace.Scope{ID: f.teamTwo, TeamID: f.teamTwo})
	personal := workspace.WithContext(f.ctx, workspace.Scope{ID: "personal"})

	created, err := f.service.CreateTerminalSession(teamOne, f.account, f.machine, "team-debug", "ws_terminal_"+f.suffix)
	if err != nil {
		t.Fatalf("create team terminal session: %v", err)
	}
	var ownerTeam string
	if err := f.store.SQL().QueryRowContext(f.ctx, `SELECT owner_team_id FROM paperboat.user_machine_terminal_sessions WHERE id=$1`, created.ID).Scan(&ownerTeam); err != nil || ownerTeam != f.teamOne {
		t.Fatalf("persisted terminal workspace = %q, %v; want %q", ownerTeam, err, f.teamOne)
	}
	teamSessions, err := f.service.ListTerminalSessions(teamOne, f.account, f.machine)
	if err != nil || len(teamSessions) != 1 || teamSessions[0].ID != created.ID {
		t.Fatalf("team one terminal list = %#v, %v", teamSessions, err)
	}
	if _, err := f.service.ListTerminalSessions(teamTwo, f.account, f.machine); !errors.Is(err, ErrNotFound) {
		t.Fatalf("team two saw terminal hosted in team one: %v", err)
	}
	personalSessions, err := f.service.ListTerminalSessions(personal, f.account, f.machine)
	if err != nil || len(personalSessions) != 0 {
		t.Fatalf("Personal terminal list included team session: %#v, %v", personalSessions, err)
	}
	if _, err := f.service.terminalSession(teamTwo, f.account, f.machine, created.ID); !errors.Is(err, ErrTerminalSessionNotFound) {
		t.Fatalf("team two attached to team one session: %v", err)
	}
	if _, err := f.service.terminalSession(f.ctx, f.account, f.machine, created.ID); err != nil {
		t.Fatalf("resource-bound unselected lifecycle lost its target: %v", err)
	}
	boundContext, host, err := f.service.transferSessionHost(f.ctx, f.account, created.ID)
	if err != nil || host.ID != f.machine {
		t.Fatalf("unselected transfer did not resolve the exact session host: host=%q error=%v", host.ID, err)
	}
	if scope, ok := workspace.FromContext(boundContext); !ok || scope.ID != f.teamOne {
		t.Fatalf("unselected transfer scope = %+v, selected=%v; want persisted Team One", scope, ok)
	}
	if _, _, err := f.service.transferSessionHost(teamTwo, f.account, created.ID); !errors.Is(err, ErrTerminalSessionNotFound) {
		t.Fatalf("transfer lookup escaped Team One through Team Two: %v", err)
	}
	if _, _, err := f.service.transferSessionHost(personal, f.account, created.ID); !errors.Is(err, ErrTerminalSessionNotFound) {
		t.Fatalf("transfer lookup escaped Team One through Personal: %v", err)
	}
	if _, err := f.service.SetTerminalSessionTransferDestination(teamTwo, f.account, created.ID, f.machine); !errors.Is(err, ErrTransferDestinationInvalid) {
		t.Fatalf("Team Two selected a destination for Team One session: %v", err)
	}
	if _, err := f.service.SetTerminalSessionTransferDestination(personal, f.account, created.ID, f.machine); !errors.Is(err, ErrTransferDestinationInvalid) {
		t.Fatalf("Personal selected a destination for Team One session: %v", err)
	}
	if selected, err := f.service.SetTerminalSessionTransferDestination(f.ctx, f.account, created.ID, f.machine); err != nil || selected.ID != f.machine {
		t.Fatalf("resource-bound transfer destination = %+v, %v", selected, err)
	}
	if selected, err := f.service.TerminalSessionTransferDestination(f.ctx, f.account, created.ID); err != nil || selected.ID != f.machine {
		t.Fatalf("resource-bound transfer destination read = %+v, %v", selected, err)
	}
	if _, err := f.service.TerminalSessionTransferDestination(teamTwo, f.account, created.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Team Two read Team One transfer destination: %v", err)
	}
	if err := f.service.ClearTerminalSessionTransferDestination(teamTwo, f.account, created.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Team Two cleared Team One transfer destination: %v", err)
	}
	if err := f.service.ClearTerminalSessionTransferDestination(f.ctx, f.account, created.ID); err != nil {
		t.Fatalf("resource-bound clear transfer destination: %v", err)
	}
}

func TestFilesOnlyTeamInventoryRequiresPaidEntitlement(t *testing.T) {
	f := newWorkspaceMachineFixture(t)
	member := "files_member_" + f.suffix
	t.Cleanup(func() {
		_, _ = f.store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.team_members WHERE account_id=$1`, member)
		_, _ = f.store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.users WHERE id=$1`, member)
	})
	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, []any{member}},
		{`INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'member',true)`, []any{f.teamOne, member}},
	} {
		if _, err := f.store.SQL().ExecContext(f.ctx, statement.sql, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	teamstest.SetMachineAccess(t, f.store, f.teamOne, f.machine, "")
	teamstest.SetMachineAccess(t, f.store, f.teamOne, f.machine, member, "files")
	team := workspace.WithContext(f.ctx, workspace.Scope{ID: f.teamOne, TeamID: f.teamOne})
	check := func(want int) {
		t.Helper()
		items, total, _, err := f.service.ListInventory(team, member, InventoryOptions{Limit: 50})
		if err != nil || total != want || len(items) != want {
			t.Fatalf("files-only inventory count=%d total=%d want=%d: %v", len(items), total, want, err)
		}
		if want == 1 {
			if items[0].ID != f.machine || items[0].CanManage || len(items[0].Permissions) != 1 || items[0].Permissions[0] != "files" {
				t.Fatal("files-only member borrowed owner or terminal authority")
			}
			if _, err := f.service.Get(team, member, f.machine); err != nil {
				t.Fatalf("files-only detail denied: %v", err)
			}
		}
	}
	check(1)
	if _, err := getMachineForCapability(team, f.store.Queries(), dbsqlc.GetUserMachineForCapabilityParams{ID: f.machine, UserID: member, Capability: "terminal"}); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("files-only grant authorized terminal: %v", err)
	}
	if _, total, _, err := f.service.ListInventory(workspace.WithContext(f.ctx, workspace.Scope{ID: "personal"}), member, InventoryOptions{Limit: 50}); err != nil || total != 0 {
		t.Fatal("team grant leaked into personal inventory")
	}
	if _, err := f.store.SQL().ExecContext(f.ctx, `UPDATE paperboat.billing_workspaces SET period_end=now()-interval '1 second' WHERE team_id=$1`, f.teamOne); err != nil {
		t.Fatal(err)
	}
	check(0)
	if _, err := f.store.SQL().ExecContext(f.ctx, `UPDATE paperboat.billing_workspaces SET period_end=now()+interval '1 hour' WHERE team_id=$1`, f.teamOne); err != nil {
		t.Fatal(err)
	}
	check(1)
	teamstest.SetMachineAccess(t, f.store, f.teamOne, f.machine, member)
	check(0)
}

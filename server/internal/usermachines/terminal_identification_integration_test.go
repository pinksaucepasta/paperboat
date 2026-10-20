package usermachines

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pinksaucepasta/paperboat-server/internal/audit"
	"github.com/pinksaucepasta/paperboat-server/internal/helperruntime"
	"github.com/pinksaucepasta/paperboat-server/internal/mint"
	"github.com/pinksaucepasta/paperboat-server/internal/teams"
	"github.com/pinksaucepasta/paperboat-server/internal/workspace"
)

// This signed runtime fixture isolates the real database authorization and
// projection boundary from transport behavior covered by the terminal tests.
type terminalIdentificationRuntime struct {
	helperruntime.Client
	signer                                        *mint.Provider
	machine, environment, account, session, route string
	calls                                         atomic.Int64
	unavailable                                   bool
	slow                                          bool
	multiple                                      bool
}

func (r *terminalIdentificationRuntime) Terminal(ctx context.Context, route, credential, operation, session, operationID string) (helperruntime.Snapshot, error) {
	r.calls.Add(1)
	claims, err := r.signer.VerifyCredential(credential, "https://api.paperboat.test", "terminal_operation", time.Now().UTC())
	if err != nil || operation != "snapshot" || (!r.multiple && session != r.session) || route != r.route || operationID == "" || claims.MachineID != r.machine || claims.EnvironmentID != r.environment || claims.UserID != r.account || claims.SessionID != session {
		return helperruntime.Snapshot{}, errors.New("snapshot fixture received incorrect resource binding")
	}
	if r.slow {
		<-ctx.Done()
		return helperruntime.Snapshot{}, ctx.Err()
	}
	if r.unavailable {
		return helperruntime.Snapshot{}, errors.New("fixture host unavailable")
	}
	return helperruntime.Snapshot{ID: session, Name: "runtime-launch-name", CWD: "/launch", State: "running", Generation: 1, Title: "  vim\x1b\u202e main.go ", CurrentDirectory: "/live\x00/\u2066api", ForegroundProcess: "vi\t\u200fm"}, nil
}

func (r *terminalIdentificationRuntime) TerminalViaEdge(ctx context.Context, _, route, credential, operation, session, operationID string) (helperruntime.Snapshot, error) {
	return r.Terminal(ctx, "https://"+route, credential, operation, session, operationID)
}

func TestPostgresTerminalLiveIdentificationAuthorization(t *testing.T) {
	store := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	suffix := fmt.Sprint(time.Now().UnixNano())
	owner, stranger, machine, environment := "ti_owner_"+suffix, "ti_stranger_"+suffix, "ti_machine_"+suffix, "ti_environment_"+suffix
	session, edge, epoch, route := "terminal_"+uuid.NewString(), "ti_edge_"+suffix, "ti_epoch_"+suffix, "ti_route_"+suffix
	identity := newIntegrationPublicIdentityKey(t)
	insert := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	insert(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) SELECT x,x,x||'@invalid.test','active' FROM unnest($1::text[]) x`, []string{owner, stranger})
	insert(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,online,public_identity_key,worker_generation,configured_capabilities,observed_capabilities) VALUES($1,$2,$3,'identification-host','linux','amd64','/launch','online','occupied',true,$4,1,ARRAY['terminal_host'],ARRAY['terminal_host'])`, machine, owner, environment, identity)
	insert(`INSERT INTO paperboat.control_environments(id,workspace_id,owner_user_id,desired_state) VALUES($1,$2,$3,'active')`, environment, machine, owner)
	insert(`INSERT INTO paperboat.control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch,state,ready,last_heartbeat_at,roles,transports,registry_expires_at,region,failure_domain,capacity_limit,capacity_observed_at) VALUES($1,'development','1.0',$2,'ready',true,now(),ARRAY['edge'],ARRAY['http2','http3'],now()+interval '1 hour','test-region','test-domain',100,now())`, edge, epoch)
	insert(`INSERT INTO paperboat.control_connector_generations(environment_id,machine_id,generation,edge_pool,edge_node_id,state) VALUES($1,$2,1,'development',$3,'admitted')`, environment, machine, edge)
	host := "identification-" + suffix + ".example.test"
	insert(`INSERT INTO paperboat.control_routes(id,environment_id,kind,public_host,target_host,target_port,desired_revision,applied_revision,applied_node_id,applied_generation) VALUES($1,$2,'runtime_https_wss',$3,'127.0.0.1',8080,1,1,$4,1)`, route, environment, host, edge)
	insert(`INSERT INTO paperboat.runtime_carrier_attachments(route_id,machine_id,account_id,installation_generation,worker_generation,process_id,generation,edge_node_id,edge_process_epoch,public_identity_key,route_revision,session_id,preview_session_id,admission,expires_at,ready,observed_at,active) VALUES($1,$2,$3,1,1,$4,1,$5,$6,$7,1,$8,$9,'{}',now()+interval '1 hour',true,now(),true)`, route, machine, owner, "ti_process_"+suffix, edge, epoch, identity, "session_"+uuid.NewString(), "session_"+uuid.NewString())
	insert(`INSERT INTO paperboat.user_machine_terminal_sessions(id,user_machine_id,owner_account,name,launch_cwd,title,runtime_state) VALUES($1,$2,$3,'my-work','/launch','stale manual title','running')`, session, machine, owner)
	var carrierReady bool
	if err := store.SQL().QueryRowContext(ctx, `SELECT paperboat.runtime_carrier_current($1,true)`, route).Scan(&carrierReady); err != nil || !carrierReady {
		t.Fatalf("fixture carrier readiness=%t error=%v", carrierReady, err)
	}
	signer, err := mint.NewEphemeral(5 * time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	service := New(store, audit.NewWriter(store), Policy{}, nil)
	service.issuer = "https://api.paperboat.test"
	service.ConfigureTerminalSessions(4, signer, nil)
	runtime := &terminalIdentificationRuntime{signer: signer, machine: machine, environment: environment, account: owner, session: session, route: "https://" + host}
	service.controlRuntime = runtime
	// A ready carrier without verified public ingress is temporarily unavailable,
	// not a missing or closed terminal. Authorization still precedes that check.
	if _, err = service.ResolveTerminalBrowserRole(ctx, stranger, session); !errors.Is(err, ErrTerminalSessionNotFound) {
		t.Fatalf("unverified edge disclosed terminal to stranger: %v", err)
	}
	if _, err = service.ResolveTerminalBrowserRole(ctx, owner, session); !errors.Is(err, ErrMachineCapabilityUnavailable) {
		t.Fatalf("unverified edge must preserve terminal availability classification: %v", err)
	}
	insert(`UPDATE paperboat.control_tunnel_nodes SET public_ingress_verified_at=now(),endpoint_host='edge.example.test' WHERE id=$1`, edge)
	decision, err := service.ResolveTerminalBrowserRole(ctx, owner, session)
	if err != nil || decision.EdgeNodeID != edge || decision.EdgeProcessEpoch != epoch || decision.EdgeEndpointHost != "edge.example.test" || decision.HostWSSURL != "wss://"+host+"/v1/browser-terminal" {
		t.Fatalf("verified browser route unavailable: %+v error=%v", decision, err)
	}
	if _, err = service.TerminalSessionSharing(ctx, stranger, session); !errors.Is(err, ErrTerminalSessionNotFound) {
		t.Fatalf("unauthorized sharing metadata: %v", err)
	}
	if _, err = service.TerminalBrowserSession(ctx, stranger, "", session); !errors.Is(err, ErrTerminalSessionNotFound) {
		t.Fatalf("unauthorized browser metadata: %v", err)
	}
	if runtime.calls.Load() != 0 {
		t.Fatal("unauthorized metadata request probed runtime")
	}
	item, err := service.TerminalSessionSharing(ctx, owner, session)
	if err != nil || runtime.calls.Load() != 1 || item.Name != "my-work" || item.Title != "vim main.go" || item.CurrentDirectory != "/live/api" || item.ForegroundProcess != "vim" || item.StartedIn != "/launch" || item.State != "detached" {
		t.Fatalf("live authorized projection=%+v calls=%d error=%v", item, runtime.calls.Load(), err)
	}
	items, err := service.ListTerminalSessions(ctx, owner, machine)
	if err != nil || len(items) != 1 || items[0].Title != "vim main.go" || items[0].CurrentDirectory != "/live/api" || items[0].ForegroundProcess != "vim" || items[0].Name != "my-work" || items[0].StartedIn != "/launch" {
		t.Fatalf("machine live projection=%+v error=%v", items, err)
	}

	// Sharing changes Team access, not the Personal terminal's launch workspace.
	team := "ti-team-" + suffix
	insert(`INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$2,1)`, team, owner)
	insert(`INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'admin',true),($1,$3,1,'member',true)`, team, owner, stranger)
	insert(`INSERT INTO paperboat.billing_workspaces(id,team_id,state,product_code,quantity,period_start,period_end) VALUES($1,$2,'active','team-pro',2,now()-interval '1 hour',now()+interval '1 hour')`, "bw_"+team, team)
	personal := workspace.WithContext(ctx, workspace.Scope{ID: "personal"})
	otherID := "ti-other-" + suffix
	insert(`INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$2,1)`, otherID, owner)
	insert(`INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'admin',true)`, otherID, owner)
	otherTeam := workspace.WithContext(ctx, workspace.Scope{ID: otherID, TeamID: otherID})
	var teamGeneration uint64
	if err = store.SQL().QueryRowContext(ctx, `SELECT generation FROM paperboat.teams WHERE team_id=$1`, team).Scan(&teamGeneration); err != nil {
		t.Fatal(err)
	}
	grant := teams.TerminalSessionGrantRequest{OperationID: "share_" + suffix, ExpectedGeneration: teamGeneration, Audience: "all_members", Role: "viewer", Active: true}
	if _, err = service.GrantTerminalSessionSharing(otherTeam, owner, team, session, grant); !errors.Is(err, ErrTerminalSessionNotFound) {
		t.Fatalf("other workspace could share Personal terminal: %v", err)
	}
	shared, err := service.GrantTerminalSessionSharing(personal, owner, team, session, grant)
	if err != nil || shared.Sharing.TeamID != team {
		t.Fatalf("Personal terminal sharing failed: %+v error=%v", shared.Sharing, err)
	}
	var keptPersonal bool
	if err = store.SQL().QueryRowContext(ctx, `SELECT owner_team_id IS NULL FROM paperboat.user_machine_terminal_sessions WHERE id=$1`, session).Scan(&keptPersonal); err != nil || !keptPersonal {
		t.Fatalf("sharing changed launch workspace: %v", err)
	}
	shared, err = service.RemoveTerminalParticipant(personal, owner, session, stranger, "exclude_"+suffix, shared.Sharing.Generation)
	if err != nil {
		t.Fatalf("Personal sharing participant removal: %v", err)
	}
	if _, err = service.EndTerminalSessionSharing(otherTeam, owner, session, "wrong-end_"+suffix, shared.Sharing.Generation); !errors.Is(err, ErrTerminalSessionNotFound) {
		t.Fatalf("other workspace could end sharing: %v", err)
	}
	shared, err = service.EndTerminalSessionSharing(personal, owner, session, "end_"+suffix, shared.Sharing.Generation)
	if err != nil || shared.Sharing.TeamID != "" {
		t.Fatalf("Personal end sharing: %+v error=%v", shared.Sharing, err)
	}

	// A grant to one member must show up in the owner's view, not only an all-members grant.
	selected := teams.TerminalSessionGrantRequest{OperationID: "sel_" + suffix, ExpectedGeneration: 0, Audience: "selected_member", AccountID: stranger, Role: "viewer", Active: true}
	if err = store.SQL().QueryRowContext(ctx, `SELECT generation FROM paperboat.teams WHERE team_id=$1`, team).Scan(&selected.ExpectedGeneration); err != nil {
		t.Fatal(err)
	}
	shared, err = service.GrantTerminalSessionSharing(personal, owner, team, session, selected)
	if err != nil || shared.Sharing.TeamID != team || len(shared.Sharing.Grants) != 1 || shared.Sharing.Grants[0].AccountID != stranger || !shared.Sharing.Grants[0].Active {
		t.Fatalf("selected-member grant not visible: %+v error=%v", shared.Sharing, err)
	}
	if _, err = service.EndTerminalSessionSharing(personal, owner, session, "end2_"+suffix, shared.Sharing.Generation); err != nil {
		t.Fatalf("ending the selected-member share: %v", err)
	}

	// An accepted close can precede the host's last running snapshot. Neither
	// exact detail nor global paging may turn it back into an attachable session.
	insert(`UPDATE user_machine_terminal_sessions SET desired_state='closed',runtime_state='closing' WHERE id=$1`, session)
	closedShare := grant
	closedShare.OperationID = "closed_" + suffix
	if err = store.SQL().QueryRowContext(ctx, `SELECT generation FROM paperboat.teams WHERE team_id=$1`, team).Scan(&closedShare.ExpectedGeneration); err != nil {
		t.Fatal(err)
	}
	if _, err = service.GrantTerminalSessionSharing(personal, owner, team, session, closedShare); !errors.Is(err, ErrTerminalSessionEnded) {
		t.Fatalf("a closing session was shared: %v", err)
	}
	item, err = service.TerminalSessionSharing(ctx, owner, session)
	if err != nil || item.State != "closing" || item.DesiredState != "closed" || item.RuntimeState != "running" {
		t.Fatalf("close intent lost during live enrichment: %+v err%v", item, err)
	}
	closed := true
	page, total, err := service.TerminalSessionInventory(ctx, owner, InventoryOptions{Limit: 20, Closed: &closed})
	if err != nil || total != 1 || len(page) != 1 || page[0].State != "closing" {
		t.Fatalf("closing page %+v total%d err%v", page, total, err)
	}
	page, total, err = service.TerminalSessionInventory(ctx, owner, InventoryOptions{Limit: 20, State: "running"})
	if err != nil || total != 0 || len(page) != 0 {
		t.Fatalf("close request listed as running %+v total%d err%v", page, total, err)
	}
	beforeCalls := runtime.calls.Load()
	if _, err = service.TerminalBrowserSession(ctx, owner, "", session); !errors.Is(err, ErrTerminalBrowserSessionClosed) {
		t.Fatalf("owner close error %v", err)
	}
	if _, err = service.TerminalBrowserSession(ctx, stranger, "", session); !errors.Is(err, ErrTerminalSessionNotFound) {
		t.Fatalf("closed session disclosed to stranger %v", err)
	}
	if runtime.calls.Load() != beforeCalls {
		t.Fatal("closed attachment probed runtime")
	}
	insert(`UPDATE user_machine_terminal_sessions SET desired_state='open',runtime_state='running' WHERE id=$1`, session)
	runtime.unavailable = true
	item, err = service.TerminalSessionSharing(ctx, owner, session)
	if err != nil || item.Title != "" || item.CurrentDirectory != "" || item.ForegroundProcess != "" || item.StartedIn != "/launch" || item.State != "unavailable" {
		t.Fatalf("unavailable metadata guessed or stale=%+v error=%v", item, err)
	}
	// A slow real signed runtime cannot hold either authorized discovery page.
	runtime.unavailable = false
	runtime.multiple = true
	runtime.slow = true
	for i := 0; i < 8; i++ {
		insert(`INSERT INTO user_machine_terminal_sessions(id,user_machine_id,owner_account,name,launch_cwd,runtime_state) VALUES($1,$2,$3,$4,'/launch','running')`, fmt.Sprintf("slow_%s_%d", suffix, i), machine, owner, fmt.Sprintf("slow%d", i))
	}
	for _, global := range []bool{false, true} {
		pageCtx, stop := context.WithTimeout(ctx, 500*time.Millisecond)
		start := time.Now()
		beforeCalls := runtime.calls.Load()
		if global {
			page, total, e := service.TerminalSessionInventory(pageCtx, owner, InventoryOptions{Limit: 20})
			if e != nil || total != 9 || len(page) != 9 {
				t.Fatalf("slow global page: %+v total%d err%v", page, total, e)
			}
			for _, item := range page {
				if item.State != "unavailable" || item.Title != "" || item.ParticipantsAvailable {
					t.Fatalf("slow snapshot fabricated live state: %+v", item)
				}
			}
		} else {
			page, total, e := service.ListTerminalSessionInventory(pageCtx, owner, machine, InventoryOptions{Limit: 20})
			if e != nil || total != 9 || len(page) != 9 {
				t.Fatalf("slow machine page: %+v total%d err%v", page, total, e)
			}
			for _, item := range page {
				if item.State != "unavailable" || item.Title != "" || item.AttachedCount != nil {
					t.Fatalf("slow machine fabricated live state: %+v", item)
				}
			}
		}
		stop()
		if probes := runtime.calls.Load() - beforeCalls; probes < 2 || probes > 4 {
			t.Fatalf("slow runtime probes=%d, wanted bounded concurrent page", probes)
		}
		if time.Since(start) > time.Second {
			t.Fatal("slow page exceeded bounded caller deadline")
		}
	}

}

package usermachines

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pinksaucepasta/paperboat-server/internal/audit"
	"github.com/pinksaucepasta/paperboat-server/internal/helperruntime"
	"github.com/pinksaucepasta/paperboat-server/internal/mint"
)

// hostSessionUnknown is the host's answer for a session it does not have.
type hostSessionUnknown struct{}

func (hostSessionUnknown) Error() string                  { return "not_found_or_forbidden" }
func (hostSessionUnknown) DiagnosticProtocolCode() string { return "not_found_or_forbidden" }

// hostWithSessions answers snapshots for the sessions the host has and "unknown" for every other one.
type hostWithSessions struct {
	helperruntime.Client
	has         map[string]bool
	unreachable bool
	calls       int
}

func (h *hostWithSessions) Terminal(_ context.Context, _, _, operation, session, _ string) (helperruntime.Snapshot, error) {
	h.calls++
	if h.unreachable {
		return helperruntime.Snapshot{}, errors.New("fixture host unavailable")
	}
	if !h.has[session] {
		return helperruntime.Snapshot{}, fmt.Errorf("terminal %s: %w", operation, hostSessionUnknown{})
	}
	return helperruntime.Snapshot{ID: session, State: "running", Generation: 1}, nil
}

// Sessions the host no longer has (aborted runs, a reinstalled host) must not keep counting toward the limit:
// creating a session at the limit asks the host first and closes only what the host says it does not have.
func TestPostgresTerminalSessionLimitIgnoresSessionsTheHostNoLongerHas(t *testing.T) {
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
	service.ConfigureTerminalSessions(3, signer, nil)
	insert(`UPDATE paperboat.control_tunnel_nodes SET public_ingress_verified_at=now(),endpoint_host='edge.example.test' WHERE id=$1`, edge)
	insert(`DELETE FROM paperboat.user_machine_terminal_sessions WHERE id=$1`, session)
	live, orphan, orphanClosing := "terminal_"+uuid.NewString(), "terminal_"+uuid.NewString(), "terminal_"+uuid.NewString()
	for _, row := range []struct{ id, name, desired, state string }{{live, "live-one", "open", "running"}, {orphan, "orphan-open", "open", "running"}, {orphanClosing, "orphan-closing", "closed", "running"}} {
		insert(`INSERT INTO paperboat.user_machine_terminal_sessions(id,user_machine_id,owner_account,name,launch_cwd,desired_state,runtime_state) VALUES($1,$2,$3,$4,'/launch',$5,$6)`, row.id, machine, owner, row.name, row.desired, row.state)
	}
	hostRuntime := &hostWithSessions{has: map[string]bool{live: true}}
	service.controlRuntime = hostRuntime
	state := func(id string) string {
		var runtimeState string
		if err := store.SQL().QueryRowContext(ctx, `SELECT runtime_state FROM paperboat.user_machine_terminal_sessions WHERE id=$1`, id).Scan(&runtimeState); err != nil {
			t.Fatal(err)
		}
		return runtimeState
	}
	// An unreachable host closes nothing, so the limit still refuses.
	hostRuntime.unreachable = true
	if _, err := service.CreateTerminalSession(ctx, owner, machine, "blocked", "key-blocked-"+suffix, 3); !errors.Is(err, ErrTerminalSessionLimit) {
		t.Fatalf("limit with an unreachable host: %v", err)
	}
	if state(orphan) != "running" || state(orphanClosing) != "running" {
		t.Fatal("an unreachable host closed sessions")
	}
	hostRuntime.unreachable = false
	created, err := service.CreateTerminalSession(ctx, owner, machine, "fresh", "key-fresh-"+suffix, 3)
	if err != nil || created.Name != "fresh" {
		t.Fatalf("create after the host reported its sessions: %+v %v", created, err)
	}
	if state(orphan) != "closed" || state(orphanClosing) != "closed" || state(live) != "running" {
		t.Fatalf("states orphan=%s closing=%s live=%s", state(orphan), state(orphanClosing), state(live))
	}
}

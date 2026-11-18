package usermachines

import (
	"context"
	"errors"
	"fmt"
	"github.com/pinksaucepasta/paperboat/shared/terminalprotocol"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pinksaucepasta/paperboat/server/internal/audit"
	"github.com/pinksaucepasta/paperboat/server/internal/mint"
)

// A CLI descriptor records no edge usage authority (a direct connection must keep working and costs nothing), whatever the
// workspace's bandwidth state.
func TestCLIDescriptorsRecordNoEdgeAuthority(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	userID, machineID, environmentID := "usr_rq_"+suffix, "um_rq_"+suffix, "env_rq_"+suffix
	sourceMachineID, helperID, edgeNodeID := "um_rq_src_"+suffix, "helper_rq_"+suffix, "edge_rq_"+suffix
	publicIdentityKey := newIntegrationPublicIdentityKey(t)
	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO paperboat.users (id,workos_subject,primary_email,status) VALUES ($1,$2,$3,'active')`, []any{userID, "workos_rq_" + suffix, "rq-" + suffix + "@example.test"}},
		{`INSERT INTO paperboat.user_machines (id,user_id,environment_id,platform,architecture,workspace_root,state,seat_state,online,public_identity_key,worker_generation) VALUES ($1,$2,$3,'linux','amd64','/workspace','online','occupied',true,$4,1)`, []any{machineID, userID, environmentID, publicIdentityKey}},
		{`UPDATE paperboat.user_machines SET observed_capabilities=configured_capabilities WHERE id=$1`, []any{machineID}},
		{`INSERT INTO paperboat.user_machines (id,user_id,environment_id,platform,architecture,workspace_root,state,seat_state,online) VALUES ($1,$2,$3,'linux','amd64','/source','online','occupied',true)`, []any{sourceMachineID, userID, "env_rq_src_" + suffix}},
		{`INSERT INTO paperboat.control_environments (id,workspace_id,owner_user_id,desired_state) VALUES ($1,$2,$3,'active')`, []any{environmentID, machineID, userID}},
		{`INSERT INTO paperboat.control_helpers (id,environment_id,state) VALUES ($1,$2,'active')`, []any{helperID, environmentID}},
		{`INSERT INTO paperboat.control_tunnel_nodes (id,edge_pool,protocol_version,process_epoch,state,ready,last_heartbeat_at,roles,transports,registry_expires_at,region,failure_domain,capacity_limit,capacity_observed_at) VALUES ($1,'development','1.0',$2,'ready',true,now(),ARRAY['edge'],ARRAY['http2','http3'],now()+interval '1 hour','test-region','test-domain',100,now())`, []any{edgeNodeID, "epoch_rq_" + suffix}},
		{`INSERT INTO paperboat.control_connector_generations (environment_id,machine_id,generation,edge_pool,edge_node_id,state) VALUES ($1,$2,1,'development',$3,'admitted')`, []any{environmentID, machineID, edgeNodeID}},
		{`INSERT INTO paperboat.control_routes (id,environment_id,kind,public_host,target_host,target_port,desired_revision,applied_revision,applied_node_id,applied_generation) VALUES ($1,$2,'runtime_https_wss',$3,'127.0.0.1',8080,1,1,$4,1)`, []any{"route_rq_" + suffix, environmentID, "rq-" + suffix + ".example.test", edgeNodeID}},
		{`INSERT INTO paperboat.runtime_carrier_attachments
(route_id,machine_id,account_id,installation_generation,worker_generation,process_id,generation,edge_node_id,edge_process_epoch,public_identity_key,route_revision,session_id,preview_session_id,admission,expires_at,ready,observed_at,active)
VALUES ($1,$2,$3,1,1,$4,1,$5,$6,$7,1,$8,$9,'{}',now()+interval '1 hour',true,now(),true)`, []any{"route_rq_" + suffix, machineID, userID, "process_rq_" + suffix, edgeNodeID, "epoch_rq_" + suffix, publicIdentityKey, "session_" + uuid.NewString(), "session_" + uuid.NewString()}},
	}
	for _, statement := range statements {
		if _, err := store.SQL().ExecContext(ctx, statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	var carrierReady bool
	if err := store.SQL().QueryRowContext(ctx, `SELECT paperboat.runtime_carrier_current($1,true)`, "route_rq_"+suffix).Scan(&carrierReady); err != nil || !carrierReady {
		t.Fatalf("exec fixture runtime carrier readiness=%v err=%v", carrierReady, err)
	}
	signer, err := mint.NewEphemeral(5 * time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	service := New(store, audit.NewWriter(store), Policy{}, nil)
	service.ConfigureAccess(fakeCredentialIssuer{}, "https://api.paperboat.test")
	service.ConfigureTerminalSessions(signer, nil)
	descriptor, err := service.ExecDescriptor(ctx, userID, sourceMachineID, machineID, "cli_rq_1", "operation_rq_cap_"+suffix, false)
	if err != nil {
		t.Fatalf("exec descriptor at the cap: %v", err)
	}
	accessSessionID := descriptor.Auth.AccessSessionID
	var cliAuthorities int
	if err := store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.usage_accounting_authorities WHERE authority_id=$1`, accessSessionID).Scan(&cliAuthorities); err != nil || cliAuthorities != 0 {
		t.Fatalf("CLI descriptor recorded an edge authority: %d err=%v", cliAuthorities, err)
	}

	// Lane R N3: after an outage the edge carrier re-attaches up to ~30 s after presence returns, and
	// native exec does not need it. Without a current edge route, a client that accepts native-only gets
	// the descriptor with no edge endpoints and an access session without an edge base URL; a client that
	// does not still gets machine_not_ready.
	if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.runtime_carrier_attachments SET ready=false,observed_at=now()-interval '5 minutes' WHERE route_id=$1`, "route_rq_"+suffix); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ExecDescriptor(ctx, userID, sourceMachineID, machineID, "cli_rq_1", "operation_rq_noroute_"+suffix, false); !errors.Is(err, ErrMachineCapabilityUnavailable) {
		t.Fatalf("descriptor without an edge route for a client that needs it: %v", err)
	}
	native, err := service.ExecDescriptor(ctx, userID, sourceMachineID, machineID, "cli_rq_1", "operation_rq_native_"+suffix, true)
	if err != nil || native.Endpoints != (terminalprotocol.TerminalEndpoints{}) || native.Auth.Token == "" {
		t.Fatalf("native-only descriptor = %+v err=%v", native, err)
	}
	var base string
	if err := store.SQL().QueryRowContext(ctx, `SELECT http_base_url FROM paperboat.user_machine_access_sessions WHERE id=$1`, native.Auth.AccessSessionID).Scan(&base); err != nil || base != "" {
		t.Fatalf("native-only access session base=%q err=%v", base, err)
	}
}

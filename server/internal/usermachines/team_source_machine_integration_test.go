package usermachines

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pinksaucepasta/paperboat/server/internal/audit"
	"github.com/pinksaucepasta/paperboat/server/internal/mint"
)

// A team member running pb on a team-owned machine gets descriptors from it: the source check accepts
// the user's own personal machine or a machine of a team the user is an active member of, never a
// stranger's, a suspended team's or a revoked machine (Lane O, F12: exec from a team machine was 404).
func TestTeamOwnedSourceMachineGetsDescriptors(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	userID, machineID, environmentID := "usr_ts_"+suffix, "um_ts_"+suffix, "env_ts_"+suffix
	sourceMachineID, helperID, edgeNodeID := "um_ts_src_"+suffix, "helper_ts_"+suffix, "edge_ts_"+suffix
	publicIdentityKey := newIntegrationPublicIdentityKey(t)
	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO paperboat.users (id,workos_subject,primary_email,status) VALUES ($1,$2,$3,'active')`, []any{userID, "workos_ts_" + suffix, "ts-" + suffix + "@example.test"}},
		{`INSERT INTO paperboat.user_machines (id,user_id,environment_id,platform,architecture,workspace_root,state,seat_state,online,public_identity_key,worker_generation) VALUES ($1,$2,$3,'linux','amd64','/workspace','online','occupied',true,$4,1)`, []any{machineID, userID, environmentID, publicIdentityKey}},
		{`UPDATE paperboat.user_machines SET observed_capabilities=configured_capabilities WHERE id=$1`, []any{machineID}},
		{`INSERT INTO paperboat.user_machines (id,user_id,environment_id,platform,architecture,workspace_root,state,seat_state,online) VALUES ($1,$2,$3,'linux','amd64','/source','online','occupied',true)`, []any{sourceMachineID, userID, "env_ts_src_" + suffix}},
		{`INSERT INTO paperboat.control_environments (id,workspace_id,owner_user_id,desired_state) VALUES ($1,$2,$3,'active')`, []any{environmentID, machineID, userID}},
		{`INSERT INTO paperboat.control_helpers (id,environment_id,state) VALUES ($1,$2,'active')`, []any{helperID, environmentID}},
		{`INSERT INTO paperboat.control_tunnel_nodes (id,edge_pool,protocol_version,process_epoch,state,ready,last_heartbeat_at,roles,transports,registry_expires_at,region,failure_domain,capacity_limit,capacity_observed_at) VALUES ($1,'development','1.0',$2,'ready',true,now(),ARRAY['edge'],ARRAY['http2','http3'],now()+interval '1 hour','test-region','test-domain',100,now())`, []any{edgeNodeID, "epoch_ts_" + suffix}},
		{`INSERT INTO paperboat.control_connector_generations (environment_id,machine_id,generation,edge_pool,edge_node_id,state) VALUES ($1,$2,1,'development',$3,'admitted')`, []any{environmentID, machineID, edgeNodeID}},
		{`INSERT INTO paperboat.control_routes (id,environment_id,kind,public_host,target_host,target_port,desired_revision,applied_revision,applied_node_id,applied_generation) VALUES ($1,$2,'runtime_https_wss',$3,'127.0.0.1',8080,1,1,$4,1)`, []any{"route_ts_" + suffix, environmentID, "ts-" + suffix + ".example.test", edgeNodeID}},
		{`INSERT INTO paperboat.runtime_carrier_attachments
(route_id,machine_id,account_id,installation_generation,worker_generation,process_id,generation,edge_node_id,edge_process_epoch,public_identity_key,route_revision,session_id,preview_session_id,admission,expires_at,ready,observed_at,active)
VALUES ($1,$2,$3,1,1,$4,1,$5,$6,$7,1,$8,$9,'{}',now()+interval '1 hour',true,now(),true)`, []any{"route_ts_" + suffix, machineID, userID, "process_ts_" + suffix, edgeNodeID, "epoch_ts_" + suffix, publicIdentityKey, "session_" + uuid.NewString(), "session_" + uuid.NewString()}},
	}
	for _, statement := range statements {
		if _, err := store.SQL().ExecContext(ctx, statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	var carrierReady bool
	if err := store.SQL().QueryRowContext(ctx, `SELECT paperboat.runtime_carrier_current($1,true)`, "route_ts_"+suffix).Scan(&carrierReady); err != nil || !carrierReady {
		t.Fatalf("exec fixture runtime carrier readiness=%v err=%v", carrierReady, err)
	}
	signer, err := mint.NewEphemeral(5 * time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	service := New(store, audit.NewWriter(store), Policy{}, nil)
	service.ConfigureAccess(fakeCredentialIssuer{}, "https://api.paperboat.test")
	service.ConfigureTerminalSessions(signer, nil)
	quota := &runtimeQuotaFixture{exhausted: true}
	service.ConfigureRuntimeQuota(quota, "https://console.example.test/billing")
	// At the cap a CLI descriptor is still issued: the server cannot know whether the
	// connection will be direct, and direct machine-to-machine traffic is always free.
	team := "team_ts_" + suffix
	for _, statement := range []string{
		`INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$2,1)`,
		`INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'member',true)`,
	} {
		if _, err := store.SQL().ExecContext(ctx, statement, team, userID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.user_machines SET owner_team_id=$2 WHERE id=$1`, sourceMachineID, team); err != nil {
		t.Fatal(err)
	}
	// End to end: exec and managed SSH descriptors from the team-owned source machine.
	if _, err := service.ExecDescriptor(ctx, userID, sourceMachineID, machineID, "cli_ts_1", "operation_ts_exec_"+suffix); err != nil {
		t.Fatalf("exec descriptor from a team-owned source: %v", err)
	}
	if _, err := service.SSHDescriptor(ctx, userID, sourceMachineID, machineID, "cli_ts_1", "operation_ts_ssh_"+suffix); err != nil && !errors.Is(err, ErrMachineCapabilityUnavailable) {
		t.Fatalf("ssh descriptor from a team-owned source: %v", err)
	}
	stranger := "usr_ts_other_" + suffix
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users (id,workos_subject,primary_email,status) VALUES ($1,$2,$3,'active')`, stranger, "workos_ts_other_"+suffix, "ts-other-"+suffix+"@example.test"); err != nil {
		t.Fatal(err)
	}
	allowed := func(user, machine string) bool {
		t.Helper()
		ok, err := service.sourceMachineAllowed(ctx, user, machine)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if !allowed(userID, sourceMachineID) || !allowed(userID, machineID) {
		t.Fatal("member's team machine or own personal machine refused as source")
	}
	if allowed(stranger, sourceMachineID) || allowed(stranger, machineID) {
		t.Fatal("a non-member may act from another account's machine")
	}
	if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.teams SET suspended=true,suspended_at=now(),suspended_by=$2 WHERE team_id=$1`, team, userID); err != nil {
		t.Fatal(err)
	}
	if allowed(userID, sourceMachineID) {
		t.Fatal("a suspended team's machine accepted as source")
	}
	if _, err := service.ExecDescriptor(ctx, userID, sourceMachineID, machineID, "cli_ts_1", "operation_ts_suspended_"+suffix); !errors.Is(err, ErrNotFound) {
		t.Fatalf("exec from a suspended team's machine = %v, want not found", err)
	}
	if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.user_machines SET revoked_at=now(),state='revoked' WHERE id=$1`, machineID); err != nil {
		t.Fatal(err)
	}
	if allowed(userID, machineID) {
		t.Fatal("a revoked machine accepted as source")
	}
}

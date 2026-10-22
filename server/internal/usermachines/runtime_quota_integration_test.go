package usermachines

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pinksaucepasta/paperboat-server/internal/audit"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/mint"
)

type runtimeQuotaFixture struct {
	exhausted bool
	checked   []string
}

func (f *runtimeQuotaFixture) CheckWorkspaceBandwidthTx(_ context.Context, _ *db.Tx, workspace string, _ time.Time) (bool, error) {
	f.checked = append(f.checked, workspace)
	return f.exhausted, nil
}

// Exec through the edge's runtime ingress is Paperboat bandwidth like the relay: below the cap the
// session is recorded for the payer so edge usage reports debit it; at the cap the descriptor is
// refused with the typed error and the console Billing page, and no access session is created.
func TestEdgeRuntimeSessionsAreAccountedAndRefusedAtTheBandwidthCap(t *testing.T) {
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
		{`INSERT INTO paperboat.user_machines (id,user_id,environment_id,description,platform,architecture,workspace_root,state,seat_state,online,public_identity_key,worker_generation) VALUES ($1,$2,$3,'Exec Host','linux','amd64','/workspace','online','occupied',true,$4,1)`, []any{machineID, userID, environmentID, publicIdentityKey}},
		{`UPDATE paperboat.user_machines SET observed_capabilities=configured_capabilities WHERE id=$1`, []any{machineID}},
		{`INSERT INTO paperboat.user_machines (id,user_id,environment_id,description,platform,architecture,workspace_root,state,seat_state,online) VALUES ($1,$2,$3,'Exec Source','linux','amd64','/source','online','occupied',true)`, []any{sourceMachineID, userID, "env_rq_src_" + suffix}},
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
	service.ConfigureTerminalSessions(4, signer, nil)
	quota := &runtimeQuotaFixture{}
	service.ConfigureRuntimeQuota(quota, "https://console.example.test/billing")
	descriptor, err := service.ExecDescriptor(ctx, userID, sourceMachineID, machineID, "cli_rq_1", "operation_rq_ok_"+suffix)
	if err != nil {
		t.Fatal(err)
	}
	accessSessionID, _ := descriptor.Auth["access_session_id"].(string)
	var workspace, route, actor string
	var revision int64
	if err := store.SQL().QueryRowContext(ctx, `SELECT workspace_id,route_id,route_revision,actor_account_id FROM paperboat.usage_accounting_authorities WHERE authority_id=$1 AND edge_node_id=$2 AND environment_id=$3`, accessSessionID, edgeNodeID, environmentID).Scan(&workspace, &route, &revision, &actor); err != nil || workspace != "personal:"+userID || route != "route_rq_"+suffix || revision != 1 || actor != userID {
		t.Fatalf("runtime session accounting workspace=%q route=%q revision=%d actor=%q err=%v", workspace, route, revision, actor, err)
	}
	if len(quota.checked) != 1 || quota.checked[0] != "personal:"+userID {
		t.Fatalf("admission checked %v", quota.checked)
	}
	quota.exhausted = true
	_, err = service.ExecDescriptor(ctx, userID, sourceMachineID, machineID, "cli_rq_1", "operation_rq_cap_"+suffix)
	var refusal *BandwidthQuotaError
	if !errors.Is(err, ErrBandwidthQuotaExhausted) || !errors.As(err, &refusal) || refusal.BillingURL != "https://console.example.test/billing" {
		t.Fatalf("at the cap err=%v", err)
	}
	var sessions int
	if err := store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.user_machine_access_sessions WHERE user_machine_id=$1`, machineID).Scan(&sessions); err != nil || sessions != 1 {
		t.Fatalf("refused descriptor created an access session: sessions=%d err=%v", sessions, err)
	}
	_, err = service.SSHDescriptor(ctx, userID, sourceMachineID, machineID, "cli_rq_1", "operation_rq_ssh_"+suffix)
	if !errors.Is(err, ErrBandwidthQuotaExhausted) && !errors.Is(err, ErrMachineCapabilityUnavailable) {
		t.Fatalf("ssh at the cap err=%v", err)
	}
}

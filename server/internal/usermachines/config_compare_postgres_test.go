package usermachines_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"github.com/google/uuid"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/controlplane"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
	"github.com/pinksaucepasta/paperboat/server/internal/mint"
	"github.com/pinksaucepasta/paperboat/server/internal/usermachines"
	"github.com/pinksaucepasta/paperboat/server/internal/workspace"
	"os"
	"strings"
	"testing"
	"time"
)

func TestNativeConfigCompareDescriptorAndMeterPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN for isolated PostgreSQL")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err = db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	account, other, environment, machine, repository, assignment, client, node, route := "cmp_user_"+suffix, "cmp_other_"+suffix, "cmp_env_"+suffix, "cmp_machine_"+suffix, "cmp_repo_"+suffix, "cmp_assignment_"+suffix, "cmp_client_"+suffix, "cmp_node_"+suffix, "cmp_route_"+suffix
	execute := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	defer func() {
		clean, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		for _, query := range []string{`DELETE FROM native_usage_authorities WHERE machine_id=$1`, `DELETE FROM config_compare_admissions WHERE machine_id=$1`, `DELETE FROM control_routes WHERE environment_id=$2`, `DELETE FROM control_connector_generations WHERE environment_id=$2`, `DELETE FROM user_machines WHERE id=$1`, `DELETE FROM control_environments WHERE id=$2`, `DELETE FROM control_tunnel_nodes WHERE id=$3`, `DELETE FROM control_config_repositories WHERE owner_user_id=$4`, `DELETE FROM cli_client_sessions WHERE user_id IN($4,$5)`, `DELETE FROM users WHERE id IN($4,$5)`} {
			if _, err := store.SQL().ExecContext(clean, query+` AND $1::text<>'' AND $2::text<>'' AND $3::text<>'' AND $4::text<>'' AND $5::text<>''`, machine, environment, node, account, other); err != nil {
				t.Errorf("cleanup: %v", err)
			}
		}
	}()
	for _, id := range []string{account, other} {
		execute(`INSERT INTO users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@compare.test','active')`, id)
	}
	execute(`INSERT INTO cli_client_sessions(id,user_id,client_id,client_label,device_type,os,scopes,state,created_at,approved_at) VALUES($1,$2,$1,'Compare','desktop','linux',ARRAY['machines:connect'],'active',now(),now())`, client, account)
	execute(`INSERT INTO control_environments(id,workspace_id,owner_user_id,desired_state) VALUES($1,$1,$2,'active')`, environment, account)
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	public := base64.RawURLEncoding.EncodeToString(key.Public().(ed25519.PublicKey))
	execute(`INSERT INTO user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,online,installation_generation,worker_generation,public_identity_key) VALUES($1,$2,$3,'compare','linux','amd64','/workspace','online','occupied',true,1,1,$4)`, machine, account, environment, public)
	execute(`INSERT INTO control_config_repositories(id,owner_user_id,provider,external_ref,display_name,state) VALUES($1,$2,'github',$1,'owner/config','active')`, repository, account)
	execute(`INSERT INTO control_config_assignments(id,machine_id,environment_id,repository_id,consent_state,warning_revision,accepted_at,configuration_revision) VALUES($1,$2,$3,$4,'accepted','warning',now(),repeat('a',64))`, assignment, machine, environment, repository)
	execute(`INSERT INTO control_config_sync_statuses(environment_id,repository_id,assignment_id,machine_id,installation_generation,warning_revision,policy_revision,sync_revision,state,remote_revision,conflicts,machine_updated_at,observed_at) VALUES($1,$2,$3,$4,1,'warning','policy',1,'conflict','remote',jsonb_build_array(jsonb_build_object('path','editor/settings','revision',repeat('a',64))),now(),now())`, environment, repository, assignment, machine)
	execute(`INSERT INTO control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch,endpoint_host,endpoint_tcp_port,endpoint_quic_port,state,ready,last_heartbeat_at,roles,transports,registry_expires_at,region,failure_domain,capacity_limit,capacity_observed_at,public_ingress_verified_at) VALUES($1,'development','1.0','compare-epoch','edge.compare.test',443,8443,'ready',true,now(),ARRAY['edge'],ARRAY['http2','http3'],now()+interval '1 hour','test','test',100,now(),now())`, node)
	execute(`INSERT INTO control_connector_generations(environment_id,machine_id,generation,edge_pool,edge_node_id,state) VALUES($1,$2,1,'development',$3,'admitted')`, environment, machine, node)
	execute(`INSERT INTO control_routes(id,environment_id,kind,public_host,target_host,target_port,desired_revision,applied_revision,applied_node_id,applied_generation) VALUES($1,$2,'runtime_https_wss','machine.compare.test','127.0.0.1',8080,1,1,$3,1)`, route, environment, node)
	execute(`INSERT INTO runtime_carrier_attachments(route_id,machine_id,account_id,installation_generation,worker_generation,process_id,generation,edge_node_id,edge_process_epoch,public_identity_key,route_revision,session_id,preview_session_id,admission,expires_at,ready,observed_at,active) VALUES($1,$2,$3,1,1,$4,1,$5,'compare-epoch',$6,1,$7,$8,'{}',now()+interval '1 hour',true,now(),true)`, route, machine, account, "process_"+suffix, node, public, "session_"+uuid.NewString(), "session_"+uuid.NewString())
	var carrierReady bool
	if err := store.SQL().QueryRowContext(ctx, `SELECT paperboat.runtime_carrier_current($1,true)`, route).Scan(&carrierReady); err != nil || !carrierReady {
		t.Fatalf("fixture carrier ready=%t: %v", carrierReady, err)
	}
	signer, err := mint.New([]mint.Key{{ID: "compare-key", PrivateKey: key}}, "compare-key", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	conflicts := controlplane.NewConfigConflictService(store, nil, nil)
	scoped := workspace.WithContext(ctx, workspace.Scope{ID: "personal"})
	binding := mint.ConfigCompareBinding{AssignmentID: assignment, AssignmentVersion: 1, ConfigPath: "editor/settings", ConflictRevision: strings.Repeat("a", 64), ExpectedRemoteRevision: "remote"}
	if id, err := conflicts.AuthorizeCompare(scoped, account, environment, binding); err != nil || id != assignment {
		t.Fatalf("fixture conflict authority %s: %v", id, err)
	}
	if _, err = conflicts.AuthorizeCompare(scoped, other, environment, binding); err == nil {
		t.Fatal("foreign comparison authorized")
	}
	changed := binding
	changed.ConfigPath = "editor/private"
	if _, err = conflicts.AuthorizeCompare(scoped, account, environment, changed); err == nil {
		t.Fatal("unreported path authorized")
	}
	changed = binding
	changed.AssignmentVersion = 2
	if _, err = conflicts.AuthorizeCompare(scoped, account, environment, changed); err == nil {
		t.Fatal("stale assignment authorized")
	}
	machines := usermachines.New(store, nil, usermachines.Policy{}, nil)
	machines.ConfigureAccess(nil, "https://api.compare.test")
	machines.ConfigureTerminalSessions(signer, nil)
	native, err := machines.ConfigCompareDescriptor(scoped, account, environment, machine, client, binding)
	if err != nil {
		t.Fatal(err)
	}
	nativeClaims, err := signer.VerifyCredential(native.Auth["token"].(string), "https://api.compare.test", "config_compare", time.Now())
	if err != nil || nativeClaims.SessionID != "" || nativeClaims.AssignmentID != assignment {
		t.Fatalf("native signed grant: %v", err)
	}
	var kind, id, caps string
	if err = store.SQL().QueryRowContext(ctx, `SELECT resource_kind,resource_id,array_to_string(capabilities,',') FROM native_usage_authorities WHERE access_session_id=$1`, nativeClaims.JTI).Scan(&kind, &id, &caps); err != nil || kind != "config" || id != assignment || caps != "config_compare" {
		t.Fatalf("native meter=%s/%s/%s err=%v", kind, id, caps, err)
	}
	var terminals int
	if err := store.SQL().QueryRowContext(ctx, "SELECT count(*) FROM user_machine_terminal_sessions WHERE user_machine_id=$1", machine).Scan(&terminals); err != nil {
		t.Fatal(err)
	}
	if terminals != 0 {
		t.Fatal("comparison created terminal")
	}
	// A live assignment keeps its id when its config changes, so the machine's sync state and an open
	// comparison stay attached to it; a revoked one starts a new lifecycle with a new id.
	var version int64
	var mode string
	var rules, bindings []byte
	if err := store.SQL().QueryRowContext(ctx, `SELECT version,mode,path_rules,repository_bindings FROM control_config_assignments WHERE id=$1`, assignment).Scan(&version, &mode, &rules, &bindings); err != nil {
		t.Fatal(err)
	}
	set := func(id string, revision string, expected int64) dbsqlc.ControlConfigAssignment {
		t.Helper()
		row, err := store.Queries().SetControlConfigAssignment(ctx, dbsqlc.SetControlConfigAssignmentParams{AssignmentID: id, EnvironmentID: environment, RepositoryID: sql.NullString{String: repository, Valid: true}, Mode: mode, ConsentState: "pending", WarningRevision: sql.NullString{String: "warning", Valid: true}, PathRules: rules, RepositoryBindings: bindings, ConfigurationRevision: revision, Now: time.Now(), ExpectedVersion: expected})
		if err != nil {
			t.Fatalf("assigning config: %v", err)
		}
		return row
	}
	updated := set("cmp_candidate_"+suffix, strings.Repeat("b", 64), version)
	if updated.ID != assignment || updated.Version != version+1 {
		t.Fatalf("a config change replaced the assignment id: %s (want %s) version=%d", updated.ID, assignment, updated.Version)
	}
	var kept int
	if err := store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM config_compare_admissions WHERE machine_id=$1 AND assignment_id=$2`, machine, assignment).Scan(&kept); err != nil || kept == 0 {
		t.Fatalf("the open comparison lost its assignment: %d %v", kept, err)
	}
	execute(`UPDATE control_config_assignments SET revoked_at=now(), consent_state='revoked' WHERE id=$1`, assignment)
	if fresh := set("cmp_fresh_"+suffix, strings.Repeat("c", 64), updated.Version); fresh.ID != "cmp_fresh_"+suffix {
		t.Fatalf("a revoked assignment must start a new lifecycle: %s", fresh.ID)
	}
}

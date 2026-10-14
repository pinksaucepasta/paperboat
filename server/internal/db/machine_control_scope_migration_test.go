package db

import (
	"context"
	"encoding/base64"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pressly/goose/v3"
)

func TestMachineControlScopeMigrationPreservesIdentity(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_MIGRATION_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("empty isolated migration PostgreSQL DSN required")
	}
	if err := ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	var exists bool
	if err = store.SQL().QueryRowContext(ctx, `SELECT to_regclass('paperboat.users') IS NOT NULL`).Scan(&exists); err != nil || exists {
		t.Fatalf("requires empty isolated database: exists=%v error=%v", exists, err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`CREATE SCHEMA paperboat`)
	goose.SetBaseFS(migrationsFS)
	goose.SetTableName("paperboat.goose_db_version")
	if err = goose.SetDialect("postgres"); err != nil {
		t.Fatal(err)
	}
	if err = goose.UpToContext(ctx, store.SQL(), migrationsDir, 42); err != nil {
		t.Fatal(err)
	}
	user, machine, scope, helper, route, node, session := "user_"+uuid.NewString(), "machine_"+uuid.NewString(), "environment_"+uuid.NewString(), "runtime_"+uuid.NewString(), "route_"+uuid.NewString(), "node_"+uuid.NewString(), "session_"+uuid.NewString()
	publicKey := base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("k", 32)))
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$2,'active')`, user, user+"@example.test")
	exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,public_identity_key,installation_generation,worker_generation) VALUES($1,$2,$3,'scope-migration','linux','amd64','/workspace','online','occupied',$4,7,9)`, machine, user, scope, publicKey)
	exec(`SELECT paperboat.reserve_machine_hostname($1,8080,'scope-host.example.test')`, machine)
	exec(`INSERT INTO paperboat.control_environments(id,workspace_id,owner_user_id,desired_state,desired_version) VALUES($1,$2,$3,'active',4)`, scope, machine, user)
	exec(`INSERT INTO paperboat.control_helpers(id,environment_id,state,generation,public_key,key_thumbprint) VALUES($1,$2,'active',3,decode(repeat('6b',32),'hex'),'scope-key')`, helper, scope)
	exec(`INSERT INTO paperboat.control_connector_generations(environment_id,machine_id,generation,edge_pool,state) VALUES($1,$2,5,'default','pending')`, scope, machine)
	exec(`INSERT INTO paperboat.control_routes(id,environment_id,kind,public_host,target_host,target_port,desired_revision) VALUES($1,$2,'runtime_https_wss','scope.example.test','127.0.0.1',8080,6)`, route, scope)
	exec(`INSERT INTO paperboat.control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch) VALUES($1,'default','1.0','')`, node)
	exec(`INSERT INTO paperboat.control_usage_counters(edge_node_id,counter_epoch,environment_id,route_id,route_revision,direction,bytes) VALUES($1,'epoch',$2,$3,6,'ingress',123)`, node, scope, route)
	exec(`INSERT INTO paperboat.environment_scopes(id,account_id,scope,machine_id,scope_state,version,key_epoch,authority_generation,authority_id,manifest_id) VALUES('scope-manifest',$1,'machine',$2,'active',2,3,4,'sha256:'||repeat('a',64),'sha256:'||repeat('b',64))`, user, machine)
	exec(`INSERT INTO paperboat.control_helper_enrollments(id,environment_id,helper_id,jti_hash,operation_key,request_hash,grant_ciphertext,state,expires_at) VALUES('scope-grant',$1,$2,decode(repeat('11',32),'hex'),'scope-operation',decode(repeat('22',32),'hex'),'opaque-grant','pending',now()+interval '10 minutes')`, scope, helper)
	exec(`INSERT INTO paperboat.user_machine_access_sessions(id,user_machine_id,user_id,environment_id,cli_client_session_id,http_base_url,helper_terminal_session_id,credential_kind,expires_at) VALUES($1,$2,$3,$4,'scope-client','https://scope.example.test','scope-token','control_plane_signed',now()+interval '10 minutes')`, session, machine, user, scope)
	if err = goose.UpToContext(ctx, store.SQL(), migrationsDir, 43); err == nil || !strings.Contains(err.Error(), "drained runtime carriers and access sessions") {
		t.Fatalf("active admission cutover guard: %v", err)
	}
	var unchanged bool
	if err = store.SQL().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM paperboat.user_machines WHERE id=$1 AND environment_id=$2) AND EXISTS(SELECT 1 FROM paperboat.control_helper_enrollments WHERE id='scope-grant')`, machine, scope).Scan(&unchanged); err != nil || !unchanged {
		t.Fatalf("failed cutover changed live identity: %v %v", unchanged, err)
	}
	exec(`UPDATE paperboat.user_machine_access_sessions SET state='revoked',revoked_at=now() WHERE id=$1`, session)
	exec(`CREATE TEMP TABLE scope_expected_machine AS SELECT to_jsonb(m)-'environment_id' AS value FROM paperboat.user_machines m WHERE id=$1`, machine)
	exec(`CREATE TEMP TABLE scope_expected_helper AS SELECT to_jsonb(h)-'environment_id' AS value FROM paperboat.control_helpers h WHERE id=$1`, helper)
	exec(`CREATE TEMP TABLE scope_expected_vault AS SELECT to_jsonb(s) AS value FROM paperboat.environment_scopes s`)
	exec(`CREATE TEMP TABLE scope_expected_hostname AS SELECT to_jsonb(h) AS value FROM paperboat.managed_hostnames h WHERE machine_id=$1 AND port=8080`, machine)
	exec(`CREATE TEMP TABLE scope_expected_constraints AS SELECT conname,condeferrable,condeferred FROM pg_constraint WHERE connamespace='paperboat'::regnamespace AND contype='f'`)
	if err = goose.UpToContext(ctx, store.SQL(), migrationsDir, 43); err != nil {
		t.Fatal(err)
	}
	var intact bool
	if err = store.SQL().QueryRowContext(ctx, `SELECT
 (SELECT environment_id=id AND to_jsonb(m)-'environment_id'=(SELECT value FROM scope_expected_machine) FROM paperboat.user_machines m WHERE id=$1)
 AND (SELECT id=workspace_id AND owner_user_id=$2 AND desired_version=4 FROM paperboat.control_environments WHERE id=$1)
 AND (SELECT environment_id=$1 AND to_jsonb(h)-'environment_id'=(SELECT value FROM scope_expected_helper) FROM paperboat.control_helpers h WHERE id=$3)
 AND (SELECT environment_id=$1 AND machine_id=$1 AND generation=5 FROM paperboat.control_connector_generations WHERE environment_id=$1)
 AND (SELECT environment_id=$1 AND desired_revision=6 FROM paperboat.control_routes WHERE id=$4)
 AND (SELECT environment_id=$1 AND bytes=123 FROM paperboat.control_usage_counters WHERE route_id=$4)
 AND (SELECT to_jsonb(s)=(SELECT value FROM scope_expected_vault) FROM paperboat.environment_scopes s)
 AND (SELECT hostname='scope-host.example.test' AND resource_kind='machine' AND resource_id=$1 AND machine_id=$1 AND port=8080 AND to_jsonb(h)=(SELECT value FROM scope_expected_hostname) FROM paperboat.managed_hostnames h WHERE machine_id=$1 AND port=8080)
 AND NOT EXISTS(SELECT 1 FROM paperboat.control_helper_enrollments WHERE id='scope-grant')
 AND (SELECT bool_and(c.condeferrable=e.condeferrable AND c.condeferred=e.condeferred) FROM pg_constraint c JOIN scope_expected_constraints e USING(conname) WHERE c.connamespace='paperboat'::regnamespace)
 AND to_regclass('pg_temp.machine_control_scope_rekeys') IS NULL`, machine, user, helper, route).Scan(&intact); err != nil || !intact {
		t.Fatalf("scope cutover lost machine/key/generation/vault/hostname/FK integrity: %v %v", intact, err)
	}
}

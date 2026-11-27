package browseringress

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/browseraccess"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/testutil"
)

// F1 follow-up: a draining edge keeps its private/team browser preview until the drain deadline.
func TestPreviewDrainingEdgeKeepsServingUntilDeadlinePostgres(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("PAPERBOAT_TEST_DATABASE_DSN"))
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if os.Getenv("PAPERBOAT_TEST_SCHEMA_READY") != "1" {
		if err = db.Migrate(ctx, store); err != nil {
			t.Fatal(err)
		}
	}
	suffix := fmt.Sprint(time.Now().UnixNano())
	owner, lease, operation := "usr_bi_"+suffix, "prv_bi_"+suffix, "op_bi_"+suffix
	machine, node, epoch := "mch_bi_"+suffix, "edge-bi-"+suffix, "epoch-bi-"+suffix
	now := time.Now().UTC().Truncate(time.Microsecond)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, e := store.SQL().ExecContext(ctx, q, args...); e != nil {
			t.Fatal(e)
		}
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = store.SQL().ExecContext(c, `DELETE FROM paperboat.preview_leases WHERE id=$1`, lease)
		_, _ = store.SQL().ExecContext(c, `DELETE FROM paperboat.operations WHERE id=$1`, operation)
		_, _ = store.SQL().ExecContext(c, `DELETE FROM paperboat.control_tunnel_nodes WHERE id=$1`, node)
		_, _ = store.SQL().ExecContext(c, `DELETE FROM paperboat.control_environments WHERE id IN (SELECT environment_id FROM paperboat.user_machines WHERE id=$1)`, machine)
		_, _ = store.SQL().ExecContext(c, `DELETE FROM paperboat.user_machines WHERE id=$1`, machine)
		_, _ = store.SQL().ExecContext(c, `DELETE FROM paperboat.users WHERE id=$1`, owner)
	})
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@test.invalid','active')`, owner)
	key := sha256.Sum256([]byte("browseringress-machine-key:" + suffix))
	publicKey := base64.RawURLEncoding.EncodeToString(key[:])
	exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,platform,architecture,workspace_root,state,seat_state,online,public_identity_key,installation_generation,worker_generation,worker_service_scope,connector_state) VALUES($1,$2,$3,'linux','amd64','/workspace','online','occupied',true,$4,1,1,'system','ready')`, machine, owner, "env_bi_"+suffix, publicKey)
	exec(`INSERT INTO paperboat.control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch,endpoint_host,endpoint_tcp_port,endpoint_quic_port,carrier_endpoint_host,carrier_endpoint_tcp_port,carrier_endpoint_quic_port,carrier_server_spki_sha256,carrier_server_certificate_chain_pem,state,ready,capacity,last_heartbeat_at) VALUES($1,'test','1.0',$2,'edge.example.test',24001,24002,'edge.example.test',25001,25002,'sha256:'||repeat('b',64),'test-public-certificate-chain','ready',true,'{}',$3)`, node, epoch, now)
	exec(`INSERT INTO paperboat.preview_leases(owner_session_kind,id,endpoint,account_id,actor_id,owner_machine_id,owner_session_id,target_scheme,target_address,access_mode,lease_deadline,allocation_state,edge_state,origin_state,terminal_state,created_at,last_renewed_at) VALUES('foreground',$1,$2,$3,$3,$4,$5,'http','127.0.0.1:3000','team',$6,'ready','ready','ready','active',$7,$7)`, lease, "https://bi-"+suffix+".preview.example.test", owner, machine, "owner-session-bi-"+suffix, now.Add(time.Hour), now)
	opHash := sha256.Sum256([]byte("bi-operation:" + operation))
	exec(`INSERT INTO paperboat.operations(id,account_id,idempotency_key,request_hash,operation_type,resource_kind,resource_id,phase,state,progress,retrying,outcome,correlation_id,created_at,updated_at) VALUES($1,$2,$1,$3,'preview.create','preview_lease',$4,'connecting','running',60,false,'changed',$5,$6,$6)`, operation, owner, opHash[:], lease, "cor_"+operation, now)
	exec(`INSERT INTO paperboat.preview_lease_create_operations(account_id,preview_id,operation_id) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, owner, lease, operation)
	testutil.EnginePreview{Suffix: suffix, Lease: lease, Account: owner, Machine: machine, Node: node, Epoch: epoch, PublicKey: publicKey, RouteRevision: 7, Now: now}.Serve(t, exec)
	exec(`INSERT INTO paperboat.machine_control_renewals(operation_id,machine_id,installation_generation,credential_jti,issued_at,expires_at,session_generation) VALUES($1,$2,1,$3,$4,$5,1)`, "mc_bi_"+suffix, machine, "mcc_bi_"+suffix, now, now.Add(time.Hour))
	exec(`INSERT INTO paperboat.machine_control_sessions(machine_id,installation_generation,session_generation,operation_id,credential_jti,issued_at,expires_at) VALUES($1,1,1,$2,$3,$4,$5)`, machine, "mc_bi_"+suffix, "mcc_bi_"+suffix, now, now.Add(time.Hour))

	svc := &Service{DB: store}
	auth := browseraccess.Authorization{OwnerAccountID: owner, ResourceKind: "preview", ResourceID: lease, RouteID: lease, IssuedAt: now}
	serves := func() bool {
		t.Helper()
		_, e := svc.preview(ctx, auth, node, epoch)
		return e == nil
	}
	if !serves() {
		t.Fatal("ready edge does not serve the preview: fixture is incomplete")
	}
	for _, grace := range []time.Duration{10 * time.Minute, 90 * time.Second} {
		exec(`UPDATE paperboat.control_tunnel_nodes SET state='draining',ready=false,drain_deadline=$2 WHERE id=$1`, node, now.Add(grace))
		if !serves() {
			t.Fatalf("draining edge (deadline in %s) dropped the preview before the deadline", grace)
		}
	}
	exec(`UPDATE paperboat.control_tunnel_nodes SET drain_deadline=$2 WHERE id=$1`, node, now.Add(-time.Second))
	if serves() {
		t.Fatal("drained edge kept the preview after the drain deadline")
	}
	exec(`UPDATE paperboat.control_tunnel_nodes SET state='ready',ready=true,drain_deadline=NULL WHERE id=$1`, node)
	if !serves() {
		t.Fatal("preview not restored after the edge returned to ready")
	}
}

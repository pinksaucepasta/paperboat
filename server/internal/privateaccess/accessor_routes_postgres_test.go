package privateaccess

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
)

// TestAccessorRouteDiscoveryOnPostgres is opt-in and accepts only an isolated
// *_test database. It exercises the production sqlc queries through all
// migrations without starting a server, edge, Docker, or local PostgreSQL.
func TestAccessorRouteDiscoveryOnPostgres(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("PAPERBOAT_TEST_DATABASE_DSN"))
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN to run TRK-17 PostgreSQL acceptance")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	if !safeAccessorAcceptanceDSN(dsn) {
		t.Fatal("PAPERBOAT_TEST_DATABASE_DSN must name an isolated *_test database")
	}
	ctx := context.Background()
	database, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := database.Close(); err != nil {
			t.Errorf("close isolated accessor test database: %v", err)
		}
	})
	if err := db.Migrate(ctx, database); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	f := newAccessorAcceptanceFixture(fmt.Sprint(time.Now().UnixNano()), now)
	t.Cleanup(func() {
		if _, err := database.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.users WHERE id IN ($1,$2)`, f.accountID, f.otherAccountID); err != nil {
			t.Errorf("clean up accessor test users: %v", err)
		}
		if _, err := database.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.control_tunnel_nodes WHERE id=$1`, f.nodeID); err != nil {
			t.Errorf("clean up accessor test edge node: %v", err)
		}
	})
	f.insert(t, database)
	repository, err := NewAccessorRepository(database)
	if err != nil {
		t.Fatal(err)
	}
	repository.now = func() time.Time { return now }

	routes, err := repository.MachineRoutes(ctx, f.accountID, f.accessorID)
	if err != nil {
		t.Fatal(err)
	}
	// Tunnel routes ride the endpoint engine; runtimeattachment's
	// TestPrivateEngineTunnelRouteIsDiscoveredAndResolvedOnPostgres covers them.
	if len(routes) != 1 {
		t.Fatalf("same-account routes=%d, want the preview route: %#v", len(routes), routes)
	}
	assertAccessorBindings(t, routes, f)

	admissions, err := repository.EdgeAdmissions(ctx, f.nodeID, f.epoch)
	if err != nil {
		t.Fatal(err)
	}
	if len(admissions) != 2 { // the preview route for the accessor and host machines
		t.Fatalf("edge admissions=%d, want the preview route for two eligible machines", len(admissions))
	}

	if wrong, err := repository.MachineRoutes(ctx, f.otherAccountID, f.accessorID); err != nil || len(wrong) != 0 {
		t.Fatalf("wrong-account routes=%#v err=%v", wrong, err)
	}
	for name, mutation := range map[string]string{
		"offline": `UPDATE paperboat.user_machines SET online=false,state='offline',updated_at=$2 WHERE id=$1`,
		"revoked": `UPDATE paperboat.user_machines SET revoked_at=$2 WHERE id=$1`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := database.SQL().ExecContext(ctx, mutation, f.accessorID, now); err != nil {
				t.Fatal(err)
			}
			got, err := repository.MachineRoutes(ctx, f.accountID, f.accessorID)
			if err != nil || len(got) != 0 {
				t.Fatalf("routes=%#v err=%v", got, err)
			}
			if _, err := database.SQL().ExecContext(ctx, `UPDATE paperboat.user_machines SET online=true,state='online',revoked_at=NULL WHERE id=$1`, f.accessorID); err != nil {
				t.Fatal(err)
			}
		})
	}
	if _, err := database.SQL().ExecContext(ctx, `UPDATE paperboat.user_machines SET installation_generation=installation_generation+1 WHERE id=$1`, f.accessorID); err != nil {
		t.Fatal(err)
	}
	current, err := repository.MachineRoutes(ctx, f.accountID, f.accessorID)
	if err != nil || len(current) != 1 || current[0].InstallationGeneration != f.installationGeneration+1 {
		t.Fatalf("renewed installation projection=%#v err=%v", current, err)
	}

	if _, err := database.SQL().ExecContext(ctx, `UPDATE paperboat.control_tunnel_nodes SET process_epoch=$2 WHERE id=$1`, f.nodeID, f.replacementEpoch); err != nil {
		t.Fatal(err)
	}
	if got, err := repository.MachineRoutes(ctx, f.accountID, f.accessorID); err != nil || len(got) != 0 {
		t.Fatalf("stale replaced edge routes=%#v err=%v", got, err)
	}
	if got, err := repository.EdgeAdmissions(ctx, f.nodeID, f.epoch); err != nil || len(got) != 0 {
		t.Fatalf("stale edge admissions=%#v err=%v", got, err)
	}
	if _, err := database.SQL().ExecContext(ctx, `UPDATE paperboat.control_tunnel_nodes SET process_epoch=$2 WHERE id=$1`, f.nodeID, f.epoch); err != nil {
		t.Fatal(err)
	}

	// One current route (the preview) multiplied by limit+1 current accessors must be
	// rejected rather than returned as an incomplete complete=true snapshot.
	type limitMachine struct {
		ID            string `json:"id"`
		EnvironmentID string `json:"environment_id"`
		PublicKey     string `json:"public_key"`
	}
	machines := make([]limitMachine, 0, accessorLimit+1)
	for index := 0; index < accessorLimit+1; index++ {
		seed := sha256.Sum256([]byte(fmt.Sprintf("trk17-limit:%s:%d", f.suffix, index)))
		public := ed25519.NewKeyFromSeed(seed[:]).Public().(ed25519.PublicKey)
		machineID := fmt.Sprintf("mch_limit_%s_%d", f.suffix, index)
		machines = append(machines, limitMachine{ID: machineID, EnvironmentID: fmt.Sprintf("env_limit_%s_%d", f.suffix, index), PublicKey: base64.RawURLEncoding.EncodeToString(public)})
	}
	machineRows, err := json.Marshal(machines)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machines (id,user_id,environment_id,platform,architecture,workspace_root,state,seat_state,online,public_identity_key,installation_generation) SELECT m.id,$1,m.environment_id,'linux','amd64','/workspace','online','occupied',true,m.public_key,1 FROM jsonb_to_recordset($2::jsonb) AS m(id text,environment_id text,public_key text)`, f.accountID, string(machineRows)); err != nil {
		t.Fatal(err)
	}

	if _, err := repository.EdgeAdmissions(ctx, f.nodeID, f.epoch); err == nil {
		t.Fatal("limit+1 edge snapshot succeeded; want refusal of incomplete snapshot")
	}
}

type accessorAcceptanceFixture struct {
	suffix, accountID, otherAccountID, accessorID, hostID   string
	previewID, operationID, tunnelID, routeID, tcpRouteID   string
	connectorID, sessionID, nodeID, epoch, replacementEpoch string
	publicKey, hostPublicKey, thumbprint, hostThumbprint    string
	installationGeneration                                  uint64
	now                                                     time.Time
}

func newAccessorAcceptanceFixture(suffix string, now time.Time) accessorAcceptanceFixture {
	seed := sha256.Sum256([]byte("trk17-accessor:" + suffix))
	public := ed25519.NewKeyFromSeed(seed[:]).Public().(ed25519.PublicKey)
	hostSeed := sha256.Sum256([]byte("trk17-host:" + suffix))
	hostPublic := ed25519.NewKeyFromSeed(hostSeed[:]).Public().(ed25519.PublicKey)
	thumb, _ := connectorprotocol.IdentityThumbprint(public)
	hostThumb, _ := connectorprotocol.IdentityThumbprint(hostPublic)
	return accessorAcceptanceFixture{suffix: suffix, accountID: "usr_trk17_" + suffix, otherAccountID: "usr_trk17_other_" + suffix, accessorID: "mch_trk17_accessor_" + suffix, hostID: "mch_trk17_host_" + suffix, previewID: "prv_trk17_" + suffix, operationID: "op_trk17_" + suffix, tunnelID: "tun_trk17_" + suffix, routeID: "rte_trk17_" + suffix, tcpRouteID: "rte_trk17_tcp_" + suffix, connectorID: "con_trk17_" + suffix, sessionID: "ses_trk17_" + suffix, nodeID: "edge-trk17-" + suffix, epoch: "edge-process-trk17-" + suffix, replacementEpoch: "edge-replacement-trk17-" + suffix, publicKey: base64.RawURLEncoding.EncodeToString(public), hostPublicKey: base64.RawURLEncoding.EncodeToString(hostPublic), thumbprint: thumb, hostThumbprint: hostThumb, installationGeneration: 7, now: now}
}

func (f accessorAcceptanceFixture) insert(t *testing.T, database *db.DB) {
	t.Helper()
	ctx := context.Background()
	requestHash := sha256.Sum256([]byte("trk17-request:" + f.suffix))
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := database.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO paperboat.users (id,workos_subject,primary_email,status) VALUES ($1,$2,$3,'active'),($4,$5,$6,'active')`, f.accountID, "workos_"+f.accountID, f.accountID+"@example.test", f.otherAccountID, "workos_"+f.otherAccountID, f.otherAccountID+"@example.test")
	exec(`INSERT INTO paperboat.user_machines (id,user_id,environment_id,platform,architecture,workspace_root,state,seat_state,online,public_identity_key,installation_generation) VALUES ($1,$2,$3,'linux','amd64','/workspace','online','occupied',true,$4,$5),($6,$2,$7,'linux','amd64','/workspace','online','occupied',true,$8,1)`, f.accessorID, f.accountID, "env_"+f.accessorID, f.publicKey, f.installationGeneration, f.hostID, "env_"+f.hostID, f.hostPublicKey)
	exec(`INSERT INTO paperboat.control_tunnel_nodes (id,edge_pool,protocol_version,process_epoch,endpoint_host,endpoint_tcp_port,endpoint_quic_port,carrier_endpoint_host,carrier_endpoint_tcp_port,carrier_endpoint_quic_port,carrier_server_spki_sha256,carrier_server_certificate_chain_pem,state,ready,capacity,last_heartbeat_at,region,failure_domain,roles,transports,capacity_limit,capacity_used,capacity_observed_at,registry_expires_at) VALUES ($1,'test','1.0',$2,'edge.example.test',24001,24002,'edge.example.test',25001,25002,'sha256:' || repeat('b',64),'test-public-certificate-chain','ready',true,'{}',$3,'test','test',ARRAY['edge'],ARRAY['http3','http2'],100,0,$3,$3::timestamptz+interval '1 hour')`, f.nodeID, f.epoch, f.now)
	exec(`INSERT INTO paperboat.preview_leases (owner_session_kind,id,endpoint,account_id,actor_id,owner_machine_id,owner_session_id,target_scheme,target_address,access_mode,lease_deadline,allocation_state,edge_state,origin_state,terminal_state,created_at,last_renewed_at) VALUES ('foreground',$1,$2,$3,$3,$4,$5,'http','127.0.0.1:3000','private',$6,'ready','ready','ready','active',$7,$7)`, f.previewID, "https://preview-"+f.suffix+".preview.example.test", f.accountID, f.hostID, "owner_"+f.suffix, f.now.Add(time.Hour), f.now)
	exec(`INSERT INTO paperboat.operations (id,account_id,idempotency_key,request_hash,operation_type,resource_kind,resource_id,phase,state,progress,retrying,outcome,correlation_id,created_at,updated_at,completed_at) VALUES ($1,$2,$1,$3,'preview.create','preview_lease',$4,'ready','succeeded',100,false,'changed',$5,$6,$6,$6)`, f.operationID, f.accountID, requestHash[:], f.previewID, "cor_"+f.suffix, f.now)
	exec(`INSERT INTO paperboat.preview_lease_carrier_attachments (account_id,preview_id,operation_id,idempotency_key,request_id,correlation_id,request_hash,owner_machine_id,owner_session_id,host_id,edge_node_id,edge_process_epoch,edge_carrier_server_spki_sha256,edge_carrier_server_certificate_chain_pem,machine_identity_public_key,machine_identity_thumbprint,lease_generation,tunnel_id,connector_id,connector_session_id,process_generation,config_generation,route_id,route_generation,config_content_hash,edge_endpoints,state,edge_ready,origin_ready,issued_at,expires_at,ready_at) VALUES ($1,$2,$3,$3,$4,$5,$6,$7,$8,$7,$9,$10,'sha256:' || repeat('b',64),'test-public-certificate-chain',$11,$12,4,$13,$14,$15,2,3,$16,7,$17,ARRAY['h2://edge.example.test:25001','h3://edge.example.test:25002'],'ready',true,true,$18,$19,$18)`, f.accountID, f.previewID, f.operationID, "req_"+f.suffix, "cor_"+f.suffix, requestHash[:], f.hostID, "owner_"+f.suffix, f.nodeID, f.epoch, f.hostPublicKey, "sha256:"+strings.TrimPrefix(f.hostThumbprint, "sha256:"), "pvc_"+f.suffix, "pcc_"+f.suffix, "pcs_"+f.suffix, "proute_"+f.suffix, "sha256:"+strings.Repeat("a", 64), f.now, f.now.Add(30*time.Minute))
}

func assertAccessorBindings(t *testing.T, routes []AccessorAdmission, f accessorAcceptanceFixture) {
	t.Helper()
	byKind := map[string]AccessorAdmission{}
	for _, v := range routes {
		byKind[v.ResourceKind+"\x00"+v.Protocol] = v
		if v.AccountID != f.accountID || v.MachineID != f.accessorID || v.InstallationGeneration != f.installationGeneration || v.AccessorPublicKey != f.publicKey || v.AccessorThumbprint != f.thumbprint || v.EdgeNodeID != f.nodeID || v.EdgeProcessEpoch != f.epoch {
			t.Fatalf("binding mismatch: %#v", v)
		}
	}
	p := byKind["preview\x00http"]
	if p.RouteGeneration != 7 || p.SessionGeneration != 4 || p.ProcessGeneration != 2 || p.ConfigGeneration != 3 || p.AssignmentGeneration != 4 || p.AssignmentID != f.operationID || p.ConfigContentHash != "sha256:"+strings.Repeat("a", 64) || p.MatchType != "exact" {
		t.Fatalf("preview generations=%#v", p)
	}
}

func safeAccessorAcceptanceDSN(dsn string) bool {
	u, err := url.Parse(dsn)
	if err != nil {
		return false
	}
	name := strings.TrimPrefix(u.Path, "/")
	return strings.HasSuffix(name, "_test") && name != ""
}

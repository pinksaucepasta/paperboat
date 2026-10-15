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

	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/controlplane"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/testutil"
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
	tcpAssignment := f.stageAndActivatePrivateTCP(t, database)
	repository, err := NewAccessorRepository(database)
	if err != nil {
		t.Fatal(err)
	}
	repository.now = func() time.Time { return now }

	routes, err := repository.MachineRoutes(ctx, f.accountID, f.accessorID)
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 3 {
		t.Fatalf("same-account routes=%d, want preview, durable HTTP, and private TCP route: %#v", len(routes), routes)
	}
	assertAccessorBindings(t, routes, f, tcpAssignment)

	t.Run("owner preview uses its owner carrier instead of accessor discovery", func(t *testing.T) {
		ownerRoutes, err := repository.MachineRoutes(ctx, f.accountID, f.hostID)
		if err != nil {
			t.Fatal(err)
		}
		if len(ownerRoutes) != 2 {
			t.Fatalf("owner accessor routes=%d, want durable HTTP and private TCP only", len(ownerRoutes))
		}
		protocols := map[string]bool{}
		for _, route := range ownerRoutes {
			if route.ResourceKind != ResourceTunnel || route.ResourceID != f.tunnelID || route.MachineID != f.hostID || route.InstallationGeneration != 1 || route.AccessorPublicKey != f.hostPublicKey {
				t.Fatal("owner discovery changed exact durable authority or duplicated owner preview")
			}
			protocols[route.Protocol] = true
		}
		if !protocols[ProtocolHTTP] || !protocols["private_tcp"] {
			t.Fatal("owner discovery omitted a durable protocol")
		}
	})
	t.Run("durable resolver timestamp parameter", func(t *testing.T) {
		var current AccessorAdmission
		for _, row := range routes {
			if row.ResourceKind == ResourceTunnel && row.Protocol == "http" {
				current = row
			}
		}
		request := testRequest(now)
		request.AccountID, request.MachineID, request.ResourceKind, request.ResourceID, request.RouteID = f.accountID, f.accessorID, ResourceTunnel, current.ResourceID, current.RouteID
		request.ConnectorID, request.OperationID, request.CarrierSessionID = current.ConnectorID, "", current.CarrierSessionID
		request.Audience, request.Host, request.Path = AudienceTunnelHTTP, current.Hostname, "/app"
		request.InstallationGeneration, request.RouteGeneration, request.SessionGeneration, request.ProcessGeneration, request.ConfigGeneration, request.AssignmentGeneration = current.InstallationGeneration, current.RouteGeneration, current.SessionGeneration, current.ProcessGeneration, current.ConfigGeneration, current.AssignmentGeneration
		request.EdgeNodeID, request.EdgeProcessEpoch = f.nodeID, f.epoch
		resolver := &SQLResolver{db: database}
		binding, err := resolver.ResolvePrivate(ctx, Lookup{AccountID: f.accountID, Request: request, Edge: EdgeIdentity{NodeID: f.nodeID, ProcessEpoch: f.epoch}, Now: now})
		if err != nil {
			t.Fatal(err)
		}
		if err := exactBinding(request, binding); err != nil {
			t.Fatal(err)
		}
		if binding.Protocol != ProtocolHTTP || binding.Hostname != current.Hostname {
			t.Fatal("current durable HTTP binding changed")
		}
		missing := request
		missing.ResourceID = "tun_missing_" + f.suffix
		if _, err := resolver.ResolvePrivate(ctx, Lookup{AccountID: f.accountID, Request: missing, Edge: EdgeIdentity{NodeID: f.nodeID, ProcessEpoch: f.epoch}, Now: now}); err != ErrResourceNotFound {
			t.Fatalf("missing exact resource error = %v, want expected not-found denial", err)
		}
	})

	admissions, err := repository.EdgeAdmissions(ctx, f.nodeID, f.epoch)
	if err != nil {
		t.Fatal(err)
	}
	if len(admissions) != 6 { // three routes for accessor and connector-host machines
		t.Fatalf("edge admissions=%d, want three routes for two eligible machines", len(admissions))
	}

	t.Run("managed HTTP match projects canonical wire type", func(t *testing.T) {
		if _, err := database.SQL().ExecContext(ctx, `UPDATE paperboat.tunnel_routes SET match_type='managed' WHERE id=$1`, f.routeID); err != nil {
			t.Fatal(err)
		}
		defer func() {
			if _, err := database.SQL().ExecContext(ctx, `UPDATE paperboat.tunnel_routes SET match_type='exact' WHERE id=$1`, f.routeID); err != nil {
				t.Fatal(err)
			}
		}()
		machineRoutes, err := repository.MachineRoutes(ctx, f.accountID, f.accessorID)
		if err != nil {
			t.Fatal(err)
		}
		edgeRoutes, err := repository.EdgeAdmissions(ctx, f.nodeID, f.epoch)
		if err != nil {
			t.Fatal(err)
		}
		for _, rows := range [][]AccessorAdmission{machineRoutes, edgeRoutes} {
			found := false
			for _, row := range rows {
				if row.RouteID == f.routeID {
					found = true
					if row.MatchType != "managed_exact" {
						t.Fatalf("managed route wire match type=%q", row.MatchType)
					}
				}
			}
			if !found {
				t.Fatal("managed route missing from complete snapshot")
			}
		}
	})

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
	if err != nil || len(current) != 3 || current[0].InstallationGeneration != f.installationGeneration+1 {
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

	if _, err := database.SQL().ExecContext(ctx, `UPDATE paperboat.tunnel_edge_route_assignments SET state='detached',observed_state='detached',released_at=$2,updated_at=$2 WHERE assignment_id=$1`, tcpAssignment.assignmentID, now); err != nil {
		t.Fatal(err)
	}
	withoutTCP, err := repository.MachineRoutes(ctx, f.accountID, f.accessorID)
	if err != nil || len(withoutTCP) != 2 {
		t.Fatalf("detached private TCP withdrawal=%#v err=%v", withoutTCP, err)
	}

	// One current route multiplied by limit+1 current accessors must be rejected
	// rather than returned as an incomplete complete=true snapshot.
	if _, err := database.SQL().ExecContext(ctx, `UPDATE paperboat.preview_leases SET terminal_state='stopped',stopped_at=$2 WHERE id=$1`, f.previewID, now); err != nil {
		t.Fatal(err)
	}
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
	if _, err := database.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machines (id,user_id,environment_id,description,platform,architecture,workspace_root,state,seat_state,online,public_identity_key,installation_generation) SELECT m.id,$1,m.environment_id,m.id,'linux','amd64','/workspace','online','occupied',true,m.public_key,1 FROM jsonb_to_recordset($2::jsonb) AS m(id text,environment_id text,public_key text)`, f.accountID, string(machineRows)); err != nil {
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
	hash := sha256.Sum256([]byte("trk17-config:" + f.suffix))
	requestHash := sha256.Sum256([]byte("trk17-request:" + f.suffix))
	endpointID := testutil.EndpointID("accessor-routes:" + f.suffix)
	endpoint := "https://" + testutil.EndpointLabel(endpointID) + ".tunnels.example.test"
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := database.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO paperboat.users (id,workos_subject,primary_email,status) VALUES ($1,$2,$3,'active'),($4,$5,$6,'active')`, f.accountID, "workos_"+f.accountID, f.accountID+"@example.test", f.otherAccountID, "workos_"+f.otherAccountID, f.otherAccountID+"@example.test")
	exec(`INSERT INTO paperboat.user_machines (id,user_id,environment_id,description,platform,architecture,workspace_root,state,seat_state,online,public_identity_key,installation_generation) VALUES ($1,$2,$3,'Accessor','linux','amd64','/workspace','online','occupied',true,$4,$5),($6,$2,$7,'Host','linux','amd64','/workspace','online','occupied',true,$8,1)`, f.accessorID, f.accountID, "env_"+f.accessorID, f.publicKey, f.installationGeneration, f.hostID, "env_"+f.hostID, f.hostPublicKey)
	exec(`INSERT INTO paperboat.control_tunnel_nodes (id,edge_pool,protocol_version,process_epoch,endpoint_host,endpoint_tcp_port,endpoint_quic_port,carrier_endpoint_host,carrier_endpoint_tcp_port,carrier_endpoint_quic_port,carrier_server_spki_sha256,carrier_server_certificate_chain_pem,state,ready,capacity,last_heartbeat_at,region,failure_domain,roles,transports,capacity_limit,capacity_used,capacity_observed_at,registry_expires_at) VALUES ($1,'test','1.0',$2,'edge.example.test',24001,24002,'edge.example.test',25001,25002,'sha256:' || repeat('b',64),'test-public-certificate-chain','ready',true,'{}',$3,'test','test',ARRAY['edge'],ARRAY['http3','http2'],100,0,$3,$3::timestamptz+interval '1 hour')`, f.nodeID, f.epoch, f.now)
	exec(`INSERT INTO paperboat.preview_leases (owner_session_kind,id,endpoint,account_id,actor_id,owner_machine_id,owner_session_id,target_scheme,target_address,access_mode,lease_deadline,allocation_state,edge_state,origin_state,terminal_state,created_at,last_renewed_at) VALUES ('foreground',$1,$2,$3,$3,$4,$5,'http','127.0.0.1:3000','private',$6,'ready','ready','ready','active',$7,$7)`, f.previewID, "https://preview-"+f.suffix+".preview.example.test", f.accountID, f.hostID, "owner_"+f.suffix, f.now.Add(time.Hour), f.now)
	exec(`INSERT INTO paperboat.operations (id,account_id,idempotency_key,request_hash,operation_type,resource_kind,resource_id,phase,state,progress,retrying,outcome,correlation_id,created_at,updated_at,completed_at) VALUES ($1,$2,$1,$3,'preview.create','preview_lease',$4,'ready','succeeded',100,false,'changed',$5,$6,$6,$6)`, f.operationID, f.accountID, requestHash[:], f.previewID, "cor_"+f.suffix, f.now)
	exec(`INSERT INTO paperboat.preview_lease_carrier_attachments (account_id,preview_id,operation_id,idempotency_key,request_id,correlation_id,request_hash,owner_machine_id,owner_session_id,host_id,edge_node_id,edge_process_epoch,edge_carrier_server_spki_sha256,edge_carrier_server_certificate_chain_pem,machine_identity_public_key,machine_identity_thumbprint,lease_generation,tunnel_id,connector_id,connector_session_id,process_generation,config_generation,route_id,route_generation,config_content_hash,edge_endpoints,state,edge_ready,origin_ready,issued_at,expires_at,ready_at) VALUES ($1,$2,$3,$3,$4,$5,$6,$7,$8,$7,$9,$10,'sha256:' || repeat('b',64),'test-public-certificate-chain',$11,$12,4,$13,$14,$15,2,3,$16,7,$17,ARRAY['h2://edge.example.test:25001','h3://edge.example.test:25002'],'ready',true,true,$18,$19,$18)`, f.accountID, f.previewID, f.operationID, "req_"+f.suffix, "cor_"+f.suffix, requestHash[:], f.hostID, "owner_"+f.suffix, f.nodeID, f.epoch, f.hostPublicKey, "sha256:"+strings.TrimPrefix(f.hostThumbprint, "sha256:"), "pvc_"+f.suffix, "pcc_"+f.suffix, "pcs_"+f.suffix, "proute_"+f.suffix, "sha256:"+strings.Repeat("a", 64), f.now, f.now.Add(30*time.Minute))
	exec(`INSERT INTO paperboat.tunnels (id,account_id,name,desired_state,access_mode,generation,stable_endpoint_id,stable_endpoint,created_by_host_id,created_by_actor_id,summary_code,created_at,updated_at) VALUES ($1,$2,'trk17','active','private',5,$3,$4,$5,$2,'ready',$6,$6)`, f.tunnelID, f.accountID, endpointID, endpoint, f.hostID, f.now)
	exec(`INSERT INTO paperboat.tunnel_routes (id,tunnel_id,name,protocol,match_type,match_hostname,path_prefix,origin_scheme,origin_address,generation,desired_state,created_by_actor_id,updated_by_actor_id,created_at,updated_at) VALUES ($1,$2,'web','http','exact',$3,'/app','http','127.0.0.1:3000',7,'active',$4,$4,$5,$5)`, f.routeID, f.tunnelID, "durable-"+f.suffix+".example.test", f.accountID, f.now)
	exec(`INSERT INTO paperboat.tunnel_routes (id,tunnel_id,name,protocol,match_type,origin_scheme,origin_address,generation,desired_state,created_by_actor_id,updated_by_actor_id,created_at,updated_at) VALUES ($1,$2,'tcp','private_tcp','catch_all','tcp','127.0.0.1:5432',8,'active',$3,$3,$4,$4)`, f.tcpRouteID, f.tunnelID, f.accountID, f.now)
	exec(`INSERT INTO paperboat.tunnel_config_generations (tunnel_id,generation,content_hash,snapshot,activation_state,created_by_actor_id,created_at,activated_at,retained_until) VALUES ($1,3,$2,convert_to(jsonb_build_object('routes',jsonb_build_array(jsonb_build_object('id',$6::text),jsonb_build_object('id',$7::text)))::text,'UTF8'),'active',$3,$4,$4,$5)`, f.tunnelID, hash[:], f.accountID, f.now, f.now.Add(time.Hour), f.routeID, f.tcpRouteID)
	exec(`INSERT INTO paperboat.tunnel_connectors (id,tunnel_id,host_id,credential_reference,credential_thumbprint,rotation_generation,desired_state,protocol_version,last_session_id,last_heartbeat_at,ready_at,last_applied_config_generation,drain_state,generation,created_at,updated_at) VALUES ($1,$2,$3,$4,$5,1,'active','1.0',$6,$7,$7,3,'accepting',4,$7,$7)`, f.connectorID, f.tunnelID, f.hostID, "keychain://trk17/"+f.connectorID, f.thumbprint+f.connectorID, f.sessionID, f.now)
	exec(`INSERT INTO paperboat.tunnel_connector_sessions (id,connector_id,process_generation,protocol_version,capabilities,credential_generation,state,lease_deadline,last_heartbeat_at,ready_at,applied_config_generation,retained_until,created_at) VALUES ($1,$2,2,'1.0',ARRAY['config.snapshot.v1'],1,'ready',$3,$4,$4,3,$5,$4)`, f.sessionID, f.connectorID, f.now.Add(time.Hour), f.now, f.now.Add(2*time.Hour))
	exec(`INSERT INTO paperboat.tunnel_edge_route_assignments (assignment_id,route_id,assignment_generation,account_id,tunnel_id,connector_id,host_id,machine_identity_public_key,machine_identity_thumbprint,connector_generation,connector_session_id,connector_process_generation,config_generation,config_content_hash,access_mode,route_generation,route_revision,viewer_policy_generation,edge_node_id,edge_process_epoch,edge_failure_domain,state,observed_state,assigned_at,observed_at,created_at,updated_at) VALUES ($1,$2,9,$3,$4,$5,$6,$7,$8,4,$9,2,3,$10,'private',7,7,1,$11,$12,'test','active','ready',$13,$13,$13,$13)`, "asg_"+f.suffix, f.routeID, f.accountID, f.tunnelID, f.connectorID, f.hostID, f.hostPublicKey, strings.TrimPrefix(f.hostThumbprint, "sha256:"), f.sessionID, hash[:], f.nodeID, f.epoch, f.now)
}

type tcpAssignmentEvidence struct {
	assignmentID, configContentHash string
	assignmentGeneration            uint64
}

func (f accessorAcceptanceFixture) stageAndActivatePrivateTCP(t *testing.T, database *db.DB) tcpAssignmentEvidence {
	t.Helper()
	ctx := context.Background()
	service := controlplane.NewEdgeService(database, "trk17-acceptance")
	service.SetClock(func() time.Time { return f.now })
	staged, err := service.ReconcileTunnelEdgeRouteAssignments(ctx, 10)
	if err != nil || staged != 1 {
		t.Fatalf("private TCP reconcile=%d,%v, want one staged assignment", staged, err)
	}
	var assignmentID, accessMode string
	var viewerPolicyGeneration int64
	var assignmentGeneration, routeRevision, connectorGeneration, processGeneration, configGeneration int64
	var configHash []byte
	if err := database.SQL().QueryRowContext(ctx, `SELECT assignment_id,assignment_generation,route_revision,connector_generation,connector_process_generation,config_generation,config_content_hash,access_mode,viewer_policy_generation FROM paperboat.tunnel_edge_route_assignments WHERE route_id=$1 AND state='staged'`, f.tcpRouteID).Scan(&assignmentID, &assignmentGeneration, &routeRevision, &connectorGeneration, &processGeneration, &configGeneration, &configHash, &accessMode, &viewerPolicyGeneration); err != nil {
		t.Fatal(err)
	}
	observation := controlplane.RouteObservation{RouteID: f.tcpRouteID, AssignmentID: assignmentID, AssignmentGeneration: assignmentGeneration, RouteRevision: routeRevision, EdgeNodeID: f.nodeID, EdgeProcessEpoch: f.epoch, ConnectorID: f.connectorID, HostID: f.hostID, ConnectorGeneration: connectorGeneration, ConnectorSessionID: f.sessionID, ConnectorProcessGeneration: processGeneration, ConfigGeneration: configGeneration, ConfigContentHash: fmt.Sprintf("sha256:%x", configHash), State: "ready", AccessMode: accessMode, ViewerPolicyGeneration: viewerPolicyGeneration}
	if err := service.ObserveRoutes(ctx, f.nodeID, []controlplane.RouteObservation{observation}); err != nil {
		t.Fatalf("activate private TCP assignment: %v", err)
	}
	var state, observed string
	if err := database.SQL().QueryRowContext(ctx, `SELECT state,observed_state FROM paperboat.tunnel_edge_route_assignments WHERE assignment_id=$1`, assignmentID).Scan(&state, &observed); err != nil || state != "active" || observed != "ready" {
		t.Fatalf("private TCP assignment state=%s/%s err=%v", state, observed, err)
	}
	return tcpAssignmentEvidence{assignmentID: assignmentID, assignmentGeneration: uint64(assignmentGeneration), configContentHash: fmt.Sprintf("sha256:%x", configHash)}
}

func assertAccessorBindings(t *testing.T, routes []AccessorAdmission, f accessorAcceptanceFixture, tcp tcpAssignmentEvidence) {
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
	d := byKind["tunnel\x00http"]
	hash := sha256.Sum256([]byte("trk17-config:" + f.suffix))
	if d.TunnelName != "trk17" || d.RouteName != "web" || d.RouteGeneration != 7 || d.SessionGeneration != 4 || d.ProcessGeneration != 2 || d.ConfigGeneration != 3 || d.AssignmentGeneration != 9 || d.CarrierSessionID != f.sessionID || d.ConnectorID != f.connectorID || d.AssignmentID != "asg_"+f.suffix || d.ConfigContentHash != fmt.Sprintf("sha256:%x", hash) || d.MatchType != "exact" {
		t.Fatalf("durable generations=%#v", d)
	}
	privateTCP := byKind["tunnel\x00private_tcp"]
	if privateTCP.TunnelName != "trk17" || privateTCP.RouteName != "tcp" || privateTCP.RouteID != f.tcpRouteID || privateTCP.Hostname != "" || privateTCP.MatchType != "catch_all" || privateTCP.AssignmentID != tcp.assignmentID || privateTCP.AssignmentGeneration != tcp.assignmentGeneration || privateTCP.ConfigContentHash != tcp.configContentHash {
		t.Fatalf("private TCP authoritative binding=%#v", privateTCP)
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

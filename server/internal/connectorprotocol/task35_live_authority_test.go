package connectorprotocol

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"crypto/sha256"
	"github.com/pinksaucepasta/paperboat-server/internal/secrets"
	"github.com/pinksaucepasta/paperboat-server/internal/selfhost"
)

type task35AuthorityEdge struct {
	NodeID string `json:"node_id"`
	Epoch  string `json:"epoch"`
	Path   string `json:"path"`
}

type task35AuthorityTLSRoute struct {
	RouteID       string `json:"route_id"`
	Hostname      string `json:"hostname"`
	OriginAddress string `json:"origin_address"`
}

type task35AuthorityConfig struct {
	Edges            []task35AuthorityEdge     `json:"edges"`
	Hostname         string                    `json:"hostname"`
	OriginAddress    string                    `json:"origin_address"`
	PartitionPath    string                    `json:"partition_path"`
	StopPath         string                    `json:"stop_path"`
	ReadyPath        string                    `json:"ready_path"`
	TLSListenerPort  int32                     `json:"tls_port"`
	TLSRoutes        []task35AuthorityTLSRoute `json:"tls_routes"`
	RemoveTLSPath    string                    `json:"remove_tls_path"`
	DisableEdgePath  string                    `json:"disable_edge_path"`
	RestoreEdgePath  string                    `json:"restore_edge_path"`
	Selfhost         bool                      `json:"selfhost"`
	RevokePairPath   string                    `json:"revoke_pair_path"`
	RestorePairPath  string                    `json:"restore_pair_path"`
	SelfhostOnlyPath string                    `json:"selfhost_only_path"`
}

// TestTask35LiveIngressAuthority projects short-lived ingress decisions from
// the current SQL authority for two physical edge processes. The coordinator
// owns only private paths and the isolated PAPERBOAT_TEST_DATABASE_DSN.
func TestTask35LiveIngressAuthority(t *testing.T) {
	path := strings.TrimSpace(os.Getenv("PAPERBOAT_TASK35_AUTHORITY_CONFIG"))
	if path == "" {
		t.Skip("Task 35 live authority disabled")
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 || len(data) > 1<<20 {
		t.Fatal("read bounded authority config")
	}
	var cfg task35AuthorityConfig
	if json.Unmarshal(data, &cfg) != nil || len(cfg.Edges) != 2 || cfg.Hostname == "" || cfg.OriginAddress == "" || cfg.PartitionPath == "" || cfg.StopPath == "" || cfg.ReadyPath == "" {
		t.Fatal("invalid authority config")
	}
	for _, edge := range cfg.Edges {
		if edge.NodeID == "" || edge.Epoch == "" || edge.Path == "" {
			t.Fatal("invalid authority edge")
		}
	}

	f := newTRK08PostgresFixture(t)
	ctx := context.Background()
	c := f.connectors[0]
	resources := []*trk08PostgresFixture{f}
	if len(cfg.TLSRoutes) > 1 {
		if len(cfg.TLSRoutes) != 2 {
			t.Fatal("TLS process fixture supports at most two tunnels")
		}
		resources = append(resources, newTRK08PostgresFixture(t))
	}
	for i := range cfg.TLSRoutes {
		cfg.TLSRoutes[i].RouteID += "_" + f.suffix
	}
	var selfhostService *selfhost.Service
	var selfhostInstallation selfhost.Registered
	teamID := "task35_team_" + f.suffix
	if cfg.Selfhost {
		installationPublic, _, _ := ed25519.GenerateKey(rand.Reader)
		selfhostService = selfhost.New(f.database, "task35-selfhost-test-encryption-key")
		selfhostInstallation = selfhost.Registered{InstallationID: "task35_install_" + f.suffix, NodeID: "task35_selfhost_" + f.suffix, RuntimeCredential: "task35-runtime-credential-" + f.suffix, NodeGeneration: 1}
		selfhostInstallation.EdgePool = selfhostInstallation.InstallationID
		encrypted, encryptErr := secrets.Encrypt("task35-selfhost-test-encryption-key", selfhostInstallation.RuntimeCredential)
		if encryptErr != nil {
			t.Fatal(encryptErr)
		}
		runtimeDigest := sha256.Sum256([]byte(selfhostInstallation.RuntimeCredential))
		fixtureExec := func(q string, args ...any) {
			t.Helper()
			if _, e := f.database.SQL().ExecContext(ctx, q, args...); e != nil {
				t.Fatal(e)
			}
		}
		fixtureExec(`INSERT INTO teams(team_id,owner_account,generation) VALUES($1,$2,1)`, teamID, f.accountID)
		for _, resource := range resources {
			fixtureExec(`INSERT INTO team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'admin',true)`, teamID, resource.accountID)
		}
		fixtureExec(`INSERT INTO control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch,state,region,failure_domain,roles,transports,capacity_limit,capacity_observed_at,registry_expires_at,tls_spki_sha256) VALUES($1,$2,'1.0','initial_fixture','registered','task35','task35-a',ARRAY['edge'],ARRAY['http2','http3'],100,now(),now()+interval '1 hour',$3)`, selfhostInstallation.NodeID, selfhostInstallation.InstallationID, make([]byte, 32))
		fixtureExec(`INSERT INTO selfhost_installations(id,node_id,installation_public_key,runtime_hash,runtime_ciphertext,name,capability,scope_kind,scope_id,claim_id,claim_complete) VALUES($1,$2,$3,$4,$5,'task35 selfhost edge','tunnel','team',$6,$1,true)`, selfhostInstallation.InstallationID, selfhostInstallation.NodeID, []byte(installationPublic), runtimeDigest[:], encrypted, teamID)
		cfg.Edges[0].NodeID = selfhostInstallation.NodeID
		cfg.Edges[1].NodeID += "_" + f.suffix
		for _, resource := range resources {
			if _, poolErr := selfhostService.SetPool(ctx, resource.accountID, "tunnel", selfhost.Policy{Mode: "mixed", InstallationIDs: []string{selfhostInstallation.InstallationID}}); poolErr != nil {
				t.Fatal(poolErr)
			}
		}
	}
	environmentID := "env_" + c.hostID
	routeID := "route_http_" + f.suffix
	defer func() {
		cleanup := func(query string, args ...any) {
			if _, err := f.database.SQL().ExecContext(context.Background(), query, args...); err != nil {
				t.Errorf("fixture cleanup: %v", err)
			}
		}
		for _, edge := range cfg.Edges {
			cleanup(`DELETE FROM tunnel_edge_route_assignments WHERE edge_node_id=$1`, edge.NodeID)
		}
		cleanup(`DELETE FROM tunnel_domains WHERE route_id=$1`, routeID)
		cleanup(`DELETE FROM tunnel_routes WHERE id=$1`, routeID)
		for _, route := range cfg.TLSRoutes {
			cleanup(`DELETE FROM tunnel_domains WHERE route_id=$1`, route.RouteID)
			cleanup(`DELETE FROM tunnel_routes WHERE id=$1`, route.RouteID)
		}
		for _, resource := range resources {
			cleanup(`DELETE FROM machine_control_sessions WHERE machine_id=$1`, resource.connectors[0].hostID)
			cleanup(`DELETE FROM control_environments WHERE id=$1`, "env_"+resource.connectors[0].hostID)
			cleanup(`DELETE FROM selfhost_pool_policies WHERE scope_kind='account' AND scope_id=$1`, resource.accountID)
		}
		cleanup(`DELETE FROM control_environments WHERE id=$1`, environmentID)
		if selfhostInstallation.InstallationID != "" {
			cleanup(`DELETE FROM selfhost_pool_policies WHERE scope_kind='account' AND scope_id=$1`, f.accountID)
			cleanup(`DELETE FROM selfhost_installations WHERE id=$1`, selfhostInstallation.InstallationID)
			cleanup(`DELETE FROM team_members WHERE team_id=$1`, teamID)
			cleanup(`DELETE FROM teams WHERE team_id=$1`, teamID)
		}
		for _, edge := range cfg.Edges {
			cleanup(`DELETE FROM control_tunnel_nodes WHERE id=$1`, edge.NodeID)
		}
	}()
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := f.database.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	// Run this external-process fixture in its own isolated database.
	for _, resource := range resources {
		exec(`INSERT INTO control_environments(id,workspace_id,owner_user_id,desired_state) VALUES($1,$2,$3,'active')`, "env_"+resource.connectors[0].hostID, resource.connectors[0].hostID, resource.accountID)
	}
	exec(`INSERT INTO tunnel_routes(id,tunnel_id,name,protocol,match_type,match_hostname,path_prefix,origin_scheme,origin_address,tls_verification,created_by_actor_id,updated_by_actor_id) VALUES($1,$2,'task35','http','exact',$3,'/','http',$4,'not_applicable',$5,$5)`, routeID, f.tunnelID, cfg.Hostname, cfg.OriginAddress, f.accountID)
	if cfg.Selfhost {
		exec(`INSERT INTO tunnel_domains(id,account_id,tunnel_id,route_id,hostname,match_type,ownership_challenge_reference,ownership_state,dns_target,certificate_strategy,certificate_state,caa_state,conflict_state) VALUES($1,$2,$3,$4,$5,'exact',$1,'verified',$5,'managed','ready','ready','clear')`, "domain_task35_"+f.suffix, f.accountID, f.tunnelID, routeID, cfg.Hostname)
	}
	if len(cfg.TLSRoutes) > 0 {
		if cfg.TLSListenerPort != 443 {
			t.Fatal("invalid TLS listener port")
		}
		for index, route := range cfg.TLSRoutes {
			if route.RouteID == "" || route.Hostname == "" || route.OriginAddress == "" {
				t.Fatal("invalid TLS route")
			}
			resource := resources[index]
			exec(`UPDATE tunnels SET stable_endpoint='https://'||$2 WHERE id=$1`, resource.tunnelID, route.Hostname)
			tlsRouteID := route.RouteID
			exec(`INSERT INTO tunnel_routes(id,tunnel_id,name,protocol,match_type,match_hostname,origin_scheme,origin_address,preserve_host,tls_verification,created_by_actor_id,updated_by_actor_id,public_tcp_listener_id,public_tcp_port) VALUES($1,$2,$1,'tls','managed',$3,'tcp',$4,false,'not_applicable',$5,$5,$6,$7)`, tlsRouteID, resource.tunnelID, route.Hostname, route.OriginAddress, resource.accountID, "listener_tls_443", cfg.TLSListenerPort)
		}
	}
	for _, resource := range resources {
		exec(`INSERT INTO machine_control_renewals(operation_id,machine_id,installation_generation,credential_jti,issued_at,expires_at,session_generation) SELECT $1,id,installation_generation,$1,$3,$4,1 FROM user_machines WHERE id=$2`, "renew_task35_"+resource.suffix, resource.connectors[0].hostID, f.clock.now.Add(-time.Minute), f.clock.now.Add(time.Hour))
		exec(`INSERT INTO machine_control_sessions(machine_id,installation_generation,session_generation,operation_id,credential_jti,issued_at,expires_at) SELECT machine_id,installation_generation,session_generation,operation_id,credential_jti,issued_at,expires_at FROM machine_control_renewals WHERE operation_id=$1`, "renew_task35_"+resource.suffix)
	}

	for i, edge := range cfg.Edges {
		if cfg.Selfhost && i == 0 {
			exec(`UPDATE control_tunnel_nodes SET protocol_version='1.0',process_epoch=$2,state='ready',ready=true,last_heartbeat_at=$3,region='task35',failure_domain=$1,roles=ARRAY['edge'],transports=ARRAY['http2'],capacity_limit=100,capacity_used=0,capacity_observed_at=$3,registry_expires_at=$3::timestamptz+interval '1 hour' WHERE id=$1`, edge.NodeID, edge.Epoch, f.clock.now)
		} else {
			exec(`INSERT INTO control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch,state,ready,last_heartbeat_at,region,failure_domain,roles,transports,capacity_limit,capacity_used,capacity_observed_at,registry_expires_at) VALUES($1,'task35','1.0',$2,'ready',true,$3,$4,$4,ARRAY['edge'],ARRAY['http2'],100,0,$3,$3::timestamptz+interval '1 hour')`, edge.NodeID, edge.Epoch, f.clock.now, edge.NodeID)
		}
		exec(`INSERT INTO tunnel_edge_route_assignments(assignment_id,route_id,assignment_generation,account_id,tunnel_id,connector_id,host_id,machine_identity_public_key,machine_identity_thumbprint,connector_generation,connector_session_id,connector_process_generation,config_generation,config_content_hash,access_mode,route_generation,route_revision,edge_node_id,edge_process_epoch,edge_failure_domain,state,observed_state)
SELECT $1,$2,$7,t.account_id,t.id,c.id,c.host_id,m.public_identity_key,$3,c.generation,c.last_session_id,1,1,g.content_hash,'public',1,1,$4,$5,$4,'active','ready' FROM tunnels t JOIN tunnel_connectors c ON c.tunnel_id=t.id JOIN user_machines m ON m.id=c.host_id JOIN tunnel_config_generations g ON g.tunnel_id=t.id AND g.generation=1 WHERE c.id=$6`, "assignment_task35_"+f.suffix+string(rune('a'+i)), routeID, c.oldThumbprint, edge.NodeID, edge.Epoch, c.id, i+1)
		for j, route := range cfg.TLSRoutes {
			tlsConnector := resources[j].connectors[0]
			exec(`INSERT INTO tunnel_edge_route_assignments(assignment_id,route_id,assignment_generation,account_id,tunnel_id,connector_id,host_id,machine_identity_public_key,machine_identity_thumbprint,connector_generation,connector_session_id,connector_process_generation,config_generation,config_content_hash,access_mode,route_generation,route_revision,edge_node_id,edge_process_epoch,edge_failure_domain,state,observed_state) SELECT $1,$2,$7,t.account_id,t.id,c.id,c.host_id,m.public_identity_key,$3,c.generation,c.last_session_id,1,1,g.content_hash,'public',1,1,$4,$5,$4,'active','ready' FROM tunnels t JOIN tunnel_connectors c ON c.tunnel_id=t.id JOIN user_machines m ON m.id=c.host_id JOIN tunnel_config_generations g ON g.tunnel_id=t.id AND g.generation=1 WHERE c.id=$6`, "assignment_task35_tls_"+f.suffix+string(rune('a'+i))+string(rune('a'+j)), route.RouteID, tlsConnector.oldThumbprint, edge.NodeID, edge.Epoch, tlsConnector.id, i+1)
		}
	}
	source := SQLIngressSource{DB: f.database, Clock: f.clock}
	write := func(path string, value any) error {
		encoded, err := json.Marshal(value)
		if err != nil {
			return err
		}
		temporary := path + ".incoming"
		if err := os.WriteFile(temporary, encoded, 0o600); err != nil {
			return err
		}
		return os.Rename(temporary, path)
	}
	ready := map[string]any{"schema": "paperboat.task35.authority-ready.v1", "edge_node_ids": []string{cfg.Edges[0].NodeID, cfg.Edges[1].NodeID}}
	if len(cfg.TLSRoutes) > 0 {
		ready["tls_route_id"] = cfg.TLSRoutes[0].RouteID
		ids := make([]string, len(cfg.TLSRoutes))
		for i, route := range cfg.TLSRoutes {
			ids[i] = route.RouteID
		}
		ready["tls_route_ids"] = ids
	}
	if err := write(cfg.ReadyPath, ready); err != nil {
		t.Fatal(err)
	}
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	pairingRevoked, pairingRestored := false, false
	initial := true
	selfhostOnly := false
	for {
		if _, err := os.Stat(cfg.StopPath); err == nil {
			return
		}
		if _, partitioned := os.Stat(cfg.PartitionPath); partitioned != nil {
			if cfg.Selfhost && !selfhostOnly {
				if _, switchErr := os.Stat(cfg.SelfhostOnlyPath); switchErr == nil {
					for _, resource := range resources {
						if _, err := selfhostService.SetPool(ctx, resource.accountID, "tunnel", selfhost.Policy{Mode: "self-hosted-only", InstallationIDs: []string{selfhostInstallation.InstallationID}}); err != nil {
							t.Fatal(err)
						}
					}
					selfhostOnly = true
				}
			}
			if cfg.Selfhost && cfg.RevokePairPath != "" {
				_, revoked := os.Stat(cfg.RevokePairPath)
				_, restored := os.Stat(cfg.RestorePairPath)
				if revoked == nil && restored != nil && !pairingRevoked {
					for _, resource := range resources {
						if _, err := selfhostService.SetPool(ctx, resource.accountID, "tunnel", selfhost.Policy{Mode: "mixed", InstallationIDs: []string{}}); err != nil {
							t.Fatal(err)
						}
					}
					pairingRevoked = true
				} else if restored == nil && !pairingRestored {
					for _, resource := range resources {
						mode := "mixed"
						if selfhostOnly {
							mode = "self-hosted-only"
						}
						if _, err := selfhostService.SetPool(ctx, resource.accountID, "tunnel", selfhost.Policy{Mode: mode, InstallationIDs: []string{selfhostInstallation.InstallationID}}); err != nil {
							t.Fatal(err)
						}
					}
					exec(`UPDATE tunnel_edge_route_assignments a SET state='active',observed_state='ready',assignment_generation=(SELECT coalesce(max(other.assignment_generation),0)+1 FROM tunnel_edge_route_assignments other WHERE other.route_id=a.route_id),updated_at=now() WHERE a.edge_node_id=$1 AND a.state='draining'`, selfhostInstallation.NodeID)
					pairingRestored = true
				}
			}
			f.clock.now = time.Now().UTC()
			for _, resource := range resources {
				exec(`UPDATE tunnel_connector_sessions SET last_heartbeat_at=$2,lease_deadline=$2::timestamptz+interval '1 hour' WHERE id=$1`, resource.connectors[0].oldSessionID, f.clock.now)
			}
			for _, edge := range cfg.Edges {
				disabled := false
				if edge == cfg.Edges[0] && cfg.DisableEdgePath != "" {
					_, disabledErr := os.Stat(cfg.DisableEdgePath)
					_, restoreErr := os.Stat(cfg.RestoreEdgePath)
					disabled = disabledErr == nil && restoreErr != nil
				}
				exec(`UPDATE control_tunnel_nodes SET state=CASE WHEN $3 THEN 'draining' ELSE 'ready' END,ready=NOT $3,last_heartbeat_at=$2,capacity_observed_at=$2,registry_expires_at=$2::timestamptz+interval '1 hour' WHERE id=$1`, edge.NodeID, f.clock.now, disabled)
				for _, route := range cfg.TLSRoutes {
					removed := false
					if cfg.RemoveTLSPath != "" {
						_, removeErr := os.Stat(cfg.RemoveTLSPath)
						removed = removeErr == nil
					}
					exec(`UPDATE tunnel_routes SET desired_state=CASE WHEN $2 THEN 'deleted' ELSE 'active' END,deleted_at=CASE WHEN $2 THEN $3::timestamptz ELSE NULL END WHERE id=$1`, route.RouteID, removed, f.clock.now)
				}
				decisions, err := source.Edge(ctx, edge.NodeID, edge.Epoch)
				if err != nil {
					t.Fatalf("edge projection %s err=%v", edge.NodeID, err)
				}
				var httpDecision *IngressDecision
				tlsDecisions := make([]IngressDecision, 0)
				for i := range decisions {
					if decisions[i].Binding.Protocol == "http" {
						value := decisions[i]
						httpDecision = &value
					} else if decisions[i].Binding.Protocol == "tls" {
						tlsDecisions = append(tlsDecisions, decisions[i])
					}
				}
				if initial && len(tlsDecisions) != len(cfg.TLSRoutes) {
					t.Fatalf("initial TLS projection got %d decisions, want %d", len(tlsDecisions), len(cfg.TLSRoutes))
				}
				expectedEmpty := cfg.Selfhost && selfhostOnly && (edge.NodeID != selfhostInstallation.NodeID || pairingRevoked && !pairingRestored)
				if !disabled && httpDecision == nil && !expectedEmpty {
					t.Fatalf("edge projection %s missing HTTP", edge.NodeID)
				}
				if err := write(edge.Path, map[string]any{"HTTP": httpDecision, "TLS": tlsDecisions}); err != nil {
					t.Fatal(err)
				}
			}
		}
		initial = false
		select {
		case <-time.After(100 * time.Millisecond):
		case <-ticker.C:
		}
	}
}

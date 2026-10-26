package tunnelv1

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// The live facts behind tunnel status are read with SQL the service fake cannot exercise.
func TestHealthFactsReadDomainAndOriginStateOnPostgres(t *testing.T) {
	f := newTRK07PostgresFixture(t)
	ctx := context.Background()
	account, host := f.addAccount(t, "health-facts")
	tunnel := "tun_health_" + f.suffix
	f.createTunnel(t, account, host, tunnel, "health-"+f.suffix)
	var route string
	if err := f.database.SQL().QueryRowContext(ctx, `SELECT id FROM paperboat.tunnel_routes WHERE tunnel_id=$1 AND deleted_at IS NULL ORDER BY id LIMIT 1`, tunnel).Scan(&route); err != nil {
		t.Fatal(err)
	}
	now := f.now
	facts, err := f.repository.HealthFacts(ctx, account, tunnel, now)
	if err != nil {
		t.Fatal(err)
	}
	if facts.HostConnected || facts.EngineServed || facts.OriginReason != "" || len(facts.PendingDomains) != 0 || len(facts.PendingCertificates) != 0 {
		t.Fatalf("a fresh tunnel reports live facts: %+v", facts)
	}
	insertDomain := func(id, hostname, ownership, certificate string) {
		if _, err := f.database.SQL().ExecContext(ctx, `
INSERT INTO paperboat.tunnel_domains (id,account_id,tunnel_id,route_id,hostname,match_type,ownership_challenge_reference,dns_target,dns_provider,expected_records,dns_next_check_at,created_at,updated_at,ownership_state,certificate_state)
VALUES ($1,$2,$3,$4,$5,'exact','dns-challenge://test','stable.example.test','generic','[]'::jsonb,$6,$6,$6,$7,$8)`, id, account, tunnel, route, hostname, now, ownership, certificate); err != nil {
			t.Fatal(err)
		}
	}
	insertDomain("dom_pending_"+f.suffix, "pending-"+f.suffix+".example.test", "pending", "pending")
	insertDomain("dom_cert_"+f.suffix, "cert-"+f.suffix+".example.test", "verified", "issuing")
	insertDomain("dom_ready_"+f.suffix, "ready-"+f.suffix+".example.test", "verified", "ready")
	facts, err = f.repository.HealthFacts(ctx, account, tunnel, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(facts.PendingDomains) != 1 || facts.PendingDomains[0] != "pending-"+f.suffix+".example.test" || len(facts.PendingCertificates) != 1 || facts.PendingCertificates[0] != "cert-"+f.suffix+".example.test" {
		t.Fatalf("domain facts = %+v", facts)
	}
	// The tunnel's endpoint on the host's agent: connected once the agent holds a live carrier, served
	// once an edge observes the route ready.
	node, epoch, carrierRoute := "node_health_"+f.suffix, "epoch_health_"+f.suffix, "runtime_health_"+f.suffix
	session := fmt.Sprintf("session_00000000-0000-4000-8000-%012d", now.UnixNano()%1e12)
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := f.database.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_, _ = f.database.SQL().ExecContext(ctx, `DELETE FROM paperboat.control_routes WHERE id=$1`, carrierRoute)
		_, _ = f.database.SQL().ExecContext(ctx, `DELETE FROM paperboat.control_tunnel_nodes WHERE id=$1`, node)
		_, _ = f.database.SQL().ExecContext(ctx, `DELETE FROM paperboat.control_environments WHERE id=$1`, "env_"+host)
	})
	exec(`INSERT INTO paperboat.control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch,state,ready,last_heartbeat_at,roles,registry_expires_at,region,failure_domain,transports,capacity_limit,capacity_used,capacity_observed_at) VALUES($1,'test','1.0',$2,'ready',true,$3,ARRAY['edge'],$3::timestamptz+interval '1 hour','test','test',ARRAY['http3','http2'],100,0,$3)`, node, epoch, now)
	exec(`INSERT INTO paperboat.control_environments(id,workspace_id,owner_user_id,desired_state) SELECT environment_id,environment_id,user_id,'active' FROM paperboat.user_machines WHERE id=$1 ON CONFLICT DO NOTHING`, host)
	exec(`INSERT INTO paperboat.control_routes(id,environment_id,kind,public_host,target_host,target_port) SELECT $1,environment_id,'runtime_https_wss',$1||'.runtime.example.test','127.0.0.1',38080 FROM paperboat.user_machines WHERE id=$2`, carrierRoute, host)
	exec(`INSERT INTO paperboat.endpoints(id,kind,account_id,agent_machine_id,access_mode,lifetime,tunnel_id) VALUES($1,'tunnel',$2,$3,'private','persistent',$1)`, tunnel, account, host)
	exec(`INSERT INTO paperboat.endpoint_routes(id,endpoint_id,hostname,protocol,tunnel_route_id) VALUES($1,$2,$3,'http',$1)`, route, tunnel, "engine-"+strings.ReplaceAll(f.suffix, "_", "-")+".example.test")
	facts, err = f.repository.HealthFacts(ctx, account, tunnel, now)
	if err != nil || !facts.EdgeReady || facts.HostConnected || facts.EngineServed {
		t.Fatalf("a tunnel whose agent holds no carrier is reported connected: %+v err=%v", facts, err)
	}
	exec(`INSERT INTO paperboat.runtime_carrier_attachments(route_id,machine_id,account_id,installation_generation,worker_generation,process_id,generation,edge_node_id,edge_process_epoch,public_identity_key,route_revision,session_id,preview_session_id,admission,expires_at,active) SELECT $1,m.id,m.user_id,m.installation_generation,1,'process_health',1,$2,$3,m.public_identity_key,1,$4,$4,'{}',$5::timestamptz+interval '1 hour',true FROM paperboat.user_machines m WHERE m.id=$6`, carrierRoute, node, epoch, session, now, host)
	facts, err = f.repository.HealthFacts(ctx, account, tunnel, now.Add(5*time.Second))
	if err != nil || !facts.HostConnected || facts.EngineServed {
		t.Fatalf("a connected agent is not reported online: %+v err=%v", facts, err)
	}
	facts, err = f.repository.HealthFacts(ctx, account, tunnel, now.Add(2*time.Hour))
	if err != nil || facts.HostConnected {
		t.Fatalf("an agent whose carrier expired still counts as online: %+v err=%v", facts, err)
	}
	exec(`INSERT INTO paperboat.endpoint_edge_assignments(route_id,endpoint_id,edge_node_id,edge_process_epoch,carrier_session_id,observed_ready,observed_at) VALUES($1,$2,$3,$4,$5,true,$6)`, route, tunnel, node, epoch, session, now)
	facts, err = f.repository.HealthFacts(ctx, account, tunnel, now)
	if err != nil || !facts.EngineServed {
		t.Fatalf("a route an edge observes ready is not reported served: %+v err=%v", facts, err)
	}
	if _, err := f.database.SQL().ExecContext(ctx, `INSERT INTO paperboat.tunnel_route_origin_health(route_id,machine_id,tunnel_id,state,code,observed_at) VALUES($1,$2,$3,'unreachable','refused',$4)`, route, host, tunnel, now.Add(-10*time.Second)); err != nil {
		t.Fatal(err)
	}
	facts, err = f.repository.HealthFacts(ctx, account, tunnel, now)
	if err != nil || facts.OriginReason != "refused" {
		t.Fatalf("origin fact = %+v err=%v", facts, err)
	}
	facts, err = f.repository.HealthFacts(ctx, account, tunnel, now.Add(OriginHealthMaxAge+time.Second))
	if err != nil || facts.OriginReason != "" {
		t.Fatalf("an old origin report still counts: %+v err=%v", facts, err)
	}
}

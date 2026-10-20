package tunnelv1

import (
	"context"
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
	if facts.ConnectorSessions != 0 || facts.FailedRoutes != 0 || facts.OriginReason != "" || len(facts.PendingDomains) != 0 || len(facts.PendingCertificates) != 0 {
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
	connector, session := "con_health_"+f.suffix, "ses_health_"+f.suffix
	if _, err := f.database.SQL().ExecContext(ctx, `INSERT INTO paperboat.tunnel_connectors (id,tunnel_id,host_id,credential_reference,credential_thumbprint,rotation_generation,desired_state,protocol_version,last_session_id,last_heartbeat_at,ready_at,last_applied_config_generation,drain_state,generation,created_at,updated_at)
VALUES ($1,$2,$3,$4,$4,1,'active','1.0',$5,$6,$6,1,'accepting',1,$6,$6)`, connector, tunnel, host, "keychain://health/"+connector, session, now); err != nil {
		t.Fatal(err)
	}
	if _, err := f.database.SQL().ExecContext(ctx, `INSERT INTO paperboat.tunnel_connector_sessions (id,connector_id,process_generation,protocol_version,capabilities,credential_generation,state,lease_deadline,last_heartbeat_at,ready_at,applied_config_generation,retained_until,created_at)
VALUES ($1,$2,1,'1.0',ARRAY['config.snapshot.v1'],1,'ready',$3,$4,$4,1,$5,$4)`, session, connector, now.Add(time.Hour), now, now.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	facts, err = f.repository.HealthFacts(ctx, account, tunnel, now.Add(5*time.Second))
	if err != nil || facts.ActiveConnectors != 1 || facts.ConnectorSessions != 1 {
		t.Fatalf("a connected host is not reported online: %+v err=%v", facts, err)
	}
	facts, err = f.repository.HealthFacts(ctx, account, tunnel, now.Add(2*time.Minute))
	if err != nil || facts.ActiveConnectors != 1 || facts.ConnectorSessions != 0 {
		t.Fatalf("a host that stopped heartbeating still counts as online: %+v err=%v", facts, err)
	}
	if _, err := f.database.SQL().ExecContext(ctx, `INSERT INTO paperboat.tunnel_route_origin_health(route_id,connector_id,tunnel_id,session_id,state,code,observed_at) VALUES($1,$2,$3,'ses_1','unreachable','refused',$4)`, route, connector, tunnel, now.Add(-10*time.Second)); err != nil {
		t.Fatal(err)
	}
	facts, err = f.repository.HealthFacts(ctx, account, tunnel, now)
	if err != nil || facts.OriginReason != "refused" || facts.ActiveConnectors != 1 {
		t.Fatalf("origin fact = %+v err=%v", facts, err)
	}
	facts, err = f.repository.HealthFacts(ctx, account, tunnel, now.Add(OriginHealthMaxAge+time.Second))
	if err != nil || facts.OriginReason != "" {
		t.Fatalf("an old origin report still counts: %+v err=%v", facts, err)
	}
}

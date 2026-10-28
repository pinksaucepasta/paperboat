package selfhost

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/testutil"
)

func TestSelfhostHTTPTopologyPolicyOnPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("requires isolated PostgreSQL")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	ctx := context.Background()
	if err = db.Migrate(ctx, database); err != nil {
		t.Fatal(err)
	}
	suffix := randomToken(8)
	account, other := "top_a_"+suffix, "top_b_"+suffix
	admin, member, team := "top_admin_"+suffix, "top_member_"+suffix, "top_team_"+suffix
	for _, id := range []string{account, other, admin, member} {
		if _, err = database.SQL().ExecContext(ctx, `INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$2,'active')`, id, id+"@example.test"); err != nil {
			t.Fatal(err)
		}
	}
	service := New(database, "selfhost-topology-test-encryption-key")
	if _, err = database.SQL().ExecContext(ctx, `INSERT INTO paperboat.tunnel_platform_certificate_targets(id,kind,hostname,account_id,challenge_reference) VALUES('platform_cert_public_v1','public_wildcard','*.managed.example.test','platform_system_v1','dns:topology-test')`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := database.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.tunnel_platform_certificate_targets WHERE id='platform_cert_public_v1'`); err != nil {
			t.Errorf("platform target cleanup: %v", err)
		}
	})
	for hostname, expected := range map[string]bool{"managed.example.test": false, "one.managed.example.test": false, "nested.one.managed.example.test": false, "notmanaged.example.test": true, "customer.example.test": true} {
		var allowed bool
		if err := database.SQL().QueryRowContext(ctx, `SELECT paperboat.selfhost_publication_hostname_allowed($1)`, hostname).Scan(&allowed); err != nil || allowed != expected {
			t.Fatalf("reserved namespace %s allowed=%v want=%v: %v", hostname, allowed, expected, err)
		}
	}
	installed := scopedInstallationFixture(t, database, "selfhost-topology-test-encryption-key", account, "tunnel")
	t.Cleanup(func() {
		statements := []struct {
			query string
			args  []any
		}{
			{`DELETE FROM paperboat.preview_domains WHERE account_id=$1`, []any{account}},
			{`DELETE FROM paperboat.preview_leases WHERE account_id=$1`, []any{account}},
			{`DELETE FROM paperboat.team_resource_bindings WHERE team_id=$1`, []any{team}},
			{`DELETE FROM paperboat.tunnels WHERE account_id=$1`, []any{account}},
			{`DELETE FROM paperboat.selfhost_pool_policies WHERE scope_kind='account' AND scope_id IN ($1,$2)`, []any{account, other}},
			{`DELETE FROM paperboat.selfhost_pool_policies WHERE scope_kind='team' AND scope_id=$1`, []any{team}},
			{`DELETE FROM paperboat.team_members WHERE team_id=$1`, []any{team}},
			{`DELETE FROM paperboat.teams WHERE team_id=$1`, []any{team}},
			{`DELETE FROM paperboat.selfhost_installations WHERE id=$1`, []any{installed.InstallationID}},
			{`DELETE FROM paperboat.control_tunnel_nodes WHERE id=$1`, []any{installed.NodeID}},
			{`DELETE FROM paperboat.users WHERE id IN ($1,$2,$3,$4)`, []any{account, other, admin, member}},
		}
		for _, statement := range statements {
			if _, cleanupErr := database.SQL().ExecContext(context.Background(), statement.query, statement.args...); cleanupErr != nil {
				t.Errorf("cleanup failed: %v", cleanupErr)
			}
		}
	})
	if _, err = database.SQL().ExecContext(ctx, `INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$2,1)`, team, account); err != nil {
		t.Fatal(err)
	}
	if _, err = database.SQL().ExecContext(ctx, `INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'member',true),($1,$3,1,'admin',true),($1,$4,1,'member',true)`, team, account, admin, member); err != nil {
		t.Fatal(err)
	}
	teamInstalled := scopedInstallationFixture(t, database, "selfhost-topology-test-encryption-key", account, "tunnel")
	if _, err = database.SQL().ExecContext(ctx, `UPDATE paperboat.selfhost_installations SET scope_kind='team',scope_id=$2 WHERE id=$1`, teamInstalled.InstallationID, team); err != nil {
		t.Fatal(err)
	}
	teamScope := Scope{Kind: "team", ID: team}
	teamPool := Policy{Mode: "self-hosted-only", InstallationIDs: []string{teamInstalled.InstallationID}}
	for _, actor := range []string{account, admin} {
		if _, err = service.SetPoolScoped(ctx, actor, "tunnel", teamPool, teamScope); err != nil {
			t.Fatalf("team pool write by %s: %v", actor, err)
		}
	}
	for _, actor := range []string{member} {
		got, readErr := service.PoolScoped(ctx, actor, "tunnel", teamScope)
		if readErr != nil || got.Mode != teamPool.Mode || len(got.InstallationIDs) != 1 || got.InstallationIDs[0] != teamInstalled.InstallationID {
			t.Fatalf("team member could not read team pool: %+v %v", got, readErr)
		}
		if _, writeErr := service.SetPoolScoped(ctx, actor, "tunnel", teamPool, teamScope); !errors.Is(writeErr, ErrUnauthorized) {
			t.Fatalf("team member changed team pool: %v", writeErr)
		}
	}
	for _, operation := range []struct {
		name string
		call func() error
	}{
		{"read", func() error { _, err := service.PoolScoped(ctx, other, "tunnel", teamScope); return err }},
		{"write", func() error { _, err := service.SetPoolScoped(ctx, other, "tunnel", teamPool, teamScope); return err }},
	} {
		if err = operation.call(); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("team nonmember %s was authorized: %v", operation.name, err)
		}
	}
	if _, err = service.SetPool(ctx, account, "tunnel", Policy{"mixed", []string{installed.InstallationID}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("mixed edges accepted: %v", err)
	}
	if _, err = service.SetPool(ctx, account, "tunnel", Policy{"managed-only", []string{installed.InstallationID}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("managed selected private nodes: %v", err)
	}
	var before bool
	if err = database.SQL().QueryRowContext(ctx, `SELECT paperboat.node_pool_eligible($1,$2,'tunnel')`, installed.NodeID, account).Scan(&before); err != nil || before {
		t.Fatalf("default managed used private node: %v %v", before, err)
	}
	if _, err = service.SetPool(ctx, account, "tunnel", Policy{"self-hosted-only", []string{installed.InstallationID}}); err != nil {
		t.Fatalf("scoped pool rejected: %v", err)
	}
	personalPool, err := service.PoolScoped(ctx, account, "tunnel", Scope{Kind: "account", ID: account})
	if err != nil || personalPool.Mode != "self-hosted-only" || len(personalPool.InstallationIDs) != 1 || personalPool.InstallationIDs[0] != installed.InstallationID {
		t.Fatalf("Personal tunnel pool was replaced by Team policy: %+v %v", personalPool, err)
	}
	teamPoolRead, err := service.PoolScoped(ctx, account, "tunnel", teamScope)
	if err != nil || teamPoolRead.Mode != "self-hosted-only" || len(teamPoolRead.InstallationIDs) != 1 || teamPoolRead.InstallationIDs[0] != teamInstalled.InstallationID {
		t.Fatalf("Team tunnel pool was replaced by Personal policy: %+v %v", teamPoolRead, err)
	}
	if _, err = database.SQL().ExecContext(ctx, `UPDATE paperboat.control_tunnel_nodes SET ready=true,state='ready',process_epoch='topology_epoch',last_heartbeat_at=now(),registry_expires_at=now()+interval '1 hour',public_ingress_verified_at=now(),public_ingress_ipv4='203.0.113.1' WHERE id=$1`, installed.NodeID); err != nil {
		t.Fatal(err)
	}
	tunnel, route := "top_t_"+suffix, "top_r_"+suffix
	endpointID := testutil.EndpointID("topology:" + suffix)
	_, err = database.SQL().ExecContext(ctx, `INSERT INTO paperboat.tunnels(id,account_id,name,stable_endpoint_id,stable_endpoint,created_by_host_id,created_by_actor_id) VALUES($1,$2,$1,$3,$4,$5,'actor')`, tunnel, account, endpointID, "https://"+testutil.EndpointLabel(endpointID)+".example.test", testutil.PublicationMachine(t, database.SQL(), account))
	if err != nil {
		t.Fatal(err)
	}
	_, err = database.SQL().ExecContext(ctx, `INSERT INTO paperboat.tunnel_routes(id,tunnel_id,name,protocol,match_type,match_hostname,origin_scheme,origin_address,created_by_actor_id,updated_by_actor_id) VALUES($1,$2,$1,'http','managed','managed.example.test','http','127.0.0.1:3000','actor','actor')`, route, tunnel)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.SetPool(ctx, account, "tunnel", Policy{"managed-only", []string{}}); !errors.Is(err, ErrLiveEdgeModeChange) {
		t.Fatalf("implicit live ownership switch accepted: %v", err)
	}
	if _, err = service.SetPool(ctx, account, "relay", Policy{"mixed", []string{}}); err != nil {
		t.Fatalf("relay policy affected: %v", err)
	}
	if _, err = service.SetPool(ctx, other, "tunnel", Policy{"managed-only", []string{}}); err != nil {
		t.Fatalf("other account affected: %v", err)
	}
	var managedEligible bool
	if err = database.SQL().QueryRowContext(ctx, `SELECT paperboat.node_route_eligible($1,$2,$3)`, installed.NodeID, account, route).Scan(&managedEligible); err != nil || managedEligible {
		t.Fatalf("managed route eligible=%v err=%v", managedEligible, err)
	}
	_, err = database.SQL().ExecContext(ctx, `INSERT INTO paperboat.tunnel_domains(id,account_id,tunnel_id,route_id,hostname,match_type,ownership_challenge_reference,ownership_state,dns_target,certificate_strategy,certificate_state,caa_state,conflict_state) VALUES($1,$2,$3,$4,$5,'exact','dns:test','verified','target.example.test','managed','ready','ready','clear')`, `top_d_`+suffix, account, tunnel, route, "custom-"+testutil.EndpointLabel(endpointID)+".example.test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = database.SQL().ExecContext(ctx, `UPDATE paperboat.tunnel_domains SET expected_records='[{"name":"test","type":"A","value":"203.0.113.1","ttl":300}]' WHERE account_id=$1`, account); err != nil {
		t.Fatal(err)
	}
	if _, err = service.SetPool(ctx, account, "tunnel", Policy{"self-hosted-only", []string{installed.InstallationID}}); err != nil {
		t.Fatalf("verified route rejected: %v", err)
	}
	var eligible bool
	if err = database.SQL().QueryRowContext(ctx, `SELECT paperboat.node_route_eligible($1,$2,$3)`, installed.NodeID, account, route).Scan(&eligible); err != nil || !eligible {
		t.Fatalf("route eligible=%v err=%v", eligible, err)
	}
	if err = database.SQL().QueryRowContext(ctx, `SELECT paperboat.node_route_eligible($1,$2,$3)`, installed.NodeID, other, route).Scan(&eligible); err != nil || eligible {
		t.Fatalf("cross-account route eligible=%v err=%v", eligible, err)
	}
	preview := "top_p_" + suffix
	previewEndpointID := testutil.EndpointLabel("topology-preview:" + suffix)
	_, err = database.SQL().ExecContext(ctx, `INSERT INTO paperboat.preview_leases(owner_session_kind,id,endpoint,account_id,actor_id,owner_machine_id,owner_session_id,target_scheme,target_address,lease_deadline) VALUES('foreground',$1,$3,$2,'actor',$4,'session','http','127.0.0.1:3000',now()+interval '10 minutes')`, preview, account, fmt.Sprintf("https://%s.preview.example.test", previewEndpointID), testutil.PublicationMachine(t, database.SQL(), account))
	if err != nil {
		t.Fatal(err)
	}
	if err = database.SQL().QueryRowContext(ctx, `SELECT paperboat.node_preview_eligible($1,$2,$3)`, installed.NodeID, account, preview).Scan(&eligible); err != nil || eligible {
		t.Fatalf("managed preview eligible=%v err=%v", eligible, err)
	}
	_, err = database.SQL().ExecContext(ctx, `INSERT INTO paperboat.preview_domains(id,account_id,preview_id,preview_generation,hostname,match_type,ownership_challenge_reference,ownership_state,dns_target,certificate_strategy,certificate_state,caa_state,conflict_state) VALUES($1,$2,$3,1,$4,'exact','dns:test','verified','target.example.test','managed','ready','ready','clear')`, `top_pd_`+suffix, account, preview, "custom-preview-"+previewEndpointID+".example.test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = database.SQL().ExecContext(ctx, `UPDATE paperboat.preview_domains SET expected_records='[{"name":"test","type":"A","value":"203.0.113.1","ttl":300}]' WHERE account_id=$1`, account); err != nil {
		t.Fatal(err)
	}
	if err = database.SQL().QueryRowContext(ctx, `SELECT paperboat.node_preview_eligible($1,$2,$3)`, installed.NodeID, account, preview).Scan(&eligible); err != nil || !eligible {
		t.Fatalf("verified preview eligible=%v err=%v", eligible, err)
	}
	ids := []string{installed.InstallationID}
	for index := 0; index < 3; index++ {
		extra := scopedInstallationFixture(t, database, "selfhost-topology-test-encryption-key", account, "tunnel")
		ids = append(ids, extra.InstallationID)
		if _, err = database.SQL().ExecContext(ctx, `UPDATE paperboat.control_tunnel_nodes SET ready=true,state='ready',process_epoch='topology_epoch',failure_domain=$2,last_heartbeat_at=now(),registry_expires_at=now()+interval '1 hour',public_ingress_verified_at=now(),public_ingress_ipv4=$3::inet WHERE id=$1`, extra.NodeID, fmt.Sprintf("extra-%d", index), fmt.Sprintf("203.0.113.%d", index+2)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = service.SetPool(ctx, account, "tunnel", Policy{"self-hosted-only", ids}); err != nil {
		t.Fatalf("more than two enrolled eligible edges rejected: %v", err)
	}
	var used int
	if err = database.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.publication_edge_nodes('tunnel',$1)`, tunnel).Scan(&used); err != nil || used != 2 {
		t.Fatalf("publication selected %d edges: %v", used, err)
	}
	if _, err = database.SQL().ExecContext(ctx, `UPDATE paperboat.control_tunnel_nodes SET ready=false WHERE id IN(SELECT node_id FROM paperboat.selfhost_installations WHERE id=ANY($1))`, ids); err != nil {
		t.Fatal(err)
	}
	if err = database.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.publication_edge_nodes('tunnel',$1)`, tunnel).Scan(&used); err != nil || used != 0 {
		t.Fatalf("outage used foreign fallback: %d %v", used, err)
	}
	if _, err = database.SQL().ExecContext(ctx, `UPDATE paperboat.control_tunnel_nodes SET ready=true,state='ready',process_epoch='team_topology_epoch',last_heartbeat_at=now(),registry_expires_at=now()+interval '1 hour',public_ingress_verified_at=now(),public_ingress_ipv4='203.0.113.50' WHERE id=$1`, teamInstalled.NodeID); err != nil {
		t.Fatal(err)
	}
	teamTunnel, teamRoute := "top_team_t_"+suffix, "top_team_r_"+suffix
	teamEndpointID := testutil.EndpointID("topology-team:" + suffix)
	if _, err = database.SQL().ExecContext(ctx, `INSERT INTO paperboat.tunnels(id,account_id,name,stable_endpoint_id,stable_endpoint,created_by_host_id,created_by_actor_id,owner_team_id) VALUES($1,$2,$1,$3,$4,$6,'actor',$5)`, teamTunnel, account, teamEndpointID, "https://"+testutil.EndpointLabel(teamEndpointID)+".example.test", team, testutil.PublicationMachine(t, database.SQL(), account)); err != nil {
		t.Fatal(err)
	}
	if _, err = database.SQL().ExecContext(ctx, `INSERT INTO paperboat.team_resource_bindings(team_id,resource_kind,resource_id,owner_account,active,generation) VALUES($1,'tunnel',$2,$3,true,1)`, team, teamTunnel, account); err != nil {
		t.Fatal(err)
	}
	if _, err = database.SQL().ExecContext(ctx, `INSERT INTO paperboat.tunnel_routes(id,tunnel_id,name,protocol,match_type,match_hostname,origin_scheme,origin_address,created_by_actor_id,updated_by_actor_id) VALUES($1,$2,$1,'http','managed','managed.example.test','http','127.0.0.1:3000','actor','actor')`, teamRoute, teamTunnel); err != nil {
		t.Fatal(err)
	}
	if _, err = database.SQL().ExecContext(ctx, `INSERT INTO paperboat.tunnel_domains(id,account_id,tunnel_id,route_id,hostname,match_type,ownership_challenge_reference,ownership_state,dns_target,certificate_strategy,certificate_state,caa_state,conflict_state,expected_records) VALUES($1,$2,$3,$4,$5,'exact','dns:test','verified','203.0.113.50','managed','ready','ready','clear','[{"name":"test","type":"A","value":"203.0.113.50","ttl":300}]')`, "top_team_d_"+suffix, account, teamTunnel, teamRoute, "custom-team-"+testutil.EndpointLabel(teamEndpointID)+".example.test"); err != nil {
		t.Fatal(err)
	}
	if _, err = service.SetPlacement(ctx, account, "tunnel", teamTunnel, nil); !errors.Is(err, ErrPlacementMode) {
		t.Fatalf("self-hosted-only Team accepted managed placement: %v", err)
	}
	if _, err = service.SetPlacement(ctx, account, "tunnel", teamTunnel, &installed.InstallationID); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("Team accepted a Personal installation placement: %v", err)
	}
	placed, err := service.SetPlacement(ctx, account, "tunnel", teamTunnel, &teamInstalled.InstallationID)
	if err != nil || placed.NodeID == nil || *placed.NodeID != teamInstalled.NodeID {
		t.Fatalf("Team self-hosted placement: %+v %v", placed, err)
	}
	t.Cleanup(func() {
		if _, cleanupErr := database.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.selfhost_placements WHERE resource_kind='tunnel' AND resource_id=$1`, teamTunnel); cleanupErr != nil {
			t.Errorf("team placement cleanup failed: %v", cleanupErr)
		}
	})
	if _, err = database.SQL().ExecContext(ctx, `UPDATE paperboat.tunnel_domains SET ownership_state='verified',conflict_state='clear',expected_records='[{"name":"test","type":"A","value":"203.0.113.50","ttl":300}]' WHERE tunnel_id=$1`, teamTunnel); err != nil {
		t.Fatal(err)
	}
	var teamEligible, personalEligible bool
	if err = database.SQL().QueryRowContext(ctx, `SELECT paperboat.node_route_eligible($1,$2,$3)`, teamInstalled.NodeID, account, teamRoute).Scan(&teamEligible); err != nil || !teamEligible {
		t.Fatalf("Team self-hosted route eligibility=%v: %v", teamEligible, err)
	}
	if err = database.SQL().QueryRowContext(ctx, `SELECT paperboat.node_route_eligible($1,$2,$3)`, installed.NodeID, account, teamRoute).Scan(&personalEligible); err != nil || personalEligible {
		t.Fatalf("Team placement crossed into Personal pool: %v %v", personalEligible, err)
	}

	// A terminal preview cannot block an explicit policy switch just because its
	// original publication deadline is still in the future.
	if _, err = database.SQL().ExecContext(ctx, `UPDATE paperboat.tunnels SET desired_state='paused' WHERE id=$1`, tunnel); err != nil {
		t.Fatal(err)
	}
	if _, err = service.SetPool(ctx, account, "tunnel", Policy{"managed-only", []string{}}); !errors.Is(err, ErrLiveEdgeModeChange) {
		t.Fatalf("active preview did not fence mode switch: %v", err)
	}
	for _, terminal := range []string{"owner_lost", "failed"} {
		if _, err = database.SQL().ExecContext(ctx, `UPDATE paperboat.preview_leases SET terminal_state=$2,stopped_at=now() WHERE id=$1`, preview, terminal); err != nil {
			t.Fatal(err)
		}
		if _, err = service.SetPool(ctx, account, "tunnel", Policy{"managed-only", []string{}}); err != nil {
			t.Fatalf("future-deadline %s preview blocked mode switch: %v", terminal, err)
		}
		if _, err = service.SetPool(ctx, account, "tunnel", Policy{"self-hosted-only", ids}); err != nil {
			t.Fatalf("mode restore after %s preview failed: %v", terminal, err)
		}
	}

}

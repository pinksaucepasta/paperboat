package connectorprotocol

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/billing"
)

func TestTask24IngressProjectionPostgres(t *testing.T) { testIngressProjectionPostgres(t, false) }
func TestIngressServingGenerationSurvivesAppliedCandidatePostgres(t *testing.T) {
	testIngressProjectionPostgres(t, true)
}
func testIngressProjectionPostgres(t *testing.T, onlyLKG bool) {
	f := newTRK08PostgresFixture(t)
	ctx := context.Background()
	c := f.connectors[0]
	now := f.clock.now
	nodeID := "edge_task24_" + f.suffix
	epoch := "epoch_task24_" + f.suffix
	routeID := "route_task24_" + f.suffix
	environmentID := "env_" + c.hostID
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := f.database.SQL().ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	// Clean foreign-key owners in order, even when an assertion fails. The
	// existing fixture subsequently deletes the account and closes its database.
	t.Cleanup(func() {
		for _, q := range []string{
			`DELETE FROM tunnel_edge_route_assignments WHERE edge_node_id=$1`,
			`DELETE FROM control_tunnel_nodes WHERE id=$1`,
		} {
			if _, err := f.database.SQL().ExecContext(ctx, q, nodeID); err != nil {
				t.Error(err)
			}
		}
		if _, err := f.database.SQL().ExecContext(ctx, `DELETE FROM billing_workspace_usage_periods WHERE workspace_id IN (SELECT id FROM billing_workspaces WHERE user_id=$1)`, f.accountID); err != nil {
			t.Error(err)
		}
		if _, err := f.database.SQL().ExecContext(ctx, `DELETE FROM billing_workspaces WHERE user_id=$1`, f.accountID); err != nil {
			t.Error(err)
		}
		if _, err := f.database.SQL().ExecContext(ctx, `DELETE FROM machine_control_sessions WHERE machine_id=$1`, c.hostID); err != nil {
			t.Error(err)
		}
		if _, err := f.database.SQL().ExecContext(ctx, `DELETE FROM control_environments WHERE id=$1`, environmentID); err != nil {
			t.Error(err)
		}
	})
	exec(`INSERT INTO control_environments(id,workspace_id,owner_user_id,desired_state) VALUES($1,$2,$3,'active')`, environmentID, c.hostID, f.accountID)
	exec(`INSERT INTO control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch,state,ready,last_heartbeat_at) VALUES($1,'task24','1.0',$2,'ready',true,$3)`, nodeID, epoch, now)
	exec(`INSERT INTO tunnel_routes(id,tunnel_id,name,protocol,match_type,match_hostname,path_prefix,origin_scheme,origin_address,tls_verification,created_by_actor_id,updated_by_actor_id) VALUES($1,$2,'task24','http','exact','task24.example.test','/','http','127.0.0.1:3000','not_applicable',$3,$3)`, routeID, f.tunnelID, f.accountID)
	exec(`INSERT INTO machine_control_renewals(operation_id,machine_id,installation_generation,credential_jti,issued_at,expires_at,session_generation) SELECT $1,id,installation_generation,$1,$3,$4,1 FROM user_machines WHERE id=$2`, "renew_task24_"+f.suffix, c.hostID, now.Add(-time.Minute), now.Add(time.Hour))
	exec(`INSERT INTO machine_control_sessions(machine_id,installation_generation,session_generation,operation_id,credential_jti,issued_at,expires_at) SELECT machine_id,installation_generation,session_generation,operation_id,credential_jti,issued_at,expires_at FROM machine_control_renewals WHERE operation_id=$1`, "renew_task24_"+f.suffix)
	exec(`INSERT INTO tunnel_edge_route_assignments(assignment_id,route_id,assignment_generation,account_id,tunnel_id,connector_id,host_id,machine_identity_public_key,machine_identity_thumbprint,connector_generation,connector_session_id,connector_process_generation,config_generation,config_content_hash,access_mode,viewer_policy_generation,route_generation,route_revision,edge_node_id,edge_process_epoch,edge_failure_domain,state,observed_state)
 SELECT $1,$2,1,t.account_id,t.id,c.id,c.host_id,m.public_identity_key,$3,c.generation,c.last_session_id,1,1,g.content_hash,'public',(SELECT viewer_policy_generation FROM paperboat.current_tunnel_route_viewer_policy WHERE route_id=$2),1,1,$4,$5,'task24','active','ready' FROM tunnels t JOIN tunnel_connectors c ON c.tunnel_id=t.id JOIN user_machines m ON m.id=c.host_id JOIN tunnel_config_generations g ON g.tunnel_id=t.id AND g.generation=1 WHERE c.id=$6`, "assignment_task24_"+f.suffix, routeID, c.oldThumbprint, nodeID, epoch, c.id)
	exec(`UPDATE control_tunnel_nodes SET region='task24',failure_domain='task24',roles=ARRAY['edge'],transports=ARRAY['http3','http2'],capacity_limit=100,capacity_used=0,capacity_observed_at=$2,registry_expires_at=$2::timestamptz+interval '1 hour' WHERE id=$1`, nodeID, now)
	source := SQLIngressSource{DB: f.database, Clock: f.clock}
	active := ActiveControlSession{AccountID: f.accountID, TunnelID: f.tunnelID, ConnectorID: c.id, HostID: c.hostID, SessionID: c.oldSessionID, IdentityKeyID: c.oldIdentityKeyID, IdentityKeyThumbprint: c.oldThumbprint, ProcessGeneration: 1, CredentialGeneration: 1, ConfigGeneration: 1, ConfigContentHash: f.config.ContentHash}
	check := func(t *testing.T, want int) {
		t.Helper()
		for _, read := range []func() ([]IngressDecision, error){func() ([]IngressDecision, error) { return source.Edge(ctx, nodeID, epoch) }, func() ([]IngressDecision, error) { return source.Daemon(ctx, active) }} {
			got, err := read()
			if err != nil || len(got) != want {
				t.Fatalf("projection count=%d want=%d err=%v", len(got), want, err)
			}
			if want == 1 {
				d := got[0]
				if d.Binding.EnvironmentID != environmentID || d.Binding.RouteID != routeID || d.Binding.HostID != c.hostID || d.Binding.OriginAddress != "127.0.0.1:3000" || d.Validate(now) != nil || !d.ExpiresAt.Equal(now.Add(IngressAuthorityLifetime)) {
					t.Fatal("incorrect current binding")
				}
			}
		}
	}
	check(t, 1)

	t.Run("prepared exact generation is authorized but pending is denied", func(t *testing.T) {
		id := "assignment_task24_" + f.suffix
		exec(`UPDATE tunnel_edge_route_assignments SET state='staged',observed_state='pending' WHERE assignment_id=$1`, id)
		check(t, 0)
		exec(`UPDATE tunnel_edge_route_assignments SET observed_state='prepared' WHERE assignment_id=$1`, id)
		check(t, 1)
		exec(`UPDATE tunnel_edge_route_assignments SET observed_state='prepared',edge_process_epoch='wrong_epoch' WHERE assignment_id=$1`, id)
		check(t, 0)
		exec(`UPDATE tunnel_edge_route_assignments SET state='active',observed_state='ready',edge_process_epoch=$2 WHERE assignment_id=$1`, id, epoch)
	})
	if onlyLKG {
		payload := []byte(strings.ReplaceAll(string(testConfigPayload(2, "task24.example.test")), "tunnel_1", f.tunnelID))
		digest := sha256.Sum256(payload)
		exec(`INSERT INTO tunnel_config_generations(tunnel_id,generation,previous_generation,content_hash,snapshot,activation_state,created_by_actor_id,retained_until) SELECT tunnel_id,2,1,$2,$3,'pending',created_by_actor_id,retained_until FROM tunnel_config_generations WHERE tunnel_id=$1 AND generation=1`, f.tunnelID, digest[:], payload)
		exec(`UPDATE tunnel_connectors SET last_applied_config_generation=2 WHERE id=$1`, c.id)
		exec(`UPDATE tunnel_connector_sessions SET applied_config_generation=2 WHERE id=$1`, c.oldSessionID)
		defer func() {
			exec(`UPDATE tunnel_connectors SET last_applied_config_generation=1 WHERE id=$1`, c.id)
			exec(`UPDATE tunnel_connector_sessions SET applied_config_generation=1 WHERE id=$1`, c.oldSessionID)
			exec(`DELETE FROM tunnel_config_generations WHERE tunnel_id=$1 AND generation=2`, f.tunnelID)
		}()
		check(t, 1)
		exec(`UPDATE tunnel_config_generations SET activation_state='superseded',activated_at=NULL WHERE tunnel_id=$1 AND generation=1`, f.tunnelID)
		check(t, 0)
		exec(`UPDATE tunnel_config_generations SET activation_state='active',activated_at=$2 WHERE tunnel_id=$1 AND generation=1`, f.tunnelID, now)
		return
	}
	// Configured zero is enforced before the first observed usage report.
	quota := billing.NewWorkspaceService(f.database, nil, "")
	if err := quota.InitializeAllowancePolicy(ctx, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	var originalPersonal, originalTeam sql.NullInt64
	var originalMachines sql.NullInt32
	var originalRevision int64
	if err := f.database.SQL().QueryRowContext(ctx, `SELECT personal_allowance_bytes,team_allowance_bytes,personal_machine_limit,revision FROM paperboat.billing_operator_policy WHERE singleton`).Scan(&originalPersonal, &originalTeam, &originalMachines, &originalRevision); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := f.database.SQL().ExecContext(context.Background(), `UPDATE paperboat.billing_operator_policy SET personal_allowance_bytes=$1,team_allowance_bytes=$2,personal_machine_limit=$3,revision=$4,updated_at=now() WHERE singleton`, originalPersonal, originalTeam, originalMachines, originalRevision); err != nil {
			t.Error(err)
		}
	})
	setPolicy := func(personal, team *int64, machines *int32) {
		t.Helper()
		if _, err := f.database.SQL().ExecContext(ctx, `UPDATE paperboat.billing_operator_policy SET personal_allowance_bytes=$1,team_allowance_bytes=$2,personal_machine_limit=$3,revision=revision+1,updated_at=now() WHERE singleton`, personal, team, machines); err != nil {
			t.Fatal(err)
		}
	}
	zero := int64(0)
	setPolicy(&zero, nil, nil)
	source.Quota = quota
	check(t, 0)
	setPolicy(nil, nil, nil)
	check(t, 1)
	// A public route on an unpaid team-owned host cannot retain personal benefits.
	teamID := "public_team_" + f.suffix
	exec(`INSERT INTO teams(team_id,owner_account,generation) VALUES($1,$2,1)`, teamID, f.accountID)
	t.Cleanup(func() {
		_, _ = f.database.SQL().ExecContext(context.Background(), `UPDATE user_machines SET owner_team_id=NULL WHERE id=$1`, c.hostID)
		_, _ = f.database.SQL().ExecContext(context.Background(), `DELETE FROM teams WHERE team_id=$1`, teamID)
	})
	exec(`UPDATE user_machines SET owner_team_id=$2 WHERE id=$1`, c.hostID, teamID)
	check(t, 0)
	exec(`UPDATE user_machines SET owner_team_id=NULL WHERE id=$1`, c.hostID)
	check(t, 1)
	for _, tc := range []struct {
		name, change, restore string
		id                    string
	}{
		{"quota suspension", `UPDATE control_environments SET desired_state='suspended' WHERE id=$1`, `UPDATE control_environments SET desired_state='active' WHERE id=$1`, environmentID},
		{"revoked environment", `UPDATE control_environments SET revoked_at=now() WHERE id=$1`, `UPDATE control_environments SET revoked_at=NULL WHERE id=$1`, environmentID},
		{"wrong environment workspace", `UPDATE control_environments SET workspace_id='wrong-machine' WHERE id=$1`, `UPDATE control_environments SET workspace_id=substring(id from 5) WHERE id=$1`, environmentID},
		{"stale route", `UPDATE tunnel_routes SET generation=2 WHERE id=$1`, `UPDATE tunnel_routes SET generation=1 WHERE id=$1`, routeID},
		{"paused publication", `UPDATE tunnels SET desired_state='paused' WHERE id=$1`, `UPDATE tunnels SET desired_state='active' WHERE id=$1`, f.tunnelID},
		{"restricted publication", `UPDATE tunnels SET access_mode='private' WHERE id=$1`, `UPDATE tunnels SET access_mode='public' WHERE id=$1`, f.tunnelID},
		{"revoked machine", `UPDATE user_machines SET revoked_at=now() WHERE id=$1`, `UPDATE user_machines SET revoked_at=NULL WHERE id=$1`, c.hostID},
		{"wrong installation", `UPDATE user_machines SET installation_generation=installation_generation+1 WHERE id=$1`, `UPDATE user_machines SET installation_generation=installation_generation-1 WHERE id=$1`, c.hostID},
		{"expired credential", `UPDATE tunnel_connector_credential_generations SET created_at=now()-interval '1 hour',valid_until=now()-interval '1 minute' WHERE connector_id=$1`, `UPDATE tunnel_connector_credential_generations SET valid_until=now()+interval '4 hours' WHERE connector_id=$1`, c.id},
		{"revoked credential", `UPDATE tunnel_connector_credential_generations SET state='revoked',revoked_at=now() WHERE connector_id=$1`, `UPDATE tunnel_connector_credential_generations SET state='active',revoked_at=NULL WHERE connector_id=$1`, c.id},
		{"expired session", `UPDATE tunnel_connector_sessions SET created_at=now()-interval '1 hour',lease_deadline=now()-interval '1 minute' WHERE id=$1`, `UPDATE tunnel_connector_sessions SET lease_deadline=now()+interval '20 minutes' WHERE id=$1`, c.oldSessionID},
		{"stale edge heartbeat", `UPDATE control_tunnel_nodes SET last_heartbeat_at=now()-interval '3 minutes' WHERE id=$1`, `UPDATE control_tunnel_nodes SET last_heartbeat_at=now() WHERE id=$1`, nodeID},
		{"stale connector heartbeat", `UPDATE tunnel_connector_sessions SET last_heartbeat_at=now()-interval '3 minutes' WHERE id=$1`, `UPDATE tunnel_connector_sessions SET last_heartbeat_at=now() WHERE id=$1`, c.oldSessionID},
		{"expired machine identity", `UPDATE machine_control_sessions SET expires_at=issued_at+interval '1 second' WHERE machine_id=$1`, `UPDATE machine_control_sessions SET expires_at=now()+interval '1 hour' WHERE machine_id=$1`, c.hostID},
	} {
		// Match the projection's fixed clock even when database round trips take
		// longer than the short expiry cases being exercised.
		fixtureNow := "'" + now.Format(time.RFC3339Nano) + "'::timestamptz"
		t.Run(tc.name, func(t *testing.T) {
			exec(strings.ReplaceAll(tc.change, "now()", fixtureNow), tc.id)
			t.Cleanup(func() {
				exec(strings.ReplaceAll(tc.restore, "now()", fixtureNow), tc.id)
				if tc.name == "restricted publication" {
					// Restoring access advances the authoritative viewer policy;
					// the edge must acknowledge that new policy before serving.
					check(t, 0)
					exec(`UPDATE tunnel_edge_route_assignments SET viewer_policy_generation=(SELECT viewer_policy_generation FROM paperboat.current_tunnel_route_viewer_policy WHERE route_id=$1) WHERE route_id=$1`, routeID)
				}
			})
			check(t, 0)
		})
		check(t, 1)
	}
	wrong := active
	wrong.HostID = f.connectors[1].hostID
	if got, err := source.Daemon(ctx, wrong); err != nil || len(got) != 0 {
		t.Fatalf("wrong host authority count=%d err=%v", len(got), err)
	}
	if got, err := source.Edge(ctx, nodeID, epoch+"_old"); err != nil || len(got) != 0 {
		t.Fatalf("wrong edge epoch count=%d err=%v", len(got), err)
	}
	// HTTP aliases carry their own publication generation without replacing
	// the canonical route or coupling authorization to certificate renewal.
	aliasHTTP := "alias-http-" + f.suffix + ".example.test"
	aliasDomainID := "domain_http_" + f.suffix
	exec(`INSERT INTO tunnel_domains(id,account_id,tunnel_id,route_id,hostname,match_type,ownership_challenge_reference,ownership_state,dns_target,certificate_strategy,certificate_state,caa_state,conflict_state,last_verified_at) VALUES($1,$2,$3,$4,$5,'exact',$1,'verified',$5,'managed','issuing','ready','clear',$6)`, aliasDomainID, f.accountID, f.tunnelID, routeID, aliasHTTP, now)
	check(t, 2)
	assertHTTPAlias := func(generation uint64) {
		t.Helper()
		for _, read := range []func() ([]IngressDecision, error){func() ([]IngressDecision, error) { return source.Edge(ctx, nodeID, epoch) }, func() ([]IngressDecision, error) { return source.Daemon(ctx, active) }} {
			decisions, err := read()
			if err != nil || len(decisions) != 2 {
				t.Fatalf("HTTP names count=%d err=%v", len(decisions), err)
			}
			seen := map[string]bool{}
			for _, decision := range decisions {
				seen[decision.Binding.Hostname] = true
				if decision.Binding.Hostname == aliasHTTP && (decision.Binding.PublicationID != aliasDomainID || decision.Binding.PublicationGeneration != generation || decision.Binding.RouteID != routeID || decision.Validate(now) != nil) {
					t.Fatalf("HTTP alias lost authority fence: %+v", decision.Binding)
				}
			}
			if !seen[aliasHTTP] || !seen["task24.example.test"] {
				t.Fatalf("HTTP names=%v", seen)
			}
		}
	}
	assertHTTPAlias(1)
	exec(`UPDATE tunnel_domains SET generation=generation+1 WHERE id=$1`, aliasDomainID)
	assertHTTPAlias(2)
	for _, mutation := range []struct{ change, restore string }{
		{`UPDATE tunnel_domains SET ownership_state='revoked' WHERE id=$1`, `UPDATE tunnel_domains SET ownership_state='verified' WHERE id=$1`},
		{`UPDATE tunnel_domains SET conflict_state='conflicted' WHERE id=$1`, `UPDATE tunnel_domains SET conflict_state='clear' WHERE id=$1`},
		{`UPDATE tunnel_domains SET deleted_at=$2 WHERE id=$1`, `UPDATE tunnel_domains SET deleted_at=NULL WHERE id=$1`},
	} {
		if strings.Contains(mutation.change, "$2") {
			exec(mutation.change, aliasDomainID, now)
		} else {
			exec(mutation.change, aliasDomainID)
		}
		check(t, 1)
		exec(mutation.restore, aliasDomainID)
		check(t, 2)
	}
	// Restricted selection must remain separate from anonymous projections and
	// cannot turn a route or edge selector into viewer authority.
	for _, mode := range []string{"private", "team"} {
		exec(`UPDATE tunnels SET access_mode=$2 WHERE id=$1`, f.tunnelID, mode)
		exec(`UPDATE tunnel_edge_route_assignments SET access_mode=$2,viewer_policy_generation=(SELECT viewer_policy_generation FROM paperboat.current_tunnel_route_viewer_policy WHERE route_id=$1) WHERE route_id=$1`, routeID, mode)
		check(t, 0)
		d, err := source.RestrictedRoute(ctx, nodeID, epoch, mode, routeID, "task24.example.test")
		if err != nil || d.Binding.Audience != mode || d.Binding.RouteID != routeID || d.Binding.HostID != c.hostID {
			t.Fatalf("restricted %s projection: %v", mode, err)
		}
		if d.Validate(now) == nil {
			t.Fatal("connector projection must not itself grant viewer access")
		}
		alias, err := source.RestrictedRoute(ctx, nodeID, epoch, mode, routeID, aliasHTTP)
		if err != nil || alias.Binding.Hostname != aliasHTTP || alias.Binding.PublicationID != aliasDomainID || alias.Binding.PublicationGeneration != 2 || alias.Binding.Audience != mode || alias.Validate(now) == nil {
			t.Fatalf("restricted %s alias authority: %+v err=%v", mode, alias.Binding, err)
		}
		for _, hostname := range []string{"", "unowned.example.test"} {
			if _, err := source.RestrictedRoute(ctx, nodeID, epoch, mode, routeID, hostname); err == nil {
				t.Fatal("unowned hostname received restricted authority")
			}
		}
		exec(`UPDATE tunnel_domains SET ownership_state='revoked' WHERE id=$1`, aliasDomainID)
		if _, err := source.RestrictedRoute(ctx, nodeID, epoch, mode, routeID, aliasHTTP); err == nil {
			t.Fatal("revoked alias received restricted authority")
		}
		exec(`UPDATE tunnel_domains SET ownership_state='verified' WHERE id=$1`, aliasDomainID)
		if _, err := source.RestrictedRoute(ctx, nodeID, epoch+"_old", mode, routeID, "task24.example.test"); err == nil {
			t.Fatal("stale edge received restricted projection")
		}
		if _, err := source.RestrictedRoute(ctx, nodeID, epoch, mode, routeID+"_wrong", "task24.example.test"); err == nil {
			t.Fatal("wrong route received restricted projection")
		}
	}
	exec(`UPDATE tunnels SET access_mode='public' WHERE id=$1`, f.tunnelID)
	exec(`UPDATE tunnel_edge_route_assignments SET access_mode='public',viewer_policy_generation=(SELECT viewer_policy_generation FROM paperboat.current_tunnel_route_viewer_policy WHERE route_id=$1) WHERE route_id=$1`, routeID)
	exec(`DELETE FROM tunnel_domains WHERE id=$1`, aliasDomainID)
	exec(`UPDATE tunnel_routes SET protocol='tcp',match_type='managed',path_prefix=NULL,origin_scheme='tcp',origin_address='127.0.0.1:5432',preserve_host=false,public_tcp_listener_id=$2,public_tcp_port=24568 WHERE id=$1`, routeID, "listener_task28_"+f.suffix)
	decisions, err := source.Edge(ctx, nodeID, epoch)
	if err != nil || len(decisions) != 1 {
		t.Fatalf("public TCP projection count=%d err=%v", len(decisions), err)
	}
	binding := decisions[0].Binding
	if binding.Protocol != "tcp" || binding.ListenerID != "listener_task28_"+f.suffix || binding.PublicPort != 24568 || binding.PathPrefix != "" || binding.OriginAddress != "127.0.0.1:5432" || decisions[0].Validate(now) != nil {
		t.Fatalf("public TCP binding = %+v", decisions[0])
	}
	var managedEndpoint string
	if err := f.database.SQL().QueryRowContext(ctx, `SELECT stable_endpoint FROM tunnels WHERE id=$1`, f.tunnelID).Scan(&managedEndpoint); err != nil {
		t.Fatal(err)
	}
	managedTLS := strings.TrimPrefix(managedEndpoint, "https://")
	aliasTLS := "alias-" + f.suffix + ".example.test"
	exec(`UPDATE tunnel_routes SET protocol='tls',match_hostname=$2,public_tcp_listener_id='listener_tls_443',public_tcp_port=443 WHERE id=$1`, routeID, managedTLS)
	exec(`INSERT INTO tunnel_domains(id,account_id,tunnel_id,route_id,hostname,match_type,ownership_challenge_reference,ownership_state,dns_target,certificate_strategy,certificate_state,caa_state,conflict_state,last_verified_at) VALUES($1,$2,$3,$4,$5,'exact',$1,'verified',$5,'none','not_applicable','not_applicable','clear',$6)`, "domain_tls_"+f.suffix, f.accountID, f.tunnelID, routeID, aliasTLS, now)
	decisions, err = source.Edge(ctx, nodeID, epoch)
	if err != nil || len(decisions) != 2 {
		t.Fatalf("TLS managed+alias projection count=%d err=%v", len(decisions), err)
	}
	seen := map[string]bool{}
	var aliasDecision IngressDecision
	for _, decision := range decisions {
		seen[decision.Binding.Hostname] = true
		if decision.Binding.Protocol != "tls" || decision.Binding.ListenerID != "listener_tls_443" || decision.Binding.PublicPort != 443 || decision.Validate(now) != nil {
			t.Fatalf("TLS binding=%+v", decision)
		}
		if decision.Binding.Hostname == aliasTLS {
			aliasDecision = decision
		}
	}
	if !seen[managedTLS] || !seen[aliasTLS] {
		t.Fatalf("TLS names=%v", seen)
	}
	if aliasDecision.Binding.PublicationID != "domain_tls_"+f.suffix || aliasDecision.Binding.PublicationGeneration != 1 {
		t.Fatalf("alias publication fence=%+v", aliasDecision.Binding)
	}
	exec(`UPDATE tunnel_domains SET generation=2 WHERE id=$1`, "domain_tls_"+f.suffix)
	decisions, err = source.Edge(ctx, nodeID, epoch)
	if err != nil || len(decisions) != 2 {
		t.Fatalf("refreshed alias projection count=%d err=%v", len(decisions), err)
	}
	for _, decision := range decisions {
		if decision.Binding.Hostname == aliasTLS && (decision.Binding.PublicationGeneration != 2 || decision.Binding == aliasDecision.Binding) {
			t.Fatalf("alias generation did not fence old authority: old=%+v new=%+v", aliasDecision.Binding, decision.Binding)
		}
	}
	exec(`UPDATE tunnel_domains SET ownership_state='revoked' WHERE id=$1`, "domain_tls_"+f.suffix)
	decisions, err = source.Edge(ctx, nodeID, epoch)
	if err != nil || len(decisions) != 1 || decisions[0].Binding.Hostname != managedTLS {
		t.Fatalf("revoked alias projection=%+v err=%v", decisions, err)
	}
	installation := "installation_ingress_" + f.suffix
	identity := sha256.Sum256([]byte(installation))
	t.Cleanup(func() {
		for _, query := range []string{`DELETE FROM selfhost_pool_policies WHERE scope_kind='account' AND scope_id=$1`, `DELETE FROM selfhost_installations WHERE scope_kind='account' AND scope_id=$1`} {
			if _, err := f.database.SQL().ExecContext(context.Background(), query, f.accountID); err != nil {
				t.Error(err)
			}
		}
	})
	exec(`UPDATE control_tunnel_nodes SET tls_spki_sha256=$2,public_ingress_ipv4='140.245.11.41',public_ingress_verified_at=now(),last_heartbeat_at=now(),endpoint_host='edge.example.test',endpoint_tcp_port=27443,endpoint_quic_port=27444 WHERE id=$1`, nodeID, identity[:])
	exec(`INSERT INTO selfhost_installations(id,node_id,installation_public_key,runtime_hash,runtime_ciphertext,name,capability,scope_kind,scope_id,claim_id,claim_complete) VALUES($1,$2,$3,$3,$3,'Ingress fixture','tunnel','account',$4,$1,true)`, installation, nodeID, identity[:], f.accountID)
	exec(`INSERT INTO selfhost_pool_policies(scope_kind,scope_id,capability,mode,installation_ids) VALUES('account',$1,'tunnel','self-hosted-only',ARRAY[$2])`, f.accountID, installation)
	exec(`UPDATE tunnel_domains SET ownership_state='verified',expected_records='[{"type":"A","value":"140.245.11.41"}]'::jsonb WHERE id=$1`, "domain_tls_"+f.suffix)
	decisions, err = source.Edge(ctx, nodeID, epoch)
	if err != nil || len(decisions) != 1 || decisions[0].Binding.Hostname != aliasTLS {
		t.Fatalf("selfhost TLS exposed managed hostname or lost verified alias: %+v %v", decisions, err)
	}
	exec(`UPDATE tunnel_domains SET ownership_state='revoked' WHERE id=$1`, "domain_tls_"+f.suffix)
	decisions, err = source.Edge(ctx, nodeID, epoch)
	if err != nil || len(decisions) != 0 {
		t.Fatalf("selfhost TLS retained managed authority after alias revocation: %+v %v", decisions, err)
	}

}

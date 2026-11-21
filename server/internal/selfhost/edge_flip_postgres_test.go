package selfhost

import (
	"context"
	"errors"
	"github.com/google/uuid"
	"os"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/testutil"
)

// Changing a workspace's edge ownership: running previews and tunnels refuse it and are named (a browser terminal never does: it always uses Paperboat's edges);
// once they are stopped the flip repoints the domains of what is left (a paused tunnel, an expired preview) at the
// new edges and drops their placements in the same transaction, in both directions; it wakes the workspace's
// machines; and it is refused, changing nothing, while the new servers have no verified public address.
func TestEdgeFlipRepointsDomainsAndNamesLiveResourcesOnPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("requires isolated PostgreSQL")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	ctx := t.Context()
	if err = db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	pairPublishBatch = 1
	t.Cleanup(func() { pairPublishBatch = 1000 })
	stamp := randomIdentity()
	owner := "flip_owner_" + stamp
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("%v: %s", err, query)
		}
	}
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$2,'active')`, owner, owner+"@example.test")
	machine := testutil.PublicationMachine(t, store.SQL(), owner)
	installation := scopedInstallationFixture(t, store, "edge-flip-test-key", owner, "tunnel")
	m1, m2 := "flip_m1_"+stamp, "flip_m2_"+stamp
	// Each managed node is a region of its own (the pair names two regions).
	r1, r2 := "flip_r1_"+stamp, "flip_r2_"+stamp
	for _, id := range []string{r1, r2} {
		exec(`INSERT INTO paperboat.operator_regions(id,display_name,enabled) VALUES($1,'Flip '||$1,true)`, id)
	}
	for _, id := range []string{m1, m2} {
		exec(`INSERT INTO paperboat.control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch,state,ready,last_heartbeat_at,region,failure_domain,roles,transports,capacity_limit,capacity_used,capacity_observed_at,registry_expires_at,endpoint_host,carrier_endpoint_host,carrier_endpoint_tcp_port,carrier_endpoint_quic_port,carrier_server_spki_sha256,carrier_server_certificate_chain_pem,tls_spki_sha256,public_ingress_verified_at)
VALUES($1,'default','1.0','epoch','ready',true,now(),CASE WHEN $1=$2 THEN $3 ELSE $4 END,$1,ARRAY['edge'],ARRAY['http3','http2'],100,0,now(),now()+interval '1 hour',$1||'.example.test','edge.example.test',27443,27444,'sha256:'||repeat('ab',32),'pem',decode(repeat('ab',32),'hex'),now())`, id, m1, r1, r2)
	}
	tunnel, route, preview := "flip_t_"+stamp, "flip_r_"+stamp, "flip_p_"+stamp
	tunnelEndpoint, previewLabel := testutil.EndpointID("flip:"+stamp), testutil.EndpointLabel("flip-preview:"+stamp)
	managedTunnelTarget := testutil.EndpointLabel(tunnelEndpoint) + ".example.test"
	managedPreviewTarget := previewLabel + ".preview.example.test"
	t.Cleanup(func() {
		for _, q := range []string{
			`DELETE FROM paperboat.selfhost_placements WHERE installation_id=$1`,
		} {
			store.SQL().ExecContext(context.Background(), q, installation.InstallationID)
		}
		for _, q := range []string{
			`DELETE FROM paperboat.native_usage_authorities WHERE workspace_id='personal:'||$1`,
			`DELETE FROM paperboat.preview_domains WHERE account_id=$1`,
			`DELETE FROM paperboat.preview_leases WHERE account_id=$1`,
			`DELETE FROM paperboat.tunnel_domains WHERE account_id=$1`,
			`DELETE FROM paperboat.tunnel_routes WHERE tunnel_id IN (SELECT id FROM paperboat.tunnels WHERE account_id=$1)`,
			`DELETE FROM paperboat.tunnels WHERE account_id=$1`,
			`DELETE FROM paperboat.workspace_edge_sets WHERE scope_id=$1`,
			`DELETE FROM paperboat.selfhost_pool_policies WHERE scope_id=$1`,
			`DELETE FROM paperboat.user_machines WHERE user_id=$1`,
			`DELETE FROM paperboat.users WHERE id=$1`,
		} {
			store.SQL().ExecContext(context.Background(), q, owner)
		}
		store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.control_tunnel_nodes WHERE id IN ($1,$2)`, m1, m2)
		store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.operator_regions WHERE id IN ($1,$2)`, r1, r2)
	})
	exec(`INSERT INTO paperboat.tunnels(id,account_id,name,desired_state,stable_endpoint_id,stable_endpoint,created_by_host_id,created_by_actor_id) VALUES($1,$2,'api tunnel','active',$3,$4,$5,'actor')`, tunnel, owner, tunnelEndpoint, "https://"+managedTunnelTarget, machine)
	exec(`INSERT INTO paperboat.tunnel_routes(id,tunnel_id,name,protocol,match_type,match_hostname,origin_scheme,origin_address,created_by_actor_id,updated_by_actor_id) VALUES($1,$2,$1,'http','managed','managed.example.test','http','127.0.0.1:3000','actor','actor')`, route, tunnel)
	wsT := strings.ReplaceAll(uuid.NewString(), "-", "")
	exec(`INSERT INTO paperboat.workspace_domains(id,account_id,hostname,challenge_reference,ownership_state,created_by_actor_id) VALUES($1,$2,$3,'x','verified',$2)`, "wsd_"+wsT, owner, wsT+".example.test")
	exec(`INSERT INTO paperboat.tunnel_domains(id,account_id,tunnel_id,route_id,hostname,match_type,ownership_challenge_reference,ownership_state,dns_target,certificate_strategy,certificate_state,caa_state,conflict_state,workspace_domain_id) VALUES($1,$2,$3,$4,$5,'exact','dns:test','verified',$6,'managed','ready','ready','clear',$7)`, "flip_td_"+stamp, owner, tunnel, route, "t."+stamp+".example.test", managedTunnelTarget, "wsd_"+wsT)
	exec(`INSERT INTO paperboat.preview_leases(owner_session_kind,id,endpoint,account_id,actor_id,owner_machine_id,owner_session_id,target_scheme,target_address,created_at,lease_deadline) VALUES('foreground',$1,$2,$3,'actor',$4,'session','http','127.0.0.1:3000',now()-interval '1 hour',now()+interval '10 minutes')`, preview, "https://"+managedPreviewTarget, owner, machine)
	wsP := strings.ReplaceAll(uuid.NewString(), "-", "")
	exec(`INSERT INTO paperboat.workspace_domains(id,account_id,hostname,challenge_reference,ownership_state,created_by_actor_id) VALUES($1,$2,$3,'x','verified',$2)`, "wsd_"+wsP, owner, wsP+".example.test")
	exec(`INSERT INTO paperboat.preview_domains(id,account_id,preview_id,preview_generation,hostname,match_type,ownership_challenge_reference,ownership_state,dns_target,certificate_strategy,certificate_state,caa_state,conflict_state,workspace_domain_id) VALUES($1,$2,$3,1,$4,'exact','dns:test','verified',$5,'managed','ready','ready','clear',$6)`, "flip_pd_"+stamp, owner, preview, "p."+stamp+".example.test", managedPreviewTarget, "wsd_"+wsP)
	exec(`INSERT INTO paperboat.native_usage_authorities(access_session_id,machine_id,installation_generation,workspace_id,resource_kind,resource_id,resource_name,machine_name,capabilities,admitted_at,expires_at,retain_until,edge_node_id)
 VALUES($1,$2,1,'personal:'||$3,'terminal',$4,'build shell','Work laptop',ARRAY['terminal'],now(),now()+interval '5 minutes',now()+interval '7 days',$5)`, "flip_att_"+stamp, machine, owner, "flip_term_"+stamp, m1)
	service := New(store, "edge-flip-test-key")
	selfHosted := Policy{Mode: "self-hosted-only", InstallationIDs: []string{installation.InstallationID}}

	// 1. The preview and tunnel run (and so does a browser terminal, which does not count): the refusal names both.
	var live *LivePublicationsError
	_, err = service.SetPool(ctx, owner, "tunnel", selfHosted)
	if !errors.As(err, &live) || live.Count != 2 || len(live.Items) != 2 || !errors.Is(err, ErrLiveEdgeModeChange) {
		t.Fatalf("live refusal: %v %+v", err, live)
	}
	names := map[string]LiveItem{}
	for _, item := range live.Items {
		names[item.Kind] = item
	}
	if names["tunnel"].Name != "api tunnel" || names["tunnel"].ID != tunnel || names["preview"].ID != preview || !strings.Contains(names["preview"].Name, previewLabel) {
		t.Fatalf("live items: %+v", live.Items)
	}
	// Only the stopped tunnel and the expired preview are left once the preview expires and the tunnel pauses.
	exec(`UPDATE paperboat.preview_leases SET lease_deadline=now()-interval '1 minute' WHERE id=$1`, preview)
	exec(`UPDATE paperboat.tunnels SET desired_state='paused' WHERE id=$1`, tunnel)

	domain := func(table, id string) (target, recordType, value string, generation int64) {
		t.Helper()
		if err := store.SQL().QueryRowContext(ctx, `SELECT dns_target,coalesce(expected_records->0->>'type',''),coalesce(expected_records->0->>'value',''),generation FROM paperboat.`+table+` WHERE id=$1`, id).Scan(&target, &recordType, &value, &generation); err != nil {
			t.Fatal(err)
		}
		return
	}
	// 2. The new server has no verified public address: refused with its own message, nothing changed.
	if _, err = service.SetPool(ctx, owner, "tunnel", selfHosted); !errors.Is(err, ErrNewEdgesNotAddressable) {
		t.Fatalf("flip to servers without an address: %v", err)
	}
	if target, _, _, generation := domain("tunnel_domains", "flip_td_"+stamp); target != managedTunnelTarget || generation != 1 {
		t.Fatalf("a refused flip changed the domain: %q gen %d", target, generation)
	}
	if mode, _ := service.PoolScoped(ctx, owner, "tunnel", Scope{}); mode.Mode != "managed-only" {
		t.Fatalf("a refused flip changed the pool: %+v", mode)
	}

	// 3. managed -> self-hosted: both domains now name the server's address; machines are woken.
	exec(`UPDATE paperboat.control_tunnel_nodes SET endpoint_host='198.51.100.7',state='ready',ready=true,last_heartbeat_at=now(),registry_expires_at=now()+interval '1 hour',public_ingress_verified_at=now(),public_ingress_ipv4='198.51.100.7' WHERE id=$1`, installation.NodeID)
	if _, err = service.SetPool(ctx, owner, "tunnel", selfHosted); err != nil {
		t.Fatal(err)
	}
	for table, id := range map[string]string{"tunnel_domains": "flip_td_" + stamp, "preview_domains": "flip_pd_" + stamp} {
		if target, typ, value, generation := domain(table, id); target != "198.51.100.7" || typ != "A" || value != "198.51.100.7" || generation != 2 {
			t.Fatalf("%s after the flip to self-hosted: target=%q record=%s %q gen %d", table, target, typ, value, generation)
		}
	}
	for _, topic := range []string{"tunnels", "previews"} {
		var woken int
		if err = store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.machine_topic_generations WHERE machine_id=$1 AND topic=$2`, machine, topic).Scan(&woken); err != nil || woken != 1 {
			t.Fatalf("machine not woken on %s by a pool flip: %d %v", topic, woken, err)
		}
	}

	// 4. self-hosted -> managed through a pair: the placement is gone and the domains name the managed targets.
	if _, err = service.SetPlacement(ctx, owner, "tunnel", tunnel, &installation.InstallationID); err != nil {
		t.Fatal(err)
	}
	if _, err = service.SetEdgePair(ctx, owner, Scope{Kind: "account", ID: owner}, "managed", r1, r2); err != nil {
		t.Fatal(err)
	}
	var placements int
	if err = store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.selfhost_placements WHERE installation_id=$1`, installation.InstallationID).Scan(&placements); err != nil || placements != 0 {
		t.Fatalf("placements left after the flip: %d %v", placements, err)
	}
	if target, typ, _, generation := domain("tunnel_domains", "flip_td_"+stamp); target != managedTunnelTarget || typ != "CNAME" || generation != 3 {
		t.Fatalf("tunnel domain after the flip to managed: target=%q type=%s gen %d", target, typ, generation)
	}
	if target, typ, _, generation := domain("preview_domains", "flip_pd_"+stamp); target != managedPreviewTarget || typ != "CNAME" || generation != 3 {
		t.Fatalf("preview domain after the flip to managed: target=%q type=%s gen %d", target, typ, generation)
	}
	// A pair that keeps the mode changes no domain.
	if _, err = service.SetEdgePair(ctx, owner, Scope{Kind: "account", ID: owner}, "managed", r2, r1); err != nil {
		t.Fatal(err)
	}
	if _, _, _, generation := domain("tunnel_domains", "flip_td_"+stamp); generation != 3 {
		t.Fatalf("a pair change inside one mode bumped the domain generation: %d", generation)
	}
}

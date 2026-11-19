package peersessions

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/teams/teamstest"
)

// What a team member's CLI pays to renew its configuration when the team shares 84 hosts, and whether the document
// survives the route/preview/service topic bumps a busy account makes about once a second (prod 11-19: 19 slow
// builds a minute, ~300 ms each, every renewal a cache miss). Logs the build time of a cold build and of a build after
// a topic bump; the bump must not rebuild.
func TestMemberConfigurationOfAnEightyFourHostTeamSurvivesTopicBumpsOnPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	store := openMeshTestStore(t, ctx)
	s := fmt.Sprint(time.Now().UnixNano())
	owner, member, team, cli := "mb_owner_"+s, "mb_member_"+s, "mb-team-"+s, "mb_cli_"+s
	const hosts = 84
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("%v\n%s", err, query)
		}
	}
	for _, account := range []string{owner, member} {
		exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, account)
	}
	t.Cleanup(func() {
		_, _ = store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.teams WHERE team_id=$1`, team)
		_, _ = store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.users WHERE id=ANY($1)`, []string{owner, member})
	})
	seedMeshMachines(t, ctx, store, owner, 0, hosts-1)
	exec(`INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$2,1)`, team, owner)
	exec(`INSERT INTO paperboat.billing_workspaces(id,team_id,state,period_start,period_end) VALUES($1,$2,'active',now()-interval '1 hour',now()+interval '1 hour')`, "billing_"+team, team)
	exec(`INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'admin',true),($1,$3,1,'member',true)`, team, owner, member)
	exec(`INSERT INTO paperboat.team_resource_bindings(team_id,resource_kind,resource_id,owner_account) SELECT $1,'machine',id,$2 FROM paperboat.user_machines WHERE user_id=$2`, team, owner)
	for i := range hosts {
		teamstest.SetMachineAccess(t, store, team, fmt.Sprintf("%s_m_%d", owner, i), member, "terminal", "exec")
	}
	// The member's CLI endpoint: session, certificate and identity.
	exec(`INSERT INTO paperboat.cli_client_sessions(id,user_id,client_id,client_label,device_type,os,scopes,state,created_at,approved_at) VALUES($1,$2,$1,'t','desktop','test',ARRAY['machines:connect'],'active',now(),now())`, cli, member)
	exec(`INSERT INTO paperboat.peer_endpoint_certificates(fingerprint,user_id,endpoint_id,role,generation,serial,certificate,quic_public_key,issued_at,expires_at,key_id,signer_public_key)
VALUES(sha256(convert_to($1,'UTF8')),$2,$1,'cli',1,1,decode(repeat('00',200),'hex'),sha256(convert_to('key'||$1,'UTF8')),now()-interval '1 minute',now()+interval '1 day','key',sha256(convert_to('key'||$1,'UTF8')))`, cli, member)
	exec(`INSERT INTO paperboat.peer_network_identities(user_id,endpoint_id,role,machine_id,endpoint_generation,machine_generation,key_generation,wireguard_public_key,disco_public_key,quic_certificate_fingerprint,virtual_address,created_at,updated_at)
VALUES($2,$1,'cli',NULL,1,0,1,sha256(convert_to('key'||$1,'UTF8')),sha256(convert_to('disco'||$1,'UTF8')),sha256(convert_to($1,'UTF8')),('fd7a:115c:a1e0::'::inet + nextval('paperboat.peer_network_virtual_address_seq')),now(),now())`, cli, member)

	service, err := NewNetworkService(store, meshTestSigner{}, "https://issuer.example.test")
	if err != nil {
		t.Fatal(err)
	}
	usePolicyCache(t, service, store)
	request := func(name string) (configTrace, uint64) {
		t.Helper()
		var trace configTrace
		before := service.cache.builds
		in := NetworkConfigRequest{OperationID: "mb_" + name + "_" + s, UserID: member, EndpointID: cli, Authority: AuthorityUntilRevoked}
		if _, err := service.configuration(ctx, in, &trace); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return trace, service.cache.builds - before
	}
	var mu sync.Mutex
	var seen []string
	marker := service.cache.observe
	service.cache.mu.Lock()
	service.cache.observe = func(key string) {
		mu.Lock()
		seen = append(seen, key)
		mu.Unlock()
		marker(key)
	}
	service.cache.mu.Unlock()
	request("warmup")                                                           // loads the mesh and primes the connections
	exec(`SELECT paperboat.publish_peer_policy(ARRAY['account:'||$1])`, member) // a document change: forces one cold build
	if err := service.policySync(ctx); err != nil {                             // the change above is applied before the cold build starts, so the document is stored
		t.Fatal(err)
	}
	cold, builds := request("cold")
	if builds != 1 || cold.peers < hosts {
		t.Fatalf("cold request: %d builds, %d peers, want 1 build and %d peers", builds, cold.peers, hosts)
	}
	if _, builds = request("again"); builds != 0 {
		mu.Lock()
		t.Fatalf("an unchanged document was rebuilt (notifications since the warmup: %v)", seen)
	}
	mu.Lock()
	seen = nil
	mu.Unlock()
	for _, topic := range []string{"private_routes", "previews", "machine_services"} {
		exec(`SELECT paperboat.publish_account_topic(ARRAY[$1::text,$2::text],$3)`, member, owner, topic)
	}
	_, builds = request("after topic bumps")
	t.Logf("RESULT member CLI of an %d-host team: cold build_ms=%d (peers %d), rebuilds after route/preview/service topic bumps: %d",
		hosts, cold.build.Milliseconds(), cold.peers, builds)
	if builds != 0 {
		mu.Lock()
		t.Fatalf("a route, preview and service topic bump rebuilt the document %d times (notifications: %v)", builds, seen)
	}
}

package peersessions

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/teams/teamstest"
)

func seedMeshCLI(t *testing.T, ctx context.Context, store *db.DB, account, cli string) {
	t.Helper()
	for _, query := range []string{
		`INSERT INTO paperboat.cli_client_sessions(id,user_id,client_id,client_label,device_type,os,scopes,state,created_at,approved_at) VALUES($1,$2,$1,'t','desktop','test',ARRAY['machines:connect'],'active',now(),now())`,
		`INSERT INTO paperboat.peer_endpoint_certificates(fingerprint,user_id,endpoint_id,role,generation,serial,certificate,quic_public_key,issued_at,expires_at,key_id,signer_public_key)
VALUES(sha256(convert_to($1,'UTF8')),$2,$1,'cli',1,1,decode(repeat('00',200),'hex'),sha256(convert_to('key'||$1,'UTF8')),now()-interval '1 minute',now()+interval '1 day','key',sha256(convert_to('key'||$1,'UTF8')))`,
		`INSERT INTO paperboat.peer_network_identities(user_id,endpoint_id,role,machine_id,endpoint_generation,machine_generation,key_generation,wireguard_public_key,disco_public_key,quic_certificate_fingerprint,virtual_address,created_at,updated_at)
VALUES($2,$1,'cli',NULL,1,0,1,sha256(convert_to('key'||$1,'UTF8')),sha256(convert_to('disco'||$1,'UTF8')),sha256(convert_to($1,'UTF8')),('fd7a:115c:a1e0::'::inet + nextval('paperboat.peer_network_virtual_address_seq')),now(),now())`,
	} {
		if _, err := store.SQL().ExecContext(ctx, query, cli, account); err != nil {
			t.Fatalf("%v\n%s", err, query)
		}
	}
}

type basisFixture struct {
	store                *db.DB
	service              *NetworkService
	account, owner, team string
	endpoints, hosts     []string // the account's machines then its CLIs; the hosts the team shares with it
	exec                 func(string, ...any)
}

// newBasisFixture is an account with machines and CLIs that is a member of a team sharing hosts of another owner.
func newBasisFixture(t *testing.T, ctx context.Context, machines, clis, hosts int) *basisFixture {
	store := openMeshTestStore(t, ctx)
	s := fmt.Sprint(time.Now().UnixNano())
	f := &basisFixture{store: store, account: "ab_" + s, owner: "abo_" + s, team: "ab-team-" + s}
	f.exec = func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("%v\n%s", err, query)
		}
	}
	for _, account := range []string{f.account, f.owner} {
		f.exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, account)
	}
	t.Cleanup(func() {
		_, _ = store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.teams WHERE team_id=$1`, f.team)
		_, _ = store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.users WHERE id=ANY($1)`, []string{f.account, f.owner})
	})
	seedMeshMachines(t, ctx, store, f.account, 0, machines-1)
	for i := range machines {
		f.endpoints = append(f.endpoints, fmt.Sprintf("%s_m_%d", f.account, i))
	}
	for i := range clis {
		f.endpoints = append(f.endpoints, fmt.Sprintf("%s_cli_%d", f.account, i))
		seedMeshCLI(t, ctx, store, f.account, f.endpoints[len(f.endpoints)-1])
	}
	seedMeshMachines(t, ctx, store, f.owner, 0, hosts-1)
	f.exec(`INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$2,1)`, f.team, f.owner)
	f.exec(`INSERT INTO paperboat.billing_workspaces(id,team_id,state,period_start,period_end) VALUES($1,$2,'active',now()-interval '1 hour',now()+interval '1 hour')`, "billing_"+f.team, f.team)
	f.exec(`INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'admin',true),($1,$3,1,'member',true)`, f.team, f.owner, f.account)
	f.exec(`INSERT INTO paperboat.team_resource_bindings(team_id,resource_kind,resource_id,owner_account) SELECT $1,'machine',id,$2 FROM paperboat.user_machines WHERE user_id=$2`, f.team, f.owner)
	for i := range hosts {
		f.hosts = append(f.hosts, fmt.Sprintf("%s_m_%d", f.owner, i))
		teamstest.SetMachineAccess(t, store, f.team, f.hosts[i], f.account, "terminal", "exec")
	}
	var err error
	if f.service, err = NewNetworkService(store, meshTestSigner{}, "https://issuer.example.test"); err != nil {
		t.Fatal(err)
	}
	usePolicyCache(t, f.service, store)
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if _, listening := f.service.cache.begin(); listening {
			return f
		}
		if time.Now().After(deadline) {
			t.Fatal("policy listener did not attach")
		}
	}
}

func (f *basisFixture) request(endpoint string) NetworkConfigRequest {
	return NetworkConfigRequest{OperationID: "ab_config_" + endpoint, UserID: f.account, EndpointID: endpoint, Authority: AuthorityUntilRevoked}
}

// One account change wakes every endpoint: the account-wide statements run once between them, not once each.
func TestOneAccountChangeReadsTheAccountOnceOnPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	f := newBasisFixture(t, ctx, 20, 10, 20)
	round := func() {
		var group sync.WaitGroup
		for _, id := range f.endpoints {
			group.Go(func() {
				if _, err := f.service.Configuration(ctx, f.request(id)); err != nil {
					t.Errorf("%s: %v", id, err)
				}
			})
		}
		group.Wait()
	}
	round()
	round()
	seedMeshMachines(t, ctx, f.store, f.account, 20, 20)
	if err := f.service.policySync(ctx); err != nil {
		t.Fatal(err)
	}
	f.service.policySync = nil // its own statements are not the endpoints'
	var queries atomic.Int64
	f.store.SetQueryObserver(func(ctx context.Context) (context.Context, func(string)) {
		queries.Add(1)
		return ctx, func(string) {}
	})
	f.service.cache.mu.Lock()
	builds := f.service.cache.builds
	f.service.cache.mu.Unlock()
	round()
	f.store.SetQueryObserver(nil)
	n := len(f.endpoints)
	t.Logf("RESULT one change, %d endpoints: %d documents built, %d statements (%.1f per endpoint)", n, f.service.cache.builds-builds, queries.Load(), float64(queries.Load())/float64(n))
	if int(queries.Load()) > n*basisStatementsPerEndpoint {
		t.Fatalf("%d statements for %d endpoints, want at most %d each", queries.Load(), n, basisStatementsPerEndpoint)
	}
}

// Half of what one endpoint's document took before the account's part was shared.
const basisStatementsPerEndpoint = 16

// After each write, the next configuration of every endpoint is the one a service with no cache builds.
func TestAccountBasisFollowsEveryRevocationOnPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	f := newBasisFixture(t, ctx, 6, 4, 2)
	m, cli := f.endpoints[:6], f.endpoints[6:]
	session := "ab_as_" + f.account
	// shape is an endpoint's document as signed.
	shape := func(d *policyDoc) [32]byte {
		return answerDigest(configurationClaims(f.service.issuer, time.Now(), d.expiry, 0, d.self, d.peers, 0), nil, "")
	}
	check := func(name string) {
		t.Helper()
		if err := f.service.policySync(ctx); err != nil { // applied before the first request, whose document is then stored
			t.Fatal(err)
		}
		for _, id := range f.endpoints {
			var trace configTrace
			_, err := f.service.configuration(ctx, f.request(id), &trace)
			bare, bareErr := NewNetworkService(f.store, meshTestSigner{}, f.service.issuer)
			if bareErr != nil {
				t.Fatal(bareErr)
			}
			fresh, freshErr := bare.buildPolicy(ctx, f.request(id), map[string][]string{}, time.Now().UTC())
			if (err == nil) != (freshErr == nil) {
				t.Fatalf("%s: %s answered %v, without a cache %v", name, id, err, freshErr)
			}
			if err != nil {
				continue
			}
			f.service.cache.mu.Lock()
			cached := f.service.cache.docs[id]
			f.service.cache.mu.Unlock()
			if cached == nil {
				t.Fatalf("%s: %s has no cached document", name, id)
			}
			if cached.validUntil.After(time.Now().Add(maxPolicyAge - meshMaxAge)) {
				t.Fatalf("%s: %s is cached past maxPolicyAge of its account-wide rows", name, id)
			}
			if shape(cached) != shape(fresh) {
				t.Fatalf("%s: %s was answered a stale document", name, id)
			}
		}
		if s := f.service.cache; s.shared("basis:"+f.account, time.Now()) == nil {
			t.Fatalf("%s: no basis is stored, so the check compares nothing", name)
		}
	}
	has := func(endpoint, peer, kind string) bool {
		f.service.cache.mu.Lock()
		defer f.service.cache.mu.Unlock()
		for _, p := range f.service.cache.docs[endpoint].peers {
			for _, scope := range p.Scopes {
				if p.Identity.EndpointID == peer && (kind == "" || strings.HasPrefix(scope.ResourceKind+":"+scope.ResourceID, kind)) {
					return true
				}
			}
		}
		return false
	}
	// The first round's own writes (the mesh's first build) settle before anything is asserted.
	for range 2 {
		for _, id := range f.endpoints {
			if _, err := f.service.Configuration(ctx, f.request(id)); err != nil {
				t.Fatal(err)
			}
		}
	}
	check("start")
	if f.service.cache.shared("hosts:"+f.account, time.Now()) == nil || !has(cli[0], f.hosts[0], "machine_access") || !has(m[0], cli[0], "") {
		t.Fatal("the fixture's CLI does not hold the shared hosts and the machines")
	}
	// A host left out of the stored host list becomes valid: the next build holds it, without waiting for an age.
	f.exec(`UPDATE paperboat.control_environments SET desired_state='suspended' WHERE owner_user_id=$1`, f.owner)
	check("shared hosts' environments suspended")
	if has(cli[0], f.hosts[0], "") {
		t.Fatal("a host with a suspended environment is still held")
	}
	f.exec(`UPDATE paperboat.control_environments SET desired_state='active' WHERE id=$1`, f.owner+"_env_0")
	if err := f.service.policySync(ctx); err != nil {
		t.Fatal(err)
	}
	built, err := f.service.buildPolicy(ctx, f.request(cli[0]), map[string][]string{}, time.Now().UTC())
	if err != nil || !slices.ContainsFunc(built.peers, func(p networkPeer) bool { return p.Identity.EndpointID == f.hosts[0] }) {
		t.Fatalf("a build after a left-out host became valid does not hold it (%v)", err)
	}
	f.exec(`UPDATE paperboat.control_environments SET desired_state='active' WHERE owner_user_id=$1`, f.owner)
	f.exec(`SELECT paperboat.publish_peer_policy(ARRAY['account:'||$1])`, f.account)
	check("shared hosts back")
	if !has(cli[0], f.hosts[1], "machine_access") {
		t.Fatal("the shared hosts did not come back")
	}
	// A team SSO session that runs out, with no write: the stored hosts end with it.
	verified := time.Now().UTC().Truncate(time.Second).Add(-3570 * time.Second)
	f.exec(`UPDATE paperboat.teams SET sso_required=true,sso_session_seconds=3600 WHERE team_id=$1`, f.team)
	f.exec(`UPDATE paperboat.team_members SET sso_verified_at=$3 WHERE team_id=$1 AND account_id=$2`, f.team, f.account, verified)
	f.exec(`SELECT paperboat.publish_peer_policy(ARRAY['account:'||$1])`, f.account)
	check("SSO session about to end")
	if d := verified.Add(time.Hour); !has(cli[0], f.hosts[0], "machine_access") || f.service.cache.shared("hosts:"+f.account, d.Add(-time.Second)) == nil || f.service.cache.shared("hosts:"+f.account, d) != nil {
		t.Fatal("the stored hosts do not end exactly with the member's SSO session")
	}
	f.exec(`UPDATE paperboat.teams SET sso_required=false WHERE team_id=$1`, f.team)
	f.exec(`SELECT paperboat.publish_peer_policy(ARRAY['account:'||$1])`, f.account)
	check("SSO not required")
	for _, step := range []struct {
		name, query string
		args        []any
		endpoint    string // holds peer with a scope of kind after the write, or not
		peer, kind  string
		want        bool
	}{
		{"access session start", `INSERT INTO paperboat.user_machine_access_sessions(credential_kind,id,user_machine_id,user_id,environment_id,cli_client_session_id,http_base_url,helper_terminal_session_id,helper_file_session_id,capabilities,operation_id,expires_at) VALUES('control_plane_signed',$1,$2,$3,$3||'_env_0',$4,'https://m.example.test',$1||'_t',$1||'_f',ARRAY['exec'],$1||'_op',now()+interval '5 minutes')`, []any{session, m[0], f.account, cli[0]}, cli[0], m[0], "machine_access:" + session, true},
		{"access session end", `UPDATE paperboat.user_machine_access_sessions SET state='revoked',revoked_at=now(),revocation_reason='test' WHERE id=$1`, []any{session}, cli[0], m[0], "machine_access:" + session, false},
		{"key rotation", `UPDATE paperboat.peer_network_identities SET key_generation=key_generation+1,wireguard_public_key=sha256(convert_to('rotated'||endpoint_id,'UTF8')) WHERE endpoint_id=$1`, []any{m[1]}, m[0], m[1], "", true},
		{"relay revocation generation only", `UPDATE paperboat.peer_network_identities SET relay_revocation_generation=relay_revocation_generation+1 WHERE endpoint_id=$1`, []any{m[1]}, m[0], m[1], "", true},
		{"machine revoked", `UPDATE paperboat.user_machines SET state='revoked',revoked_at=now(),seat_state='released' WHERE id=$1`, []any{m[1]}, m[0], m[1], "", false},
		{"CLI session revoked", `UPDATE paperboat.cli_client_sessions SET state='revoked',revoked_at=now() WHERE id=$1`, []any{cli[1]}, m[0], cli[1], "", false},
		{"certificate revoked", `UPDATE paperboat.peer_endpoint_certificates SET revoked_at=now(),revocation_reason='endpoint_removed' WHERE endpoint_id=$1`, []any{m[2]}, m[0], m[2], "", false},
		{"certificate revocation record", `INSERT INTO paperboat.peer_endpoint_certificate_revocations(operation_id,user_id,certificate_fingerprint,serial,reason,created_at) SELECT 'revoke_'||endpoint_id||'_op',user_id,fingerprint,serial,'endpoint_removed',now() FROM paperboat.peer_endpoint_certificates WHERE endpoint_id=$1`, []any{m[3]}, m[0], m[3], "", false},
		{"identity revoked", `UPDATE paperboat.peer_network_identities SET revoked_at=now() WHERE endpoint_id=$1`, []any{cli[2]}, m[0], cli[2], "", false},
		{"shared host's key rotation", `UPDATE paperboat.peer_network_identities SET disco_public_key=sha256(convert_to('rotated'||endpoint_id,'UTF8')) WHERE endpoint_id=$1`, []any{f.hosts[0]}, cli[0], f.hosts[0], "machine_access", true},
		{"shared host revoked", `UPDATE paperboat.user_machines SET state='revoked',revoked_at=now(),seat_state='released' WHERE id=$1`, []any{f.hosts[1]}, cli[0], f.hosts[1], "", false},
		{"entitlement lapse", `UPDATE paperboat.billing_workspaces SET period_end=now() WHERE team_id=$1`, []any{f.team}, cli[0], f.hosts[0], "", false},
		{"entitlement back", `UPDATE paperboat.billing_workspaces SET period_end=now()+interval '1 hour' WHERE team_id=$1`, []any{f.team}, cli[0], f.hosts[0], "", true},
		{"member removed", `DELETE FROM paperboat.team_members WHERE team_id=$1 AND account_id=$2`, []any{f.team, f.account}, cli[0], f.hosts[0], "", false},
	} {
		f.exec(step.query, step.args...)
		check(step.name)
		if has(step.endpoint, step.peer, step.kind) != step.want {
			t.Fatalf("after %s, %s holding %s is %t, want %t", step.name, step.endpoint, step.peer, !step.want, step.want)
		}
	}
	seedMeshMachines(t, ctx, f.store, f.account, 6, 6)
	check("machine enrolled")
	if joined := f.account + "_m_6"; !has(m[0], joined, "") || !has(cli[0], joined, "") {
		t.Fatal("an enrolled machine is missing from the account's documents")
	}
	// A certificate that runs out, with no write: the basis ends with it.
	ends := time.Now().UTC().Add(20 * time.Second).Truncate(time.Second)
	f.exec(`UPDATE paperboat.peer_endpoint_certificates SET expires_at=$2 WHERE endpoint_id=$1`, m[4], ends)
	f.exec(`SELECT paperboat.publish_account_topic(ARRAY[$1::text],'peer_network')`, f.account)
	check("certificate about to expire")
	if !has(m[0], m[4], "") || f.service.cache.shared("basis:"+f.account, ends.Add(-time.Second)) == nil || f.service.cache.shared("basis:"+f.account, ends) != nil {
		t.Fatal("the basis does not end exactly at its earliest certificate expiry")
	}
	f.service.now = func() time.Time { return time.Now().Add(21 * time.Second) }
	if _, err := f.service.Configuration(ctx, f.request(m[0])); err != nil {
		t.Fatal(err)
	}
	if has(m[0], m[4], "") {
		t.Fatal("a peer stayed past its certificate's expiry")
	}
}

// An entry built across one of its notifications is never handed to a later caller; a lost LISTEN session shares nothing.
func TestSharedDocIsNotReusedAcrossItsNotification(t *testing.T) {
	s := &NetworkService{cache: newPolicyCache()}
	s.cache.reset(true)
	now, builds := time.Now(), 0
	get := func(during func()) *policyDoc {
		t.Helper()
		d, err := s.sharedDoc(t.Context(), "basis:a", now, func(*policyDoc) bool { return true }, func() (*policyDoc, error) {
			builds++
			if during != nil {
				during()
			}
			return sharedEntry(&policyDoc{basis: &accountBasis{}, validUntil: now.Add(time.Hour)}, []string{"m1", "account:a"}, []time.Time{{}, now.Add(time.Minute)}), nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	get(func() { s.cache.notify("m1") })
	if get(nil); builds != 2 {
		t.Fatalf("%d builds: an entry built across its notification was reused", builds)
	}
	if get(nil); builds != 2 {
		t.Fatalf("%d builds, want the stored entry", builds)
	}
	s.cache.notify("account:b")
	if get(nil); builds != 2 {
		t.Fatal("another account's notification dropped the entry")
	}
	for i, key := range []string{"account:a", "m1"} {
		s.cache.notify(key)
		if get(nil); builds != 3+i {
			t.Fatalf("%s did not drop the entry", key)
		}
	}
	if now = now.Add(time.Minute); get(nil) == nil || builds != 5 {
		t.Fatal("an entry outlived its earliest end")
	}
	s.cache.reset(true) // the listener reattached: notifications may have been missed, so nothing stored survives
	if get(nil); builds != 6 {
		t.Fatal("an entry survived a new LISTEN session")
	}
	builds--
	s.cache.reset(false)
	get(nil)
	if get(nil); builds != 7 {
		t.Fatalf("%d builds: an entry was shared without a LISTEN session", builds)
	}
}

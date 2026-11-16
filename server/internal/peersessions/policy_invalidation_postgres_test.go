package peersessions

import (
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/mint"
	wire "github.com/pinksaucepasta/paperboat/shared/peeridentity"
)

// TestPeerPolicyInvalidationCoversEveryMutationPathOnPostgres is the invalidation owner's contract (migration 334 and
// the topic triggers): every write to a table the peer configuration reads notifies the parties it can affect, so the
// in-memory documents of those endpoints are dropped and the next request rebuilds them from the database. A table
// added to the document without a trigger, or a trigger that names the wrong party, fails here.
func TestPeerPolicyInvalidationCoversEveryMutationPathOnPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("isolated PostgreSQL DSN required")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := t.Context()
	if err := db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	_, signer, _ := ed25519.GenerateKey(rand.Reader)
	provider, err := mint.New([]mint.Key{{ID: "policy-invalidation", PrivateKey: signer}}, "policy-invalidation", mint.MaxProofTTL)
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewNetworkService(store, provider, "https://api.example.test")
	if err != nil {
		t.Fatal(err)
	}
	usePolicyCache(t, service, store)
	var mu sync.Mutex
	seen := map[string]bool{}
	marker := service.cache.observe
	service.cache.mu.Lock()
	service.cache.observe = func(key string) {
		mu.Lock()
		seen[key] = true
		mu.Unlock()
		marker(key)
	}
	service.cache.mu.Unlock()

	x := fmt.Sprint(time.Now().UnixNano())
	owner, member, other := "pi_owner_"+x, "pi_member_"+x, "pi_other_"+x
	host, cli, env, team := "pi_host_"+x, "pi_cli_"+x, "pi_env_"+x, "pi_team_"+x
	terminal, repo, assignment := "pi_ts_"+x, "pi_repo_"+x, "pi_asg_"+x
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	now := time.Now().UTC().Truncate(time.Second)
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active'),($2,$2,$2||'@example.test','active'),($3,$3,$3||'@example.test','active')`, owner, member, other)
	exec(`INSERT INTO paperboat.control_environments(id,workspace_id,owner_user_id) VALUES($1,$1,$2)`, env, owner)
	exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,platform,architecture,workspace_root,state,seat_state,online,installation_generation) VALUES($1,$2,$3,'linux','amd64','/w','online','occupied',true,1)`, host, owner, env)
	exec(`INSERT INTO paperboat.cli_client_sessions(id,user_id,client_id,client_label,device_type,os,scopes,state,created_at,approved_at) VALUES($1,$2,$1,'t','desktop','test',ARRAY['machines:connect'],'active',$3,$3)`, cli, owner, now)
	for _, e := range []struct{ id, role string }{{host, "machine"}, {cli, "cli"}} {
		wireRole := wire.RoleCLI
		if e.role == "machine" {
			wireRole = wire.RoleMachine
		}
		_, _, quic := networkEndpointTLS(t, e.id, now)
		certificate, fingerprint := networkCertificateWithKeys(t, signer, owner, wireRole, e.id, 1, 1, quic, now, now.Add(time.Hour))
		exec(`INSERT INTO paperboat.peer_endpoint_certificates(fingerprint,user_id,endpoint_id,role,generation,serial,certificate,quic_public_key,issued_at,expires_at,key_id,signer_public_key) VALUES($1,$2,$3,$4,1,1,$5,$6,$7,$8,'policy-invalidation',$9)`,
			fingerprint[:], owner, e.id, e.role, certificate, quic[:], now, now.Add(time.Hour), signer.Public().(ed25519.PublicKey))
		private, public := networkX25519Key(t)
		r := NetworkRegistration{OperationID: "register_" + e.id, UserID: owner, EndpointID: e.id, Role: e.role, EndpointGeneration: 1, WireGuardPublicKey: public, DiscoPublicKey: networkDiscoPublic(t, private), QUICCertificateFingerprint: fingerprint[:]}
		if e.role == "machine" {
			r.MachineID, r.MachineGeneration = host, 1
		}
		if _, err := service.Register(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$2,1)`, team, owner)
	exec(`INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'admin',true),($1,$3,1,'member',true)`, team, owner, member)
	exec(`INSERT INTO paperboat.team_resource_bindings(team_id,resource_kind,resource_id,owner_account) VALUES($1,'machine',$2,$3)`, team, host, owner)

	// Every case is one write and the parties it must name; "" cases must name none of the listed parties.
	teamKeys := []string{"account:" + owner, "account:" + member, host}
	cases := []struct {
		name  string
		query string
		args  []any
		want  []string
		none  bool
	}{
		{"identity key material", `UPDATE paperboat.peer_network_identities SET disco_public_key=decode(repeat('05',32),'hex') WHERE endpoint_id=$1`, []any{host}, []string{"account:" + owner, host}, false},
		{"identity relay revocation generation only", `UPDATE paperboat.peer_network_identities SET relay_revocation_generation=relay_revocation_generation+1 WHERE endpoint_id=$1`, []any{host}, []string{host}, true},
		{"identity configuration request's own write", `UPDATE paperboat.peer_network_identities SET config_generation=config_generation+1,updated_at=now() WHERE endpoint_id=$1`, []any{host}, []string{host}, true},
		{"certificate revoked", `UPDATE paperboat.peer_endpoint_certificates SET revoked_at=now(),revocation_reason='endpoint_removed' WHERE endpoint_id=$1`, []any{cli}, []string{"account:" + owner, cli}, false},
		{"user status", `UPDATE paperboat.users SET status='disabled' WHERE id=$1`, []any{other}, []string{"account:" + other}, false},
		{"environment suspended", `UPDATE paperboat.control_environments SET desired_state='suspended' WHERE id=$1`, []any{env}, []string{"account:" + owner}, false},
		{"environment active", `UPDATE paperboat.control_environments SET desired_state='active' WHERE id=$1`, []any{env}, []string{"account:" + owner}, false},
		{"machine presence", `UPDATE paperboat.user_machines SET online=false WHERE id=$1`, []any{host}, []string{host}, false},
		{"machine capabilities", `UPDATE paperboat.user_machines SET configured_capabilities=ARRAY['terminal_host'] WHERE id=$1`, []any{host}, []string{host, "account:" + owner}, false},
		{"member role", `UPDATE paperboat.team_members SET role='auditor' WHERE team_id=$1 AND account_id=$2`, []any{team, member}, teamKeys, false},
		{"member sign-in only", `UPDATE paperboat.team_members SET sso_verified_at=now() WHERE team_id=$1 AND account_id=$2`, []any{team, member}, []string{"account:" + member}, true},
		{"member joins", `INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'member',true)`, []any{team, other}, append([]string{"account:" + other}, teamKeys...), false},
		{"member leaves", `DELETE FROM paperboat.team_members WHERE team_id=$1 AND account_id=$2`, []any{team, other}, append([]string{"account:" + other}, teamKeys...), false},
		{"binding generation", `UPDATE paperboat.team_resource_bindings SET generation=generation+1 WHERE team_id=$1 AND resource_id=$2`, []any{team, host}, teamKeys, false},
		{"terminal session", `INSERT INTO paperboat.user_machine_terminal_sessions(id,user_machine_id,owner_account,name,launch_cwd) VALUES($1,$2,$3,'one','/')`, []any{terminal, host, owner}, []string{host, "account:" + owner}, false},
		{"terminal session activity only", `UPDATE paperboat.user_machine_terminal_sessions SET last_activity_at=now() WHERE id=$1`, []any{terminal}, []string{host}, true},
		{"terminal binding", `INSERT INTO paperboat.team_resource_bindings(team_id,resource_kind,resource_id,owner_account) VALUES($1,'terminal_session',$2,$3)`, []any{team, terminal, owner}, teamKeys, false},
		{"terminal grant", `INSERT INTO paperboat.team_terminal_session_grants(team_id,resource_kind,terminal_session_id,audience,account_id,role,generation,active) VALUES($1,'terminal_session',$2,'all_members',NULL,'viewer',1,true)`, []any{team, terminal}, teamKeys, false},
		{"terminal grant withdrawn", `UPDATE paperboat.team_terminal_session_grants SET active=false,generation=2 WHERE team_id=$1`, []any{team}, teamKeys, false},
		{"workspace policy", `INSERT INTO paperboat.workspace_policies(team_id,version,source,digest) VALUES($1,1,'x','d')`, []any{team}, teamKeys, false},
		{"workspace policy rule", `INSERT INTO paperboat.workspace_policy_rules(policy_id,rule,deny,subject_kind,subject_id,target_kind,target_id,action,ports) SELECT id,1,false,'all','','machine',$2,'terminal','[1,65536)'::int4range FROM paperboat.workspace_policies WHERE team_id=$1`, []any{team, host}, teamKeys, false},
		{"workspace policy rule removed", `DELETE FROM paperboat.workspace_policy_rules WHERE policy_id=(SELECT id FROM paperboat.workspace_policies WHERE team_id=$1)`, []any{team}, teamKeys, false},
		{"personal workspace policy", `INSERT INTO paperboat.workspace_policies(owner_account,version,source,digest) VALUES($1,1,'x','d')`, []any{owner}, []string{"account:" + owner}, false},
		{"recording key", `INSERT INTO paperboat.recording_keys(team_id,generation,public_key,signing_public_key,created_by) VALUES($1,1,decode(repeat('00',32),'hex'),decode(repeat('00',32),'hex'),$2)`, []any{team, owner}, teamKeys, false},
		{"team grant", `WITH b AS (INSERT INTO paperboat.team_resource_bindings(team_id,resource_kind,resource_id,owner_account) VALUES($1,'env',$1||'_env',$2)) SELECT 1`, []any{team, owner}, teamKeys, false},
		{"team grant row", `INSERT INTO paperboat.team_resource_grants(team_id,account_id,resource_kind,resource_id,permission,generation,active) VALUES($1,$2,'env',$1||'_env','read',1,true)`, []any{team, member}, teamKeys, false},
		{"team billing period", `INSERT INTO paperboat.billing_workspaces(id,team_id,state,period_start,period_end) VALUES($1||'_billing',$1,'active',now()-interval '1 hour',now()+interval '1 hour')`, []any{team}, teamKeys, false},
		{"team billing period ends", `UPDATE paperboat.billing_workspaces SET period_end=now() WHERE team_id=$1`, []any{team}, teamKeys, false},
		{"team suspended", `UPDATE paperboat.teams SET suspended=true,suspended_at=now(),suspended_by=$2 WHERE team_id=$1`, []any{team, owner}, teamKeys, false},
		{"access session", `INSERT INTO paperboat.user_machine_access_sessions(credential_kind,id,user_machine_id,user_id,environment_id,cli_client_session_id,http_base_url,helper_terminal_session_id,helper_file_session_id,capabilities,operation_id,expires_at) VALUES('external_issuer',$1||'_as',$1,$2,$3,$4,'https://m.example.test',$1||'_t',$1||'_f',ARRAY['exec'],$1||'_op',now()+interval '5 minutes')`, []any{host, owner, env, cli}, []string{host, cli}, false},
		{"access session leaves the account's other documents", `UPDATE paperboat.user_machine_access_sessions SET expires_at=expires_at+interval '1 second' WHERE id=$1||'_as'`, []any{host}, []string{"account:" + owner}, true},
		{"access session revoked", `UPDATE paperboat.user_machine_access_sessions SET state='revoked',revoked_at=now(),revocation_reason='test' WHERE id=$1||'_as'`, []any{host}, []string{host, cli}, false},
		{"recording admission", `INSERT INTO paperboat.recording_native_admissions(id,recording_id,account_id,native_cli_session_id,machine_id,expires_at) VALUES($1||'_ra','rec',$2,$3,$1,now()+interval '1 minute')`, []any{host, owner, cli}, []string{host, cli}, false},
		{"lazy access policy", `INSERT INTO paperboat.lazy_access_policies(id,hostname,account_id,environment_id,machine_id,installation_generation,generation,target_scheme,target_address,access_mode,ownership_mode,expires_at,authorization_source) VALUES($1||'_lz',replace($1,'_','-')||'.local.example.test',$2,$3,$1,1,1,'http','127.0.0.1:80','private','persistent_port',now()+interval '1 hour','explicit')`, []any{host, owner, env}, []string{host, "account:" + owner}, false},
		{"inspector credential", `INSERT INTO paperboat.inspector_credentials(credential_id,token_hash,account_id,resource_kind,resource_id,route_id,action,resource_generation,route_generation,target_generation,owner_account_id,issued_at,expires_at,native_cli_session_id,native_machine_id,native_machine_generation) VALUES($1||'_ic',sha256($1::bytea),$2,'preview',$1||'_pv','rt','inspect',1,1,1,$3,now(),now()+interval '1 minute',$4,$1,1)`, []any{host, member, owner, cli}, []string{host, cli}, false},
		// The inspected preview's lease: a renewal notifies nothing (the grant's expiry already bounds the document), its
		// end names the credential's endpoint and machine at once instead of leaving the grant until it expires.
		{"inspected preview lease", `INSERT INTO paperboat.preview_leases(owner_session_kind,id,endpoint,account_id,actor_id,owner_machine_id,owner_session_id,target_scheme,target_address,access_mode,lease_deadline) VALUES('foreground',$1||'_pv','https://'||replace($1,'_','-')||'.example.test',$2,$2,$1,'certificate-owner','http','127.0.0.1:3000','public',now()+interval '1 hour')`, []any{host, owner}, []string{host, cli}, true},
		{"inspected preview lease renewed", `UPDATE paperboat.preview_leases SET lease_deadline=lease_deadline+interval '1 hour' WHERE id=$1||'_pv'`, []any{host}, []string{host, cli}, true},
		{"inspected preview lease stopped", `UPDATE paperboat.preview_leases SET terminal_state='stopped',stopped_at=now() WHERE id=$1||'_pv'`, []any{host}, []string{host, cli}, false},
		{"config repository and assignment", `WITH r AS (INSERT INTO paperboat.control_config_repositories(id,owner_user_id,provider,external_ref,display_name) VALUES($1,$2,'github','x/y','y'))
 INSERT INTO paperboat.control_config_assignments(id,environment_id,repository_id,mode,consent_state,machine_id) VALUES($3,$4,$1,'pull_only','revoked',$5)`, []any{repo, owner, assignment, env, host}, []string{host, "account:" + owner}, false},
		{"config assignment version", `UPDATE paperboat.control_config_assignments SET version=version+1 WHERE id=$1`, []any{assignment}, []string{host, "account:" + owner}, false},
		{"config sync status", `INSERT INTO paperboat.control_config_sync_statuses(environment_id,repository_id,assignment_id,machine_id,installation_generation,warning_revision,policy_revision,sync_revision,state,machine_updated_at) VALUES($1,$2,$3,$4,1,'w','p',0,'healthy',now())`, []any{env, repo, assignment, host}, []string{host, "account:" + owner}, false},
		{"config sync status observed only", `UPDATE paperboat.control_config_sync_statuses SET observed_at=now() WHERE assignment_id=$1`, []any{assignment}, []string{host}, true},
		{"config conflict", `UPDATE paperboat.control_config_sync_statuses SET conflicts='[{"path":"/a","revision":"r"}]'::jsonb WHERE assignment_id=$1`, []any{assignment}, []string{host, "account:" + owner}, false},
		{"config compare admission", `INSERT INTO paperboat.config_compare_admissions(id,operation_id,account_id,environment_id,machine_id,installation_generation,assignment_id,assignment_version,config_path,conflict_revision,expected_remote_revision,native_cli_session_id,source_machine_id,workspace_id,expires_at) SELECT $1||'_cc','op',$2,$3,$1,1,$4,version,'/a',repeat('a',64),'r',$5,$1,'personal',now()+interval '1 minute' FROM paperboat.control_config_assignments WHERE id=$4`, []any{host, owner, env, assignment, cli}, []string{host, cli}, false},
		{"CLI session revoked", `UPDATE paperboat.cli_client_sessions SET state='revoked',revoked_at=now() WHERE id=$1`, []any{cli}, []string{cli, "account:" + owner}, false},
		{"CLI session last used only", `UPDATE paperboat.cli_client_sessions SET last_used_at=now() WHERE id=$1`, []any{cli}, []string{cli}, true},
		{"machine deleted", `DELETE FROM paperboat.user_machines WHERE id=$1`, []any{host}, []string{host, "account:" + owner}, false},
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if _, listening := service.cache.begin(); listening {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("policy listener did not attach")
		}
	}
	for _, c := range cases {
		// The documents a write must drop: fill the cache with one per party, then write.
		service.cache.reset(true)
		start, _ := service.cache.begin()
		for _, key := range c.want {
			service.cache.put("doc:"+key, &policyDoc{validUntil: time.Now().Add(time.Hour), keys: []string{key}}, start)
		}
		mu.Lock()
		clear(seen)
		mu.Unlock()
		exec(c.query, c.args...)
		if err := service.policySync(ctx); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		got := map[string]bool{}
		for k := range seen {
			got[k] = true
		}
		mu.Unlock()
		for _, key := range c.want {
			cached := service.cache.get("doc:"+key, "", time.Now()) != nil
			if c.none && (got[key] || !cached) {
				t.Errorf("%s: notified %s, which it cannot affect (seen %v)", c.name, key, seenKeys(got))
			}
			if !c.none && (!got[key] || cached) {
				t.Errorf("%s: did not notify %s (seen %v)", c.name, key, seenKeys(got))
			}
		}
	}
}

func seenKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		if !strings.HasPrefix(k, "policy-sync:") {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out
}

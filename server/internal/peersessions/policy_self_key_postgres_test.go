package peersessions

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"
)

// An access session drops exactly the cached documents of its two endpoints (migration 524), whatever the account's
// size; a revoked machine, CLI session or identity still drops every document of the account, which all name it.
func TestAccessSessionDropsOnlyItsOwnPairsDocumentsOnPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	store := openMeshTestStore(t, ctx)
	s := fmt.Sprint(time.Now().UnixNano())
	account, cli, session := "sk_"+s, "sk_cli_"+s, "sk_as_"+s
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("%v\n%s", err, query)
		}
	}
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, account)
	t.Cleanup(func() {
		_, _ = store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.users WHERE id=$1`, account)
	})
	const machines = 299
	seedMeshMachines(t, ctx, store, account, 0, machines-1)
	exec(`INSERT INTO paperboat.cli_client_sessions(id,user_id,client_id,client_label,device_type,os,scopes,state,created_at,approved_at) VALUES($1,$2,$1,'t','desktop','test',ARRAY['machines:connect'],'active',now(),now())`, cli, account)
	exec(`INSERT INTO paperboat.peer_endpoint_certificates(fingerprint,user_id,endpoint_id,role,generation,serial,certificate,quic_public_key,issued_at,expires_at,key_id,signer_public_key)
VALUES(sha256(convert_to($1,'UTF8')),$2,$1,'cli',1,1,decode(repeat('00',200),'hex'),sha256(convert_to('key'||$1,'UTF8')),now()-interval '1 minute',now()+interval '1 day','key',sha256(convert_to('key'||$1,'UTF8')))`, cli, account)
	exec(`INSERT INTO paperboat.peer_network_identities(user_id,endpoint_id,role,machine_id,endpoint_generation,machine_generation,key_generation,wireguard_public_key,disco_public_key,quic_certificate_fingerprint,virtual_address,created_at,updated_at)
VALUES($2,$1,'cli',NULL,1,0,1,sha256(convert_to('key'||$1,'UTF8')),sha256(convert_to('disco'||$1,'UTF8')),sha256(convert_to($1,'UTF8')),('fd7a:115c:a1e0::'::inet + nextval('paperboat.peer_network_virtual_address_seq')),now(),now())`, cli, account)
	endpoints := []string{cli}
	for i := range machines {
		endpoints = append(endpoints, fmt.Sprintf("%s_m_%d", account, i))
	}
	host, third := endpoints[1], endpoints[2]

	service, err := NewNetworkService(store, meshTestSigner{}, "https://issuer.example.test")
	if err != nil {
		t.Fatal(err)
	}
	usePolicyCache(t, service, store)
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if _, listening := service.cache.begin(); listening {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("policy listener did not attach")
		}
	}
	request := func(endpoint string) NetworkConfigRequest {
		return NetworkConfigRequest{OperationID: "sk_config_" + s, UserID: account, EndpointID: endpoint, Authority: AuthorityUntilRevoked}
	}
	fill := func(ids ...string) {
		t.Helper()
		for _, id := range ids {
			if _, err := service.Configuration(ctx, request(id)); err != nil {
				t.Fatalf("%s: %v", id, err)
			}
		}
	}
	missing := func() []string {
		t.Helper()
		if err := service.policySync(ctx); err != nil {
			t.Fatal(err)
		}
		service.cache.mu.Lock()
		defer service.cache.mu.Unlock()
		var out []string
		for _, id := range endpoints {
			if service.cache.docs[id] == nil {
				out = append(out, id)
			}
		}
		return out
	}
	doc := func(id string) *policyDoc {
		t.Helper()
		service.cache.mu.Lock()
		defer service.cache.mu.Unlock()
		if d := service.cache.docs[id]; d != nil {
			return d
		}
		t.Fatalf("%s has no cached document", id)
		return nil
	}
	digest := func(d *policyDoc) [32]byte {
		return answerDigest(configurationClaims(service.issuer, time.Now(), d.expiry, 0, d.self, d.peers, 0), nil, "")
	}
	granted := func(id string) bool {
		for _, p := range doc(id).peers {
			for _, scope := range p.Scopes {
				if scope.ResourceKind == "machine_access" && scope.ResourceID == session {
					return true
				}
			}
		}
		return false
	}
	fill(endpoints...)
	fill(endpoints...) // a write of the first round (the mesh's first build) may have dropped a document
	if left := missing(); len(left) != 0 {
		t.Fatalf("%d of %d documents are not cached", len(left), len(endpoints))
	}

	pair := []string{cli, host}
	for _, step := range []struct {
		name, query string
		args        []any
		granted     bool
	}{
		{"start", `INSERT INTO paperboat.user_machine_access_sessions(credential_kind,id,user_machine_id,user_id,environment_id,cli_client_session_id,http_base_url,helper_terminal_session_id,helper_file_session_id,capabilities,operation_id,expires_at) VALUES('control_plane_signed',$1,$2,$3,$3||'_env_0',$4,'https://m.example.test',$1||'_t',$1||'_f',ARRAY['exec'],$1||'_op',now()+interval '5 minutes')`, []any{session, host, account, cli}, true},
		{"its wake-up of the host", `SELECT paperboat.publish_access_session_topic(ARRAY[$1::text])`, []any{host}, true},
		{"end", `UPDATE paperboat.user_machine_access_sessions SET state='revoked',revoked_at=now(),revocation_reason='test' WHERE id=$1`, []any{session}, false},
		{"restart", `UPDATE paperboat.user_machine_access_sessions SET state='active',revoked_at=NULL,revocation_reason=NULL WHERE id=$1`, []any{session}, true},
		{"deletion", `DELETE FROM paperboat.user_machine_access_sessions WHERE id=$1`, []any{session}, false},
	} {
		exec(step.query, step.args...)
		want := pair
		if step.name == "its wake-up of the host" {
			want = nil
		}
		if got := missing(); !slices.Equal(got, want) {
			t.Fatalf("session %s dropped %d documents, want %v", step.name, len(got), want)
		}
		fill(pair...)
		for _, id := range pair {
			if granted(id) != step.granted {
				t.Fatalf("after session %s, %s's grant is %t, want %t", step.name, id, !step.granted, step.granted)
			}
		}
		fresh, err := service.buildPolicy(ctx, request(third), nil, time.Now().UTC())
		if err != nil {
			t.Fatal(err)
		}
		if digest(doc(third)) != digest(fresh) {
			t.Fatalf("after session %s a third endpoint's cached document differs from a fresh one", step.name)
		}
	}

	last := endpoints[len(endpoints)-1]
	for _, step := range []struct{ name, query, id string }{
		{"identity rotated", `UPDATE paperboat.peer_network_identities SET disco_public_key=decode(repeat('05',32),'hex') WHERE endpoint_id=$1`, host},
		{"identity revoked", `UPDATE paperboat.peer_network_identities SET revoked_at=now() WHERE endpoint_id=$1`, endpoints[len(endpoints)-2]},
		{"CLI session revoked", `UPDATE paperboat.cli_client_sessions SET state='revoked',revoked_at=now() WHERE id=$1`, cli},
		{"machine revoked", `UPDATE paperboat.user_machines SET state='revoked',revoked_at=now(),seat_state='released' WHERE id=$1`, last},
	} {
		exec(step.query, step.id)
		if got := missing(); len(got) != len(endpoints) {
			t.Fatalf("%s left %d documents cached", step.name, len(endpoints)-len(got))
		}
		for _, id := range endpoints { // a revoked endpoint is refused and stays uncached: stand a document in for it
			if _, err := service.Configuration(ctx, request(id)); err != nil {
				start, _ := service.cache.begin()
				service.cache.put(id, &policyDoc{validUntil: time.Now().Add(time.Hour), keys: []string{"account:" + account}}, start)
			}
		}
		if left := missing(); len(left) != 0 {
			fill(left...)
		}
	}
}

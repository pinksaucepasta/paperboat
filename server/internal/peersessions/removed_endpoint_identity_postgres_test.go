package peersessions

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
)

// A removed machine or an ended CLI session keeps no working peer-network identity, whichever
// code path removed it.
func TestRemovedEndpointsLoseTheirPeerIdentityPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("isolated PostgreSQL required")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	x := fmt.Sprint(time.Now().UnixNano())
	user := "peer_revoke_u_" + x
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = store.SQL().ExecContext(bg, `DELETE FROM paperboat.users WHERE id=$1`, user)
	})
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, user)
	var counter uint32
	endpoint := func(id, role string) {
		t.Helper()
		counter++
		fingerprint, key := make([]byte, 32), make([]byte, 32)
		copy(fingerprint, id)
		binary.BigEndian.PutUint32(fingerprint[28:], counter)
		copy(key, "k"+id)
		binary.BigEndian.PutUint32(key[28:], counter)
		exec(`INSERT INTO paperboat.peer_endpoint_certificates(fingerprint,user_id,endpoint_id,role,generation,serial,certificate,quic_public_key,issued_at,expires_at,key_id,signer_public_key) VALUES($1,$2,$3,$4,1,1,$5,$6,now()-interval '1 hour',now()+interval '1 hour','key',$6)`, fingerprint, user, id, role, make([]byte, 200), key)
		machine, generation := any(nil), 0
		if role == "machine" {
			machine, generation = id, 1
		}
		exec(`INSERT INTO paperboat.peer_network_identities(user_id,endpoint_id,role,machine_id,endpoint_generation,machine_generation,key_generation,wireguard_public_key,quic_certificate_fingerprint,virtual_address,created_at,updated_at) VALUES($1,$2,$3,$4,1,$5,1,$6,$7,$8::inet,now(),now())`, user, id, role, machine, generation, key, fingerprint, fmt.Sprintf("fd7a:115c:a1e0::%x", 0x7000+counter+uint32(time.Now().UnixNano()%1000)))
	}
	machineRemoved, machineKept, cliEnded, cliKept := "peer_revoke_mr_"+x, "peer_revoke_mk_"+x, "peer_revoke_ce_"+x, "peer_revoke_ck_"+x
	for _, id := range []string{machineRemoved, machineKept} {
		exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,platform,architecture,workspace_root,state,seat_state,online,installation_generation) VALUES($1,$2,$3,'linux','amd64','/w','online','occupied',true,1)`, id, user, "env_"+id)
		endpoint(id, "machine")
	}
	for _, id := range []string{cliEnded, cliKept} {
		exec(`INSERT INTO paperboat.cli_client_sessions(id,user_id,client_id,client_label,device_type,os,scopes,state,created_at,approved_at) VALUES($1,$2,'c','x','desktop','t',ARRAY['machines:connect'],'active',now(),now())`, id, user)
		endpoint(id, "cli")
	}
	revoked := func(id string) bool {
		var value bool
		if err := store.SQL().QueryRowContext(ctx, `SELECT revoked_at IS NOT NULL FROM paperboat.peer_network_identities WHERE endpoint_id=$1`, id).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	exec(`UPDATE paperboat.user_machines SET state='deleted',seat_state='released',deleted_at=now(),online=false WHERE id=$1`, machineRemoved)
	exec(`UPDATE paperboat.cli_client_sessions SET state='revoked',revoked_at=now() WHERE id=$1`, cliEnded)
	if !revoked(machineRemoved) || !revoked(cliEnded) {
		t.Fatalf("a removed endpoint kept its identity: machine=%v cli=%v", revoked(machineRemoved), revoked(cliEnded))
	}
	if revoked(machineKept) || revoked(cliKept) {
		t.Fatalf("a live endpoint lost its identity: machine=%v cli=%v", revoked(machineKept), revoked(cliKept))
	}
}

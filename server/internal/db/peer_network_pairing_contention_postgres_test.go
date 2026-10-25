package db

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
)

type peerContentionFixture struct {
	store   *DB
	ctx     context.Context
	user    string
	machine string // the machine being paired: no peer identity yet
	peers   []string
}

// An account with many enrolled machines, each with a peer identity, and one released machine to pair.
func newPeerContentionFixture(t *testing.T) peerContentionFixture {
	t.Helper()
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("requires isolated PostgreSQL")
	}
	if err := ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	if err := Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	s := fmt.Sprint(time.Now().UnixNano())
	f := peerContentionFixture{store: store, ctx: ctx, user: "pc_user_" + s, machine: "pc_pairing_" + s}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("%v\n%s", err, query)
		}
	}
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, f.user)
	exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,online)
VALUES($1,$2,$1,'pairing','linux','amd64','/w','pending','released',false)`, f.machine, f.user)
	net := sha256.Sum256([]byte(s)) // a /24 of the CGNAT range per fixture, for the unique virtual addresses
	for i := 0; i < 60; i++ {
		peer := fmt.Sprintf("pc_peer_%d_%s", i, s)
		f.peers = append(f.peers, peer)
		exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,online,installation_generation)
VALUES($1,$2,$1,$3,'linux','amd64','/w','online','occupied',true,1)`, peer, f.user, fmt.Sprintf("peer-%d", i))
		fingerprint := sha256.Sum256([]byte(peer))
		key := sha256.Sum256([]byte("key" + peer))
		exec(`INSERT INTO paperboat.peer_endpoint_certificates(fingerprint,user_id,endpoint_id,role,generation,serial,certificate,quic_public_key,issued_at,expires_at,key_id,signer_public_key)
VALUES($1,$2,$3,'machine',1,1,$4,$5,now()-interval '1 minute',now()+interval '1 day','key',$5)`, fingerprint[:], f.user, peer, make([]byte, 200), key[:])
		exec(`INSERT INTO paperboat.peer_network_identities(user_id,endpoint_id,role,machine_id,endpoint_generation,machine_generation,key_generation,
wireguard_public_key,quic_certificate_fingerprint,virtual_address,created_at,updated_at) VALUES($1,$2,'machine',$2,1,1,1,$3,$4,$5::inet,now(),now())`,
			f.user, peer, key[:], fingerprint[:], fmt.Sprintf("100.%d.%d.%d", 64+int(net[0])%64, net[1], i+1))
	}
	exec(`ANALYZE paperboat.peer_network_identities`)
	return f
}

// Pairing occupies the machine's seat, which fires the peer-network trigger. It must lock only the identities it
// changes (none for a machine without one yet), not every identity of the account.
func TestPairingSeatUpdateLocksOnlyTheIdentitiesItChangesOnPostgres(t *testing.T) {
	f := newPeerContentionFixture(t)
	tx, err := f.store.Pool().Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(f.ctx, `SET TRANSACTION ISOLATION LEVEL SERIALIZABLE`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(f.ctx, `UPDATE paperboat.user_machines SET seat_state='occupied' WHERE id=$1 AND user_id=$2`, f.machine, f.user); err != nil {
		t.Fatal(err)
	}
	var heapLocks int
	var kinds string
	if err := tx.QueryRow(f.ctx, `SELECT count(*),coalesce(string_agg(DISTINCT l.locktype,','),'') FROM pg_locks l JOIN pg_class c ON c.oid=l.relation
WHERE l.pid=pg_backend_pid() AND l.mode='SIReadLock' AND c.relname='peer_network_identities'`).Scan(&heapLocks, &kinds); err != nil {
		t.Fatal(err)
	}
	if heapLocks != 0 {
		t.Fatalf("pairing's seat update holds %d predicate locks (%s) on the account's peer identities; it changes none", heapLocks, kinds)
	}
}

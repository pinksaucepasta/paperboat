package peersessions

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
)

type meshTestSigner struct{}

func (meshTestSigner) SignNetworkConfiguration(map[string]any) (string, error) { return "config", nil }
func (meshTestSigner) SignRegionalCandidates(map[string]any) (string, error)   { return "candidates", nil }
func (meshTestSigner) SignRelayGrant(map[string]any) (string, error)          { return "grant", nil }

// Every endpoint of an account rebuilt the whole personal mesh (3 queries per peer) on every change: N x N per
// join. The shared mesh is built once per change: on a 1,000-machine account a join writes one log row, a refresh
// with no change writes none, and every entry is exactly what loadNetworkBinding gives for that endpoint.
func TestMeshJoinOnAThousandMachineAccountWritesOneLogRowOnPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("requires isolated PostgreSQL")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	s := fmt.Sprint(time.Now().UnixNano())
	account := "mesh_user_" + s
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
	// One /16 of the CGNAT range per fixture for unique virtual addresses.
	net := sha256.Sum256([]byte(s))
	join := func(from, to int) {
		t.Helper()
		exec(`INSERT INTO paperboat.control_environments(id,workspace_id,owner_user_id,desired_state,desired_version,applied_state,applied_version,created_at,updated_at)
SELECT $1||'_env_'||g,$1||'_env_'||g,$1,'active',1,'ready',1,now(),now() FROM generate_series($2::int,$3::int) g`, account, from, to)
		exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,online,installation_generation)
SELECT $1||'_m_'||g,$1,$1||'_env_'||g,'m-'||g,'linux','amd64','/w','online','occupied',true,1 FROM generate_series($2::int,$3::int) g`, account, from, to)
		exec(`INSERT INTO paperboat.peer_endpoint_certificates(fingerprint,user_id,endpoint_id,role,generation,serial,certificate,quic_public_key,issued_at,expires_at,key_id,signer_public_key)
SELECT sha256(convert_to($1||'_m_'||g,'UTF8')),$1,$1||'_m_'||g,'machine',1,1,decode(repeat('00',200),'hex'),sha256(convert_to('key'||$1||'_m_'||g,'UTF8')),now()-interval '1 minute',now()+interval '1 day','key',
 sha256(convert_to('key'||$1||'_m_'||g,'UTF8')) FROM generate_series($2::int,$3::int) g`, account, from, to)
		exec(`INSERT INTO paperboat.peer_network_identities(user_id,endpoint_id,role,machine_id,endpoint_generation,machine_generation,key_generation,
wireguard_public_key,quic_certificate_fingerprint,virtual_address,created_at,updated_at)
SELECT $1,$1||'_m_'||g,'machine',$1||'_m_'||g,1,1,1,sha256(convert_to('key'||$1||'_m_'||g,'UTF8')),sha256(convert_to($1||'_m_'||g,'UTF8')),
 ('100.'||(64+$4::int)||'.'||(g/250)||'.'||(1+g%250))::inet,now(),now() FROM generate_series($2::int,$3::int) g`, account, from, to, int(net[0])%64)
	}
	join(0, 999)

	service, err := NewNetworkService(store, meshTestSigner{}, "https://issuer.example.test", NetworkOptions{})
	if err != nil {
		t.Fatal(err)
	}
	logRows := func() (rows int, last string) {
		t.Helper()
		if err := store.SQL().QueryRowContext(ctx, `SELECT count(*),coalesce((SELECT op||' '||endpoint_id FROM paperboat.peer_mesh_log WHERE account_id=$1 ORDER BY mesh_seq DESC LIMIT 1),'')
 FROM paperboat.peer_mesh_log WHERE account_id=$1`, account).Scan(&rows, &last); err != nil {
			t.Fatal(err)
		}
		return rows, last
	}
	seq, err := service.refreshMesh(ctx, account)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := logRows(); n != 1000 || seq != 1000 {
		t.Fatalf("first build: %d log rows, seq %d; want 1000 adds", n, seq)
	}

	// Every shared entry is what the per-endpoint loader gives that endpoint's peers.
	tx, err := store.SQL().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	mesh, err := latestMeshEntries(ctx, tx, account, seq, nil)
	if err != nil {
		t.Fatal(err)
	}
	for id, entry := range mesh {
		want, _, err := loadNetworkBinding(ctx, tx, account, id, service.now().UTC(), false)
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		if !sameBinding(entry, want) {
			t.Fatalf("%s: mesh entry %+v, loadNetworkBinding %+v", id, entry, want)
		}
	}
	_ = tx.Rollback()

	if again, err := service.refreshMesh(ctx, account); err != nil || again != seq {
		t.Fatalf("refresh without a change: seq %d -> %d, %v", seq, again, err)
	}
	if n, _ := logRows(); n != 1000 {
		t.Fatalf("a refresh without a change wrote %d rows", n-1000)
	}

	join(1000, 1000)
	if seq, err = service.refreshMesh(ctx, account); err != nil || seq != 1001 {
		t.Fatalf("after a join: seq %d, %v; want one more entry", seq, err)
	}
	if n, last := logRows(); n != 1001 || last != "add "+account+"_m_1000" {
		t.Fatalf("a join on a 1,000-machine account wrote %d rows (last %q), want exactly one add", n-1000, last)
	}

	exec(`UPDATE paperboat.user_machines SET revoked_at=now(),state='revoked',seat_state='released' WHERE id=$1`, account+"_m_7")
	if seq, err = service.refreshMesh(ctx, account); err != nil || seq != 1002 {
		t.Fatalf("after a revoke: seq %d, %v", seq, err)
	}
	if n, last := logRows(); n != 1002 || last != "remove "+account+"_m_7" {
		t.Fatalf("a revoke wrote %d rows (last %q), want exactly one remove", n-1001, last)
	}
}

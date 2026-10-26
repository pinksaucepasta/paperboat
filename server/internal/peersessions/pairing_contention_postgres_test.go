package peersessions

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/billing"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
)

// The production failure: while an account's daemons refreshed their peer configuration, pairing's seat
// reservation failed all its serialization retries. Pairing (the enrollment transaction's seat section:
// billing ReserveUserMachineSeat + OccupyUserMachineSeat, as usermachines runs them), 4 at a time, must succeed
// while 6 daemons call the production refresh (NetworkService.Configuration) without pause, for at least 200
// refreshes.
func TestPairingSurvivesConcurrentPeerConfigRefreshesOnPostgres(t *testing.T) {
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
	account := "pairing_user_" + s
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("%v\n%s", err, query)
		}
	}
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, account)
	// 60 paired machines with peer identities (the daemons) and 20 released machines waiting to be paired.
	net := sha256.Sum256([]byte(s))
	exec(`INSERT INTO paperboat.control_environments(id,workspace_id,owner_user_id,desired_state,desired_version,applied_state,applied_version,created_at,updated_at)
SELECT $1||'_env_'||g,$1||'_env_'||g,$1,'active',1,'ready',1,now(),now() FROM generate_series(0,79) g`, account)
	exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,online,installation_generation)
SELECT $1||'_m_'||g,$1,$1||'_env_'||g,'m-'||g,'linux','amd64','/w',
 CASE WHEN g<60 THEN 'online' ELSE 'pending' END,CASE WHEN g<60 THEN 'occupied' ELSE 'released' END,g<60,1 FROM generate_series(0,79) g`, account)
	exec(`INSERT INTO paperboat.peer_endpoint_certificates(fingerprint,user_id,endpoint_id,role,generation,serial,certificate,quic_public_key,issued_at,expires_at,key_id,signer_public_key)
SELECT sha256(convert_to($1||'_m_'||g,'UTF8')),$1,$1||'_m_'||g,'machine',1,1,decode(repeat('00',200),'hex'),sha256(convert_to('key'||$1||'_m_'||g,'UTF8')),now()-interval '1 minute',now()+interval '1 day','key',
 sha256(convert_to('key'||$1||'_m_'||g,'UTF8')) FROM generate_series(0,59) g`, account)
	exec(`INSERT INTO paperboat.peer_network_identities(user_id,endpoint_id,role,machine_id,endpoint_generation,machine_generation,key_generation,
wireguard_public_key,quic_certificate_fingerprint,virtual_address,created_at,updated_at)
SELECT $1,$1||'_m_'||g,'machine',$1||'_m_'||g,1,1,1,sha256(convert_to('key'||$1||'_m_'||g,'UTF8')),sha256(convert_to($1||'_m_'||g,'UTF8')),
 ('100.'||(64+$2::int)||'.'||$3::int||'.'||(1+g))::inet,now(),now() FROM generate_series(0,59) g`, account, int(net[0])%64, int(net[1]))

	service, err := NewNetworkService(store, meshTestSigner{}, "https://issuer.example.test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Configuration(ctx, NetworkConfigRequest{OperationID: "pairing_probe_" + s, UserID: account, EndpointID: account + "_m_0"}); err != nil {
		t.Fatalf("the daemons' refresh does not work on this fixture: %v", err)
	}
	stop := make(chan struct{})
	var refreshes atomic.Int64
	var refreshers sync.WaitGroup
	for w := 0; w < 6; w++ {
		refreshers.Add(1)
		go func() {
			defer refreshers.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := service.Configuration(ctx, NetworkConfigRequest{OperationID: fmt.Sprintf("pairing_refresh_%d_%d_%s", w, i, s), UserID: account, EndpointID: fmt.Sprintf("%s_m_%d", account, w)}); err == nil {
					refreshes.Add(1)
				}
			}
		}()
	}
	seats := billing.NewWorkspaceService(store, nil, "")
	pair := func(machine string) error {
		return store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
			if err := seats.ReserveUserMachineSeat(ctx, tx, account); err != nil {
				return err
			}
			n, err := tx.Queries().OccupyUserMachineSeat(ctx, dbsqlc.OccupyUserMachineSeatParams{ID: machine, UserID: account})
			if err == nil && n != 1 {
				err = fmt.Errorf("occupied %d seats for %s", n, machine)
			}
			return err
		})
	}
	// Rounds of 20 pairings (4 at a time) until at least 200 refreshes ran alongside them; between rounds the 20
	// machines are released again (setup, not under test).
	paired, failed := 0, 0
	var first error
	deadline := time.Now().Add(2 * time.Minute)
	for refreshes.Load() < 200 && time.Now().Before(deadline) {
		var round sync.WaitGroup
		var mu sync.Mutex
		gate := make(chan struct{}, 4)
		for i := 0; i < 20; i++ {
			round.Add(1)
			gate <- struct{}{}
			go func() {
				defer func() { <-gate; round.Done() }()
				err := pair(fmt.Sprintf("%s_m_%d", account, 60+i))
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					failed, first = failed+1, err
				} else {
					paired++
				}
			}()
		}
		round.Wait()
		exec(`UPDATE paperboat.user_machines SET seat_state='released' WHERE user_id=$1 AND id>=$1||'_m_60' AND id<=$1||'_m_79' AND seat_state='occupied' AND state='pending'`, account)
	}
	close(stop)
	refreshers.Wait()
	if failed > 0 {
		t.Fatalf("%d of %d pairings failed under %d concurrent peer-config refreshes: %v", failed, failed+paired, refreshes.Load(), first)
	}
	if refreshes.Load() < 200 || paired < 20 {
		t.Fatalf("only %d pairings under %d refreshes; the test did not exercise contention", paired, refreshes.Load())
	}
	t.Logf("%d pairings under %d refreshes", paired, refreshes.Load())
}

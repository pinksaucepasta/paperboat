package httpapi

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/machineservices"
	"github.com/pinksaucepasta/paperboat/server/internal/nativeprivateaccess"
	wire "github.com/pinksaucepasta/paperboat/shared/machineservices"
)

// 2026-11-11 03:37:46Z: 32 grants in 3 s from one CLI session exhausted the serializable retries of 4 of them
// (503). Concurrent grants of one session and service must all succeed and share one access session.
func TestNativePrivateGrantBurstSucceedsPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN")
	}
	if e := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); e != nil {
		t.Fatal(e)
	}
	application := fmt.Sprintf("native_burst_%d", time.Now().UnixNano())
	store, e := db.Open(config.Database{Driver: "postgres", DSN: db.TestDSNWithApplicationName(dsn, application)})
	if e != nil {
		t.Fatal(e)
	}
	defer store.Close()
	ctx := context.Background()
	if e := db.Migrate(ctx, store); e != nil {
		t.Fatal(e)
	}
	suffix := fmt.Sprint(time.Now().UnixNano())
	owner, machine, environment := "burst-owner-"+suffix, "burst-machine-"+suffix, "burst-env-"+suffix
	exec := func(q string, args ...any) {
		t.Helper()
		if _, e := store.SQL().ExecContext(ctx, q, args...); e != nil {
			t.Fatal(e)
		}
	}
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@invalid.test','active')`, owner)
	exec(`INSERT INTO paperboat.cli_client_sessions(id,user_id,client_id,client_label,device_type,os,scopes,state,created_at,approved_at) VALUES($1,$1,$1,'machine service','desktop','linux',ARRAY['machines:connect'],'active',now(),now())`, owner)
	exec(`INSERT INTO paperboat.control_environments(id,workspace_id,owner_user_id) VALUES($1,$1,$2)`, environment, owner)
	exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,online,installation_generation) VALUES($1,$2,$3,'office','linux','amd64','/workspace','online','occupied',true,1)`, machine, owner, environment)
	now := time.Now().UTC()
	snapshot := wire.Snapshot{Schema: "paperboat.machine-services/v1", InstallationGeneration: 1, BootID: "boot-012345678901", StartedAt: now, Generation: 1, Services: []wire.Port{{Port: 5432, AddressFamily: "ipv4"}}}
	body, _ := json.Marshal(map[string]any{"environment_id": environment, "resource_id": machine, "sampled_at": now, "machine_services": snapshot})
	r := httptest.NewRequest("POST", "/v1/runtime-observations", bytes.NewReader(body))
	r.Header.Set("Authorization", "Bearer current-runtime")
	r.Header.Set("X-Paperboat-Machine-Proof", base64.RawURLEncoding.EncodeToString([]byte("proof")))
	w := httptest.NewRecorder()
	runtimeObservation(&fakeRuntimeObservationRepository{}, &machineServiceProof{fakeRuntimeIdentity: &fakeRuntimeIdentity{}}, 10, machineservices.New(store, "local.example.test")).ServeHTTP(w, r)
	if w.Code != 202 {
		t.Fatalf("snapshot %d %s", w.Code, w.Body.String())
	}

	resolver, _ := nativeprivateaccess.NewSQLResolver(store)
	// A serializable transaction left open keeps the predicate locks of every serializable transaction that commits
	// meanwhile; grants take none (the pool exhausted them in a reconnect storm: 53200 and 40001, 503 to daemons).
	siReadLocks := func() (n int) {
		if e := store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM pg_locks l JOIN pg_stat_activity a ON a.pid=l.pid WHERE l.mode='SIReadLock' AND a.application_name=$1`, application).Scan(&n); e != nil {
			t.Fatal(e)
		}
		return n
	}
	held, e := store.SQL().BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if e != nil {
		t.Fatal(e)
	}
	defer held.Rollback()
	if _, e = held.ExecContext(ctx, `SET LOCAL enable_seqscan TO off`); e != nil {
		t.Fatal(e)
	}
	if e = held.QueryRowContext(ctx, `SELECT online FROM paperboat.user_machines WHERE id=$1`, machine).Scan(new(bool)); e != nil {
		t.Fatal(e)
	}
	if siReadLocks() == 0 {
		t.Fatal("a serializable read took no predicate lock the count can see: the check would pass vacuously")
	}
	lockedBefore := siReadLocks()
	const burst = 300
	errs := make([]error, burst)
	var wg sync.WaitGroup
	// The machine's own heartbeats rewrite the rows the grants read while they run, and keep its snapshot live (30 s) however long the burst takes.
	stop := make(chan struct{})
	var writers sync.WaitGroup
	for range 4 {
		writers.Add(1)
		go func() {
			defer writers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				store.SQL().ExecContext(ctx, `UPDATE paperboat.machine_service_snapshots SET expires_at=now()+interval '30 seconds' WHERE machine_id=$1`, machine)
				store.SQL().ExecContext(ctx, `UPDATE paperboat.user_machines SET online=true WHERE id=$1`, machine)
				time.Sleep(10 * time.Millisecond) // a real heartbeat is every ~15 s; a tight loop only adds host load
			}
		}()
	}
	// One session's burst must not use up the pool: an unrelated request made meanwhile still gets a connection.
	bystander := make(chan error, 1)
	go func() {
		time.Sleep(300 * time.Millisecond)
		probe, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		bystander <- store.SQL().QueryRowContext(probe, `SELECT 1`).Scan(new(int))
	}()
	for i := range burst {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = resolver.ResolveNativePrivate(ctx, nativeprivateaccess.Request{AccountID: owner, UserID: owner, CLIClientSessionID: owner, OperationID: fmt.Sprintf("burst-%d", i), ResourceKind: "machine_service", ResourceID: machine, RouteID: "tcp:5432", Protocol: "tcp"})
		}()
	}
	wg.Wait()
	close(stop)
	writers.Wait()
	if e := <-bystander; e != nil {
		t.Fatalf("an unrelated query during the burst: %v (the grants used up the pool)", e)
	}
	for i, e := range errs {
		if e != nil {
			t.Fatalf("grant %d of %d failed: %v", i, burst, e)
		}
	}
	if grown := siReadLocks() - lockedBefore; grown != 0 {
		t.Fatalf("%d grants kept %d predicate locks (want 0)", burst, grown)
	}
	var sessions int
	if e := store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.user_machine_access_sessions WHERE user_machine_id=$1`, machine).Scan(&sessions); e != nil || sessions != 1 {
		t.Fatalf("access sessions = %d, %v; want 1 shared", sessions, e)
	}
}

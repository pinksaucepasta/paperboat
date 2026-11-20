package machineevents

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// One change in a 50-machine account wakes only the machines whose view of it changed: a preview lease change only
// its owner machine (the host runtime that attaches it, and the observer that waits on the owner), a machine's own
// configuration change only that machine. Every other machine's held wait stays held, so it costs no topic re-read and
// no refetch. Prior art: NetBird computes the affected peers first and updates only them (management/server/peer.go:1605
// ExpandAndUpdateAffected, affectedpeers.Snapshot); Headscale queues a targeted change (TargetNode) for that node alone and
// a broadcast for all, filtering visibility when it renders (hscontrol/mapper/batcher.go:660-731). Audience-wide topics (inventory, machine_services, peer_network, private_routes) are not narrowed: each
// machine of the audience reads them.
func TestOneChangeInAFiftyMachineAccountWakesOnlyTheAffectedMachinesOnPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("requires isolated PostgreSQL")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	database, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err = db.Migrate(ctx, database); err != nil {
		t.Fatal(err)
	}
	stamp := fmt.Sprint(time.Now().UnixNano())
	account := "acct_fan_" + stamp
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := database.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("%v: %s", err, query)
		}
	}
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, account)
	t.Cleanup(func() {
		_, _ = database.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.users WHERE id=$1`, account)
	})
	exec(`INSERT INTO paperboat.user_machines (id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,last_seen_at)
SELECT $1||'_m'||g,$1,$1||'_e'||g,'m-'||g,'linux','amd64','/w','online','occupied',now() FROM generate_series(1,50) g`, account)
	machine := func(n int) string { return fmt.Sprintf("%s_m%d", account, n) }

	hub := NewHub(database.Pool(), database.Direct())
	go func() {
		for ctx.Err() == nil {
			_ = hub.ListenFor(ctx, 10*time.Second)
		}
	}()
	waitListening(t, ctx, database)

	// round holds all 50 waits on the given topics, runs change, and returns which machines were told what.
	round := func(known map[string]int64, change func()) map[string]map[string]int64 {
		t.Helper()
		var mu sync.Mutex
		told := map[string]map[string]int64{}
		var wg sync.WaitGroup
		for n := 1; n <= 50; n++ {
			wg.Add(1)
			go func(id string) {
				defer wg.Done()
				got, err := hub.Wait(ctx, id, known, 4*time.Second)
				if err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				told[id] = got
				mu.Unlock()
			}(machine(n))
		}
		eventually(t, "all 50 waiters to subscribe", func() bool {
			hub.mu.Lock()
			defer hub.mu.Unlock()
			return len(hub.subs[accountKey(account)]) == 50
		})
		change()
		wg.Wait()
		return told
	}
	only := func(told map[string]map[string]int64, id, topic string) {
		t.Helper()
		for machineID, topics := range told {
			if machineID == id {
				if topics[topic] == 0 {
					t.Fatalf("%s was not told of %s: %v", id, topic, topics)
				}
				continue
			}
			if len(topics) != 0 {
				t.Fatalf("%s was woken by a %s change of %s: %v", machineID, topic, id, topics)
			}
		}
	}

	known := map[string]int64{TopicPreviews: 0, TopicConfig: 0}
	only(round(known, func() {
		exec(`INSERT INTO paperboat.preview_leases(id,endpoint,account_id,actor_id,owner_machine_id,owner_session_id,owner_session_kind,target_scheme,target_address,lease_deadline) VALUES($1,$2,$3,$3,$4,'session','foreground','http','127.0.0.1:3000',now()+interval '1 hour')`,
			"preview_"+stamp, "https://p-"+stamp+".preview.example.test", account, machine(7))
	}), machine(7), TopicPreviews)

	known[TopicPreviews] = 1 // the owner machine has seen the lease's generation
	only(round(known, func() {
		exec(`UPDATE paperboat.user_machines SET availability_desired_version=availability_desired_version+1 WHERE id=$1`, machine(3))
	}), machine(3), TopicConfig)
}

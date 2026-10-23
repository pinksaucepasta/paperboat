package machineevents

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// An audience-wide topic wrote one generation row per machine of the audience: a rename on an account with N
// machines wrote N rows, and a 4,097-machine fixture insert ran 5.5 h. One change on a 2,000-machine account now
// writes one account row, and a waiter on another machine of the account is still told at once.
func TestAccountWideChangeWritesOneRowAndStillWakesEveryMachineOnPostgres(t *testing.T) {
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
	account := fmt.Sprintf("acct_topic_%d", time.Now().UnixNano())
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
	started := time.Now()
	exec(`INSERT INTO paperboat.user_machines (id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,last_seen_at)
SELECT $1||'_m'||g,$1,$1||'_e'||g,'m-'||g,'linux','amd64','/w','online','occupied',now() FROM generate_series(1,2000) g`, account)
	if took := time.Since(started); took > time.Minute {
		t.Fatalf("inserting 2,000 machines took %v", took)
	}
	rows := func() (machineRows, accountGeneration int64) {
		t.Helper()
		if err := database.SQL().QueryRowContext(ctx, `SELECT
 (SELECT coalesce(sum(generation),0) FROM paperboat.machine_topic_generations g JOIN paperboat.user_machines m ON m.id=g.machine_id WHERE m.user_id=$1),
 coalesce((SELECT generation FROM paperboat.account_topic_generations WHERE account_id=$1 AND topic='inventory'),0)`, account).Scan(&machineRows, &accountGeneration); err != nil {
			t.Fatal(err)
		}
		return machineRows, accountGeneration
	}

	hub := NewHub(database.Pool())
	go func() {
		for ctx.Err() == nil {
			_ = hub.ListenFor(ctx, 10*time.Second)
		}
	}()
	time.Sleep(300 * time.Millisecond) // let the listener attach
	waiter := account + "_m2000"
	known, err := hub.Wait(ctx, waiter, map[string]int64{TopicInventory: 0}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	beforeMachines, beforeAccount := rows()
	woke := make(chan map[string]int64, 1)
	go func() {
		changed, err := hub.Wait(ctx, waiter, map[string]int64{TopicInventory: known[TopicInventory]}, MaxWait)
		if err != nil {
			t.Error(err)
		}
		woke <- changed
	}()
	time.Sleep(200 * time.Millisecond)
	renamed := time.Now()
	exec(`UPDATE paperboat.user_machines SET alias='renamed' WHERE id=$1`, account+"_m1")
	select {
	case changed := <-woke:
		if changed[TopicInventory] <= known[TopicInventory] {
			t.Fatalf("waiter woke without the inventory change: %v (known %v)", changed, known)
		}
		if took := time.Since(renamed); took > 3*time.Second {
			t.Fatalf("the waiter was told after %v, want at once (not the 5 s recheck)", took)
		}
	case <-time.After(MaxWait):
		t.Fatal("a rename on the account never woke another machine's waiter")
	}
	afterMachines, afterAccount := rows()
	if afterMachines != beforeMachines {
		t.Fatalf("a rename changed machine topic rows of the account (sum %d -> %d), want only the account row", beforeMachines, afterMachines)
	}
	if afterAccount != beforeAccount+1 {
		t.Fatalf("account inventory generation %d -> %d, want one bump", beforeAccount, afterAccount)
	}
}

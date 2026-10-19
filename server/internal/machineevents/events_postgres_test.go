package machineevents

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
)

func TestHostIsToldAboutAChangeWithoutPollingOnPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("requires isolated PostgreSQL")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
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
	account, machine, other := "events_account_"+stamp, "events_machine_"+stamp, "events_other_"+stamp
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := database.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("%v: %s", err, query)
		}
	}
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$2,'active')`, account, account+"@example.test")
	for index, id := range []string{machine, other} {
		exec(`INSERT INTO paperboat.user_machines (id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state) VALUES ($1,$2,$3,$4,'linux','amd64','/workspace','online','occupied')`, id, account, "env_"+id, fmt.Sprintf("box-%d", index))
	}
	t.Cleanup(func() {
		_, _ = database.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.users WHERE id=$1`, account)
	})
	hub := NewHub(database.Pool())
	go func() {
		for ctx.Err() == nil {
			_ = hub.ListenFor(ctx, 10*time.Second)
		}
	}()
	time.Sleep(300 * time.Millisecond) // let the listener attach

	publish := func(ids []string, topic string) {
		t.Helper()
		tx, err := database.Pool().Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := Publish(ctx, tx, ids, topic); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	type result struct {
		changed map[string]int64
		err     error
		after   time.Duration
	}
	wait := func(machineID string, known map[string]int64, maxWait time.Duration) <-chan result {
		out := make(chan result, 1)
		started := time.Now()
		go func() {
			changed, err := hub.Wait(ctx, machineID, known, maxWait)
			out <- result{changed, err, time.Since(started)}
		}()
		return out
	}

	// Nothing has happened: the wait answers "no change" at its deadline.
	if r := <-wait(machine, map[string]int64{}, 700*time.Millisecond); r.err != nil || len(r.changed) != 0 || r.after < 600*time.Millisecond {
		t.Fatalf("idle wait = %+v", r)
	}
	// A publish wakes a waiting host in well under a second, and only for that machine and topic set.
	waiting := wait(machine, map[string]int64{}, 20*time.Second)
	bystander := wait(other, map[string]int64{}, 1500*time.Millisecond)
	time.Sleep(200 * time.Millisecond)
	publish([]string{machine}, TopicPreviews)
	r := <-waiting
	if r.err != nil || r.changed[TopicPreviews] != 1 || len(r.changed) != 1 || r.after > 2*time.Second {
		t.Fatalf("woken wait = %+v", r)
	}
	if b := <-bystander; b.err != nil || len(b.changed) != 0 {
		t.Fatalf("another machine was told: %+v", b)
	}
	// A host that was away compares what it knew: an older generation is answered at once, a current one waits.
	publish([]string{machine}, TopicPreviews)
	publish([]string{machine}, TopicMachineAccess)
	if r := <-wait(machine, map[string]int64{TopicPreviews: 1}, 5*time.Second); r.err != nil || r.changed[TopicPreviews] != 2 || r.changed[TopicMachineAccess] != 1 || r.after > time.Second {
		t.Fatalf("catch-up wait = %+v", r)
	}
	if r := <-wait(machine, map[string]int64{TopicPreviews: 2, TopicMachineAccess: 1}, 600*time.Millisecond); r.err != nil || len(r.changed) != 0 {
		t.Fatalf("up-to-date wait = %+v", r)
	}
	// A rolled-back write publishes nothing.
	tx, err := database.Pool().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := Publish(ctx, tx, []string{machine}, TopicConfig); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if r := <-wait(machine, map[string]int64{TopicPreviews: 2, TopicMachineAccess: 1}, 600*time.Millisecond); r.err != nil || len(r.changed) != 0 {
		t.Fatalf("rolled-back publish was seen: %+v", r)
	}
	// Writes that already exist in the product publish on their own (triggers): a preview for the
	// machine and an access session for it.
	waiting = wait(machine, map[string]int64{TopicPreviews: 2, TopicMachineAccess: 1}, 20*time.Second)
	time.Sleep(200 * time.Millisecond)
	exec(`INSERT INTO paperboat.preview_leases(id,endpoint,account_id,actor_id,owner_machine_id,owner_session_id,owner_session_kind,target_scheme,target_address,lease_deadline) VALUES($1,$2,$3,$3,$4,'session','foreground','http','127.0.0.1:3000',now()+interval '1 hour')`, "preview_"+stamp, "https://p-"+stamp+".preview.example.test", account, machine)
	if r := <-waiting; r.err != nil || r.changed[TopicPreviews] != 3 {
		t.Fatalf("a new preview did not wake its machine: %+v", r)
	}
	exec(`UPDATE paperboat.preview_leases SET lease_deadline=now()+interval '2 hours',last_renewed_at=now() WHERE id=$1`, "preview_"+stamp)
	if r := <-wait(machine, map[string]int64{TopicPreviews: 3, TopicMachineAccess: 1}, 600*time.Millisecond); r.err != nil || len(r.changed) != 0 {
		t.Fatalf("a lease renewal woke the host: %+v", r)
	}
	exec(`UPDATE paperboat.preview_leases SET terminal_state='stopped',stopped_at=now() WHERE id=$1`, "preview_"+stamp)
	if r := <-wait(machine, map[string]int64{TopicPreviews: 3, TopicMachineAccess: 1}, 5*time.Second); r.err != nil || r.changed[TopicPreviews] != 4 {
		t.Fatalf("a stopped preview did not wake its machine: %+v", r)
	}
	for _, bad := range []string{"unknown_topic"} {
		if _, err := hub.Wait(ctx, machine, map[string]int64{bad: 0}, time.Second); err == nil {
			t.Fatalf("unknown topic %q accepted", bad)
		}
	}
}

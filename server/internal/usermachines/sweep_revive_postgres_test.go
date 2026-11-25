package usermachines

import (
	"context"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
	"github.com/pinksaucepasta/paperboat/server/internal/machineevents"
	"github.com/pinksaucepasta/paperboat/server/internal/metering"
)

// A running machine the sweep marks offline is told to observe: its held wait answers with the config topic at once,
// and its observation brings it back. One topic move per transition.
func TestSweptMachineWithAHeldWaitComesBackPostgres(t *testing.T) {
	f := newWorkspaceMachineFixture(t)
	service := New(f.store, nil, Policy{OfflineAfter: 90 * time.Second}, nil)
	hub := machineevents.NewHub(f.store.Pool(), f.store.Direct())
	listening, stop := context.WithCancel(f.ctx)
	stopped := make(chan struct{})
	go func() { defer close(stopped); _ = hub.ListenFor(listening, time.Minute) }()
	t.Cleanup(func() { stop(); <-stopped })
	generation := func() (g int64) {
		t.Helper()
		if err := f.store.SQL().QueryRowContext(f.ctx, `SELECT coalesce((SELECT generation FROM paperboat.machine_topic_generations WHERE machine_id=$1 AND topic='config'),0)`, f.machine).Scan(&g); err != nil {
			t.Fatal(err)
		}
		return g
	}
	state := func() (state string) {
		t.Helper()
		if err := f.store.SQL().QueryRowContext(f.ctx, `SELECT state FROM paperboat.user_machines WHERE id=$1 AND online=(state='online')`, f.machine).Scan(&state); err != nil {
			t.Fatal(err)
		}
		return state
	}
	observe := func() {
		t.Helper()
		if err := metering.NewRuntimeRepository(f.store, 45*time.Second).RecordRuntimeObservation(f.ctx, metering.RuntimeObservation{EnvironmentID: "env_" + f.suffix, MachineID: f.machine, Capabilities: []string{"terminal_host"}}); err != nil {
			t.Fatal(err)
		}
	}
	known := generation()
	if _, err := f.store.SQL().ExecContext(f.ctx, `UPDATE paperboat.user_machines SET last_seen_at=now()-interval '91 seconds' WHERE id=$1`, f.machine); err != nil {
		t.Fatal(err)
	}
	answered := make(chan map[string]int64, 1)
	go func() {
		changed, _ := hub.Wait(f.ctx, f.machine, map[string]int64{machineevents.TopicConfig: known}, machineevents.MaxWait)
		answered <- changed
	}()
	for !hub.Watching(f.machine) {
		time.Sleep(time.Millisecond)
	}
	if err := service.sweepStaleMachines(f.ctx); err != nil {
		t.Fatal(err)
	}
	if got := state(); got != "offline" {
		t.Fatalf("the sweep left the machine %s", got)
	}
	select {
	case changed := <-answered:
		if changed[machineevents.TopicConfig] != known+1 {
			t.Fatalf("the held wait answered %v, want config %d", changed, known+1)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the held wait of a swept machine was not woken")
	}
	observe()
	if got := state(); got != "online" {
		t.Fatalf("the observation left the machine %s", got)
	}
	// Seen now, or already offline: no further move.
	if err := service.sweepStaleMachines(f.ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.SQL().ExecContext(f.ctx, `UPDATE paperboat.user_machines SET state='offline',online=false,last_seen_at=now()-interval '1 hour' WHERE id=$1`, f.machine); err != nil {
		t.Fatal(err)
	}
	if err := service.sweepStaleMachines(f.ctx); err != nil {
		t.Fatal(err)
	}
	if got := generation(); got != known+1 {
		t.Fatalf("config generation %d, want one move to %d", got, known+1)
	}
}

// A touch that meets a locked row reports it, so the flush repeats it instead of leaving it to the next wait.
func TestPresenceTouchReportsALockedRowPostgres(t *testing.T) {
	f := newWorkspaceMachineFixture(t)
	if _, err := f.store.SQL().ExecContext(f.ctx, `UPDATE paperboat.user_machines SET last_seen_at=now()-interval '80 seconds' WHERE id=$1`, f.machine); err != nil {
		t.Fatal(err)
	}
	touch := func() int {
		t.Helper()
		skipped, err := f.store.Queries().TouchUserMachinesPresence(f.ctx, dbsqlc.TouchUserMachinesPresenceParams{Ids: []string{f.machine}, GateSeconds: 25})
		if err != nil {
			t.Fatal(err)
		}
		return len(skipped)
	}
	writer, err := f.store.Pool().Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback(f.ctx)
	if _, err := writer.Exec(f.ctx, `SELECT 1 FROM paperboat.user_machines WHERE id=$1 FOR UPDATE`, f.machine); err != nil {
		t.Fatal(err)
	}
	if touch() != 1 {
		t.Fatal("a touch skipped on a locked row was not reported")
	}
	if err := writer.Rollback(f.ctx); err != nil {
		t.Fatal(err)
	}
	for _, row := range []string{"stale", "fresh"} {
		if touch() != 0 {
			t.Fatalf("a %s row nobody holds was reported as skipped", row)
		}
	}
}

// The row of an idle machine is held for 2 s when its touch is due, 2 s before the cutoff: the flushes repeat the touch
// and the sweep finds a machine just seen. The touch dropped, the machine would be swept.
func TestHeldRowAtADueTouchIsNotSweptPostgres(t *testing.T) {
	f := newWorkspaceMachineFixture(t)
	service := New(f.store, nil, Policy{OfflineAfter: 90 * time.Second}, nil)
	repo := metering.NewRuntimeRepository(f.store, 45*time.Second)
	if _, err := f.store.SQL().ExecContext(f.ctx, `UPDATE paperboat.user_machines SET last_seen_at=now()-interval '88 seconds' WHERE id=$1`, f.machine); err != nil {
		t.Fatal(err)
	}
	holder, err := f.store.Pool().Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(f.ctx)
	if _, err := holder.Exec(f.ctx, `SELECT 1 FROM paperboat.user_machines WHERE id=$1 FOR UPDATE`, f.machine); err != nil {
		t.Fatal(err)
	}
	_ = repo.TouchMachinePresence(f.ctx, f.machine)
	flush := func() {
		t.Helper()
		if err := repo.FlushPresence(f.ctx); err != nil {
			t.Fatal(err)
		}
	}
	for held := time.Now(); time.Since(held) < 2*time.Second; time.Sleep(200 * time.Millisecond) {
		flush()
	}
	if err := holder.Rollback(f.ctx); err != nil {
		t.Fatal(err)
	}
	flush()
	if err := service.sweepStaleMachines(f.ctx); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := f.store.SQL().QueryRowContext(f.ctx, `SELECT state FROM paperboat.user_machines WHERE id=$1`, f.machine).Scan(&state); err != nil || state != "online" {
		t.Fatalf("the machine is %q after its held touch (%v)", state, err)
	}
}

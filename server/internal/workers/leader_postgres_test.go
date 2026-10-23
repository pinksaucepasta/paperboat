package workers_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/workers"
)

// Two server instances on one database: workers run in exactly one. When the leader stops, or its
// database connection dies, the standby takes over (F21-04).
func TestWorkersRunInOneInstanceAndFailOverOnPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("isolated PostgreSQL required")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err = db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	var active, overlap atomic.Int32
	runs := map[string]*atomic.Int32{"a": {}, "b": {}}
	instance := func(name string) (*workers.Supervisor, context.CancelFunc, chan error) {
		leader := &workers.PostgresLeader{DB: store.SQL(), Instance: "test-" + name, Version: "test",
			Retry: 100 * time.Millisecond, Check: 100 * time.Millisecond, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
		s := workers.NewLeaderSupervisor(leader, func(ctx context.Context) error {
			if active.Add(1) > 1 {
				overlap.Add(1)
			}
			runs[name].Add(1)
			<-ctx.Done()
			active.Add(-1)
			return nil
		})
		runCtx, stop := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() { done <- s.Run(runCtx) }()
		return s, stop, done
	}
	waitFor := func(what string, ok func() bool) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for !ok() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	leaderRow := func() string {
		var name string
		_ = store.SQL().QueryRowContext(ctx, `SELECT instance FROM paperboat.worker_leader`).Scan(&name)
		return name
	}

	_, stopA, doneA := instance("a")
	waitFor("a to lead", func() bool { return runs["a"].Load() == 1 && leaderRow() == "test-a" })
	_, stopB, doneB := instance("b")
	time.Sleep(500 * time.Millisecond)
	if runs["b"].Load() != 0 {
		t.Fatal("standby ran workers while the leader held the lock")
	}
	stopA()
	<-doneA
	waitFor("b to take over", func() bool { return runs["b"].Load() == 1 && leaderRow() == "test-b" })
	if overlap.Load() != 0 {
		t.Fatal("workers ran in two instances at once during a clean handover")
	}

	// The leader's connection dies (database restart, network cut): its workers stop and a standby leads.
	_, stopC, doneC := instance("a")
	if _, err = store.SQL().ExecContext(ctx, `SELECT pg_terminate_backend(pid) FROM pg_locks
 WHERE locktype='advisory' AND objid=$1 AND granted`, int64(0x7062776c)); err != nil {
		t.Fatal(err)
	}
	waitFor("a to lead after b lost its connection", func() bool { return runs["a"].Load() == 2 && leaderRow() == "test-a" })
	waitFor("b's workers to stop", func() bool { return active.Load() == 1 })
	stopB()
	stopC()
	<-doneB
	<-doneC
}

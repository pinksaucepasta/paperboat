package metering

import (
	"context"
	"sync"
	"testing"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/machineevents"
)

// The presence write is batched: a machine's last_seen_at is refreshed up to PresenceFlushEvery after its wait asked
// for it. The worst case a healthy machine reaches before its row is written is the gate it was skipped at, plus one
// held wait, plus the flush delay, plus a daemon's first reconnect after a server crash; that must stay under the
// offline cutoff or machines flap offline and back.
func TestBatchedPresenceNeverApproachesTheOfflineCutoff(t *testing.T) {
	worst := machineWaitPresenceGate + machineevents.MaxWait + PresenceFlushEvery + presenceReconnectBudget
	cutoff := config.Default().UserMachines.OfflineAfter
	if worst >= cutoff {
		t.Fatalf("a healthy machine's last_seen_at can reach %v before its write; the offline cutoff is %v", worst, cutoff)
	}
}

// Many touches inside one interval are one write of the distinct machines, and a failed write keeps them queued.
func TestPresenceTouchesCoalesceIntoOneWriteAndSurviveAFailure(t *testing.T) {
	var mu sync.Mutex
	var writes [][]string
	fail := true
	repo := &RuntimeRepository{writePresence: func(_ context.Context, ids []string) error {
		mu.Lock()
		defer mu.Unlock()
		writes = append(writes, ids)
		if fail {
			return context.DeadlineExceeded
		}
		return nil
	}}
	for i := 0; i < 300; i++ {
		_ = repo.TouchMachinePresence(context.Background(), []string{"m1", "m2", "m3"}[i%3])
	}
	if err := repo.FlushPresence(context.Background()); err == nil {
		t.Fatal("the failed write was not reported")
	}
	fail = false
	_ = repo.TouchMachinePresence(context.Background(), "m4")
	if err := repo.FlushPresence(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := repo.FlushPresence(context.Background()); err != nil { // nothing pending: no write
		t.Fatal(err)
	}
	if len(writes) != 2 || len(writes[0]) != 3 || len(writes[1]) != 4 {
		t.Fatalf("writes = %v, want one of 3 distinct machines, then the same 3 plus m4 after the failure", writes)
	}
}

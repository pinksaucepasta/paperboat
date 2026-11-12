package runtime

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/tunnel/internal/usage"
)

// Stream churn far above the old 4 reports/s (40 new routes per 1 s tick for 200 ticks, a
// queue of 64) must never refuse a stream, keep the queue flat, and lose no byte: counters
// are absolute, so a key the full queue could not take is retried the next tick.
func TestUsageChurnNeverRefusesAStreamAndKeepsTheQueueFlat(t *testing.T) {
	const ticks, perTick, queueSize = 200, 40, 64
	_, private, _ := ed25519.GenerateKey(nil)
	queue, _ := usage.NewQueue(queueSize, 1<<22)
	now := time.Unix(1000, 0)
	meter := &usage.Meter{Node: "edge", Epoch: "epoch", Counters: usage.NewCounters(), Queue: queue, KeyID: "k", PrivateKey: private, Persist: func() error { return nil }, Now: func() time.Time { return now }}
	sink := &usageSink{}
	worker := &UsageWorker{Queue: queue, Sink: sink, Prepare: meter, Persist: meter.Persist}
	deepest := 0
	for tick := 0; tick < ticks; tick++ {
		now = now.Add(time.Second)
		for i := 0; i < perTick; i++ {
			route := fmt.Sprintf("preview_%d_%d", tick, i)
			if err := meter.Record("env", route, 1, 100, 0, "authority"); err != nil {
				t.Fatalf("tick %d: stream refused: %v", tick, err)
			}
		}
		if _, _, err := worker.deliver(context.Background()); err != nil {
			t.Fatalf("tick %d: %v", tick, err)
		}
		if depth := queue.Len(); depth > deepest {
			deepest = depth
		}
	}
	if _, _, err := worker.deliver(context.Background()); err != nil {
		t.Fatal(err)
	}
	if sink.calls != ticks*perTick || queue.Len() != 0 || deepest > perTick {
		t.Fatalf("reported %d of %d, queue=%d, deepest=%d", sink.calls, ticks*perTick, queue.Len(), deepest)
	}
}

package machineevents

import (
	"context"
	"testing"
	"time"
)

// Waiting hosts are answered by notifications; the re-read backstop must stay a single query per held wait, or N idle
// daemons cost N/5 queries per second again.
func TestRecheckBackstopFiresAtMostOncePerWait(t *testing.T) {
	if MaxWait >= 2*recheckInterval {
		t.Fatalf("recheckInterval %v fires %d times in a MaxWait %v wait", recheckInterval, int(MaxWait/recheckInterval), MaxWait)
	}
}

func TestWatchWaitWakesByKeyTimesOutAndDrains(t *testing.T) {
	h := NewHub(nil, nil)
	ctx := context.Background()
	w := h.Watch("edge:a")
	defer w.Close()
	h.wake("edge:b") // another key does not wake it
	if woken, err := w.Wait(ctx, 30*time.Millisecond); woken || err != nil {
		t.Fatalf("wait after another key = %v, %v", woken, err)
	}
	h.wake("edge:a") // a wake before Wait is kept
	if woken, err := w.Wait(ctx, time.Second); !woken || err != nil {
		t.Fatalf("wait after its key = %v, %v", woken, err)
	}
	go func() { time.Sleep(20 * time.Millisecond); h.wake("edge:a") }()
	started := time.Now()
	if woken, err := w.Wait(ctx, 5*time.Second); !woken || err != nil || time.Since(started) > time.Second {
		t.Fatalf("held wait = %v, %v after %v", woken, err, time.Since(started))
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := w.Wait(canceled, time.Second); err == nil {
		t.Fatal("an abandoned wait returned no error")
	}
	h.Drain()
	if woken, err := w.Wait(ctx, 5*time.Second); woken || err != nil {
		t.Fatalf("wait while draining = %v, %v", woken, err)
	}
}

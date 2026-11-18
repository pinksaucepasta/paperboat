package nativeprivateaccess

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Grants of one session run one at a time, grants of another session do not wait for them, a waiter leaves when its
// context ends, and nothing is kept for a key once its last grant is done.
func TestSessionGateSerializesOneKeyAndForgetsIt(t *testing.T) {
	var gate sessionGate
	var inside, peak atomic.Int32
	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := gate.enter(context.Background(), "session-a")
			if err != nil {
				t.Error(err)
				return
			}
			defer release()
			n := inside.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			time.Sleep(time.Millisecond)
			inside.Add(-1)
		}()
	}
	wg.Wait()
	if peak.Load() != 1 {
		t.Fatalf("%d grants of one session ran at once", peak.Load())
	}
	if len(gate.gates) != 0 {
		t.Fatalf("%d keys kept after every grant finished", len(gate.gates))
	}

	hold, err := gate.enter(context.Background(), "session-a")
	if err != nil {
		t.Fatal(err)
	}
	other, err := gate.enter(context.Background(), "session-b")
	if err != nil {
		t.Fatalf("another session waited: %v", err)
	}
	other()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := gate.enter(ctx, "session-a"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a waiter past its deadline got %v", err)
	}
	hold()
	if len(gate.gates) != 0 {
		t.Fatalf("%d keys kept after the waiter left and the holder released", len(gate.gates))
	}
}

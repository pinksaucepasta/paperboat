package usermachines

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

func TestTerminalInventoryEnrichmentBoundsSlowHosts(t *testing.T) {
	ids := make([]string, 200)
	for i := range ids {
		ids[i] = fmt.Sprint(i)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	var active, maximum, calls atomic.Int64
	start := time.Now()
	got := boundedTerminalInventoryObservations(ctx, ids, func(ctx context.Context, id string) (SharedTerminalSession, error) {
		calls.Add(1)
		n := active.Add(1)
		defer active.Add(-1)
		for old := maximum.Load(); n > old; old = maximum.Load() {
			if maximum.CompareAndSwap(old, n) {
				break
			}
		}
		if id == "0" {
			return SharedTerminalSession{ID: id, Title: "Live title"}, nil
		}
		<-ctx.Done()
		return SharedTerminalSession{}, ctx.Err()
	})
	if len(got) != 200 || got[0] == nil || got[0].Title != "Live title" {
		t.Fatalf("available metadata lost: %+v", got[0])
	}
	for i := 1; i < len(got); i++ {
		if got[i] != nil {
			t.Fatalf("slow host claimed live metadata index%d", i)
		}
	}
	if active.Load() != 0 || maximum.Load() > 4 || calls.Load() > 5 || time.Since(start) > time.Second {
		t.Fatalf("unbounded work active%d maximum%d calls%d elapsed%s", active.Load(), maximum.Load(), calls.Load(), time.Since(start))
	}
}
func TestTerminalInventoryEnrichmentUsesOnePageDeadline(t *testing.T) {
	var deadline time.Time
	got := boundedTerminalInventoryObservations(context.Background(), []string{"first"}, func(ctx context.Context, id string) (SharedTerminalSession, error) {
		var ok bool
		deadline, ok = ctx.Deadline()
		if !ok {
			t.Error("runtime snapshot has no page deadline")
		}
		return SharedTerminalSession{ID: id}, nil
	})
	if got[0] == nil || deadline.IsZero() || time.Until(deadline) > terminalInventoryObservationBudget {
		t.Fatal("runtime metadata budget missing")
	}
}

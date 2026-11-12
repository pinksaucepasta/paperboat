package usage

import (
	"crypto/ed25519"
	"fmt"
	"testing"
	"time"
)

func capMeter(limit int) (*Meter, *Queue, *time.Time) {
	_, private, _ := ed25519.GenerateKey(nil)
	queue, _ := NewQueue(1000, 1<<24)
	now := time.Unix(1000, 0).UTC()
	meter := &Meter{Node: "edge", Epoch: "epoch", Counters: NewCounters(), Queue: queue, KeyID: "k", PrivateKey: private, MaxCounters: limit, Persist: func() error { return nil }, Now: func() time.Time { return now }}
	return meter, queue, &now
}

func deliverAll(queue *Queue) {
	for {
		report, ok := queue.Next()
		if !ok {
			return
		}
		queue.Ack(report.OperationID)
	}
}

func TestMeterEvictsOldestDeliveredCountersPastTheCap(t *testing.T) {
	meter, queue, now := capMeter(10)
	for i := 0; i < 10; i++ {
		*now = now.Add(time.Second)
		if err := meter.Record("env", fmt.Sprintf("route_%d", i), 1, 5, 0, "authority"); err != nil {
			t.Fatal(err)
		}
	}
	if err := meter.Flush(); err != nil {
		t.Fatal(err)
	}
	first, _ := queue.Next()
	deliverAll(queue)
	*now = now.Add(time.Second)
	for i := 10; i < 40; i++ {
		*now = now.Add(2 * time.Second)
		if err := meter.Record("env", fmt.Sprintf("route_%d", i), 1, 5, 0, "authority"); err != nil {
			t.Fatal(err)
		}
		if err := meter.Flush(); err != nil {
			t.Fatal(err)
		}
		deliverAll(queue)
		if held, limit := meter.CounterPressure(); held > limit {
			t.Fatalf("route %d: %d counters held, limit %d", i, held, limit)
		}
	}
	for _, record := range meter.Counters.Snapshot() {
		if record.Key.Route == "route_0" {
			t.Fatal("the oldest delivered counter was kept")
		}
	}
	// An evicted route that resumes must not restart its absolute value under the old identity.
	*now = now.Add(2 * time.Second)
	if err := meter.Record("env", "route_0", 1, 5, 0, "authority"); err != nil {
		t.Fatal(err)
	}
	if err := meter.Flush(); err != nil {
		t.Fatal(err)
	}
	resumed, _ := queue.Next()
	if resumed.Key.Route != "route_0" || resumed.Key.counterIdentity() == first.Key.counterIdentity() {
		t.Fatalf("resumed counter reuses the evicted identity: %+v vs %+v", resumed.Key, first.Key)
	}
}

func TestMeterKeepsServingWhenNoCounterIsDelivered(t *testing.T) {
	meter, queue, now := capMeter(4)
	for i := 0; i < 20; i++ {
		*now = now.Add(2 * time.Second)
		if err := meter.Record("env", fmt.Sprintf("route_%d", i), 1, 5, 0, "authority"); err != nil {
			t.Fatalf("stream %d refused: %v", i, err)
		}
	}
	if held, limit := meter.CounterPressure(); held != 20 || held < limit {
		t.Fatalf("held=%d limit=%d: undelivered counters must be kept and the pressure visible", held, limit)
	}
	if err := meter.Flush(); err != nil {
		t.Fatal(err)
	}
	deliverAll(queue)
	*now = now.Add(2 * time.Second)
	if err := meter.Record("env", "route_new", 1, 5, 0, "authority"); err != nil {
		t.Fatal(err)
	}
	if held, limit := meter.CounterPressure(); held > limit {
		t.Fatalf("after delivery held=%d, limit %d", held, limit)
	}
}

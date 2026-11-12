package usage

import (
	"crypto/ed25519"
	"errors"
	"testing"
	"time"
)

func TestScopedMeterSeparatesSharedRouteAndPreservesExpiredDelivery(t *testing.T) {
	_, key, _ := ed25519.GenerateKey(nil)
	now := time.Now().UTC()
	queue, _ := NewQueue(4, 1<<20)
	m := &Meter{Node: "node", Epoch: "epoch", Counters: NewCounters(), Queue: queue, KeyID: "key", PrivateKey: key, Persist: func() error { return nil }, Now: func() time.Time { return now }}
	for _, authority := range []string{"grant:team-one", "grant:team-two"} {
		if err := m.Record("environment", "shared-route", 1, 100, 0, authority); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.Flush(); err != nil {
		t.Fatal(err)
	}
	if queue.Len() != 2 {
		t.Fatalf("shared route combined independent grants: %d", queue.Len())
	}
	for _, r := range queue.Snapshot().Reports {
		if r.Bytes != 100 {
			t.Fatalf("counter=%d", r.Bytes)
		}
	}
	now = now.Add(DeliveryWindow + time.Second)
	if err := m.Flush(); err != nil {
		t.Fatal(err)
	}
	if queue.Len() != 2 {
		t.Fatal("expired pending usage silently discarded")
	}
	for _, r := range queue.Snapshot().Reports {
		queue.Ack(r.OperationID)
	}
	if err := m.Record("environment", "shared-route", 1, 1, 0, "grant:team-one"); err != nil {
		t.Fatalf("fresh usage did not recover after acknowledgement: %v", err)
	}
	if err := m.Flush(); err != nil {
		t.Fatal(err)
	}
	fresh, ok := queue.Next()
	if !ok || fresh.Bytes != 1 {
		t.Fatalf("fresh report after recovery=%+v", fresh)
	}
}

func TestScopedMeterNeverRefusesAndRotatesPrunedIdentity(t *testing.T) {
	_, key, _ := ed25519.GenerateKey(nil)
	now := time.Now().UTC()
	q, _ := NewQueue(1, 1<<20)
	m := &Meter{Node: "n", Epoch: "e", Counters: NewCounters(), Queue: q, KeyID: "k", PrivateKey: key, Persist: func() error { return nil }, Now: func() time.Time { return now }}
	if err := m.Record("env", "route", 1, 1, 0, "grant:a"); err != nil {
		t.Fatal(err)
	}
	// Past the queue size the stream is still served; the counter waits for room.
	if err := m.Record("env", "route", 1, 1, 0, "grant:b"); err != nil {
		t.Fatalf("stream refused: %v", err)
	}
	if err := m.Flush(); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("flush with a full queue = %v", err)
	}
	before, _ := q.Next()
	q.Ack(before.OperationID)
	now = now.Add(DeliveryWindow + time.Hour)
	if err := m.Record("env", "route", 1, 1, 0, "grant:a"); err != nil {
		t.Fatal(err)
	}
	reported := map[Key]bool{}
	for i := 0; i < 4 && len(reported) < 2; i++ {
		_ = m.Flush()
		if r, ok := q.Next(); ok {
			reported[r.Key.counterIdentity()] = true
			q.Ack(r.OperationID)
		}
	}
	if len(reported) != 2 || reported[before.Key.counterIdentity()] {
		t.Fatalf("pruned absolute counter reused its server identity or a counter was lost: %v", reported)
	}
	if len(m.Counters.Snapshot()) != 2 {
		t.Fatal("expired delivered counter retained")
	}
}

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

func TestScopedMeterBoundsCountersAndRotatesPrunedIdentity(t *testing.T) {
	_, key, _ := ed25519.GenerateKey(nil)
	now := time.Now().UTC()
	q, _ := NewQueue(1, 1<<20)
	m := &Meter{Node: "n", Epoch: "e", Counters: NewCounters(), Queue: q, KeyID: "k", PrivateKey: key, Persist: func() error { return nil }, Now: func() time.Time { return now }}
	if err := m.Record("env", "route", 1, 1, 0, "grant:a"); err != nil {
		t.Fatal(err)
	}
	if err := m.Record("env", "route", 1, 1, 0, "grant:b"); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("unbounded counter=%v", err)
	}
	if err := m.Flush(); err != nil {
		t.Fatal(err)
	}
	before, _ := q.Next()
	q.Ack(before.OperationID)
	now = now.Add(DeliveryWindow + time.Hour)
	if err := m.Record("env", "route", 1, 1, 0, "grant:a"); err != nil {
		t.Fatal(err)
	}
	if err := m.Flush(); err != nil {
		t.Fatal(err)
	}
	after, _ := q.Next()
	if before.Key.counterIdentity() == after.Key.counterIdentity() {
		t.Fatal("pruned absolute counter reused its server identity")
	}
	if len(m.Counters.Snapshot()) != 1 {
		t.Fatal("expired delivered counter retained")
	}
}

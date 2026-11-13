package connector

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/serverclock"
)

// A healthy pool re-reads its target descriptor only when a slot is empty or the descriptor is about to
// expire, not on every repair tick.
func TestHealthyPoolRefreshesTargetsOnlyNearExpiry(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	config := DefaultDataCarrierPoolConfig()
	config.MaximumCarriers = 1
	config.Targets = []DataCarrierTarget{{EdgeID: "edge-a", ProcessEpoch: "epoch-a", FailureDomain: "domain-a"}}
	config.Carrier = testDataCarrierConfig()
	config.TargetsExpireAt = serverclock.Now().Add(time.Minute)
	var mu sync.Mutex
	refreshes := 0
	config.RefreshTargets = func(context.Context) ([]DataCarrierTarget, time.Time, error) {
		mu.Lock()
		defer mu.Unlock()
		refreshes++
		return config.Targets, serverclock.Now().Add(time.Minute), nil
	}
	pool, err := NewDataCarrierPool(ctx, config, func(_ context.Context, r DataCarrierDialRequest) (DataCarrierDialResult, error) {
		local, remote := net.Pipe()
		if _, err := NewDataCarrierServer(ctx, remote, config.Carrier, DataCarrierAdmission{Identity: config.Session, Authorize: func(context.Context, StreamOpen) error { return nil }}); err != nil {
			return DataCarrierDialResult{}, err
		}
		return DataCarrierDialResult{Link: local, PeerIdentity: config.Session, Transport: r.Transport, EdgeID: r.EdgeID, FailureDomain: r.FailureDomain}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	count := func() int { mu.Lock(); defer mu.Unlock(); return refreshes }
	pool.refreshedAt = time.Time{}
	if err := pool.Connect(ctx); err != nil || count() != 1 {
		t.Fatalf("an empty slot must read the descriptor once: err=%v refreshes=%d", err, count())
	}
	for range 3 {
		pool.refreshedAt = time.Now().Add(-time.Minute)
		if err := pool.Connect(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if count() != 1 {
		t.Fatalf("a healthy pool re-read the descriptor %d times", count()-1)
	}
	pool.targetExpiry = serverclock.Now().Add(time.Second)
	pool.refreshedAt = time.Now().Add(-time.Minute)
	if err := pool.Connect(ctx); err != nil || count() != 2 {
		t.Fatalf("a descriptor near expiry must be refreshed: err=%v refreshes=%d", err, count())
	}
}

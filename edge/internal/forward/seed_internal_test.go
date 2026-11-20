package forward

import (
	"sync"
	"testing"
	"time"
)

func TestForcedSeedDoesNotBlockOnSeedLock(t *testing.T) {
	holders := &stubHolders{block: make(chan struct{})}
	m := internalMesh(t, holders, nil)
	setPeers(t, m, testPeer("b", 1, ""))
	var once sync.Once
	release := func() { once.Do(func() { close(holders.block) }) }
	t.Cleanup(release)
	periodic := make(chan struct{})
	go func() { m.refreshSeed(m.ctx, false); close(periodic) }()
	waitFor(t, "periodic refresh in flight", func() bool { return holders.calls.Load() == 1 })

	forced := make(chan struct{})
	go func() { m.refreshSeed(m.ctx, true); close(forced) }()
	select {
	case <-forced:
	case <-time.After(time.Second):
		t.Fatal("a forced refresh waited on the refresh in flight")
	}
	if n := holders.calls.Load(); n != 1 {
		t.Fatalf("holder pages asked = %d, want the forced refresh skipped", n)
	}
	release()
	<-periodic
}

func TestForcedSeedPageCap(t *testing.T) {
	holders := &stubHolders{}
	m := internalMesh(t, holders, nil)
	setPeers(t, m, testPeer("b", 1, ""))
	m.refreshSeed(m.ctx, true)
	if n := holders.calls.Load(); n != maxForcedSeedPages {
		t.Fatalf("forced refresh read %d pages, want %d", n, maxForcedSeedPages)
	}
	m.seedMu.Lock()
	m.seededAt = time.Time{} // past the forced-refresh gap
	m.seedMu.Unlock()
	holders.calls.Store(0)
	m.refreshSeed(m.ctx, false)
	if n := holders.calls.Load(); n != maxSeedPages {
		t.Fatalf("periodic refresh read %d pages, want the full %d", n, maxSeedPages)
	}
}

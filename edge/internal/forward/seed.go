package forward

import (
	"context"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/backoff"
	"github.com/pinksaucepasta/paperboat/shared/nodemesh"
)

const (
	// maxSeedPages bounds one seed refresh: 2048 pages of 500 names, far above any node's holdings.
	maxSeedPages = 2048
	// forceSeedGap is the shortest time between two forced refreshes, so unknown names cannot make the node hammer
	// the server.
	forceSeedGap = time.Second
	// maxForcedSeedPages bounds a request-time refresh: a visitor waits on it inside the forward's opening deadline
	// (2x dial+open timeout), and each page may cost up to 2*dial+open, so only a few pages fit. The periodic loop
	// still pages through everything.
	maxForcedSeedPages = 8
)

// seedLoop refreshes the holder map from the server (anti-entropy for what the watches miss) once the peer
// directory is known; a node with no peers (a self-hosted edge) never asks.
func (m *Mesh) seedLoop(ctx context.Context) {
	select {
	case <-m.peersReady:
	case <-ctx.Done():
		return
	}
	for {
		m.refreshSeed(ctx, false)
		if !sleep(ctx, backoff.Jitter(m.cfg.SeedInterval)) {
			return
		}
	}
}

// refreshSeed pages through the server's holder snapshot. A forced refresh (a forward found no holder that
// would take the visitor) never waits for a refresh already in flight, is skipped when one finished within
// forceSeedGap, and reads at most maxForcedSeedPages pages.
func (m *Mesh) refreshSeed(ctx context.Context, forced bool) {
	if m.peerSet().Len() == 0 {
		return
	}
	pages := maxSeedPages
	if forced {
		if !m.seedMu.TryLock() {
			return
		}
		pages = maxForcedSeedPages
	} else {
		m.seedMu.Lock()
	}
	defer m.seedMu.Unlock()
	if forced && time.Since(m.seededAt) < forceSeedGap {
		return
	}
	page, err := "", error(nil)
	for range pages {
		var resp nodemesh.HoldersResponse
		rctx, cancel := context.WithTimeout(ctx, 2*m.cfg.DialTimeout+m.cfg.OpenTimeout)
		resp, err = m.cfg.Holders.Holders(rctx, page, nodemesh.MaxHolderPage)
		cancel()
		if err != nil {
			break
		}
		valid := resp.Holders[:0:0]
		for _, h := range resp.Holders {
			if h.Validate() == nil {
				valid = append(valid, h)
			}
		}
		m.dir.ApplySeed(valid)
		if resp.Next == "" {
			break
		}
		page = resp.Next
	}
	m.seededAt = time.Now()
	m.report(StageLookup, "holder seed", err)
}

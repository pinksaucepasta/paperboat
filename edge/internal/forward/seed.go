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
		select {
		case <-m.peersReady: // the directory answered with no peers: nobody else can hold a name
			m.markSeeded()
		default:
		}
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
	page, err, complete := "", error(nil), false
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
			complete = true
			break
		}
		page = resp.Next
	}
	m.seededAt = time.Now()
	// A full refresh that read every page it may without an error is as loaded as the node gets; a forced one
	// counts only when it reached the end.
	if complete || !forced && err == nil {
		m.markSeeded()
	}
	m.report(StageLookup, "holder seed", err)
}

// startupWait (measured from New) bounds how long a fresh node treats unheld names as "not loaded yet" rather than unknown, when the
// peer directory or holder snapshot keeps failing.
const startupWait = 30 * time.Second

func (m *Mesh) markSeeded() { m.seedOnce.Do(func() { close(m.seeded) }) }

// Starting reports whether the node has not yet loaded its peers and a full holder snapshot and is still within
// startupWait of its creation: a name no holder is known for may then be held elsewhere, so it is not unknown yet.
func (m *Mesh) Starting() bool {
	select {
	case <-m.seeded:
		return false
	default:
	}
	return m.cfg.Now().Sub(m.createdAt) < startupWait
}

// AwaitSeeded waits until the first load finished (true) or ctx ends (false).
func (m *Mesh) AwaitSeeded(ctx context.Context) bool {
	select {
	case <-m.seeded:
		return true
	case <-ctx.Done():
		return false
	}
}

package httpapi

import (
	"context"
	"errors"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/backoff"

	"github.com/pinksaucepasta/paperboat/server/internal/peersessions"
)

// errPeerNetworkBusy means too many configuration requests are in flight (building or waiting for their
// endpoint's build lock) for this server to take another.
var errPeerNetworkBusy = errors.New("peer network configuration requests in flight are over the bound")

// peerNetworkShedder refuses configuration requests past max in flight, before any database work. After a server
// restart every machine reconnects into a cold policy cache at once; building them all at once exhausts the pool and
// delays every one, so the excess gets 503 with a jittered Retry-After and returns spread out (headscale bounds its
// mapper work queue the same way: hscontrol/mapper/batcher.go workCh).
type peerNetworkShedder struct {
	peerNetworkService
	inflight atomic.Int64
	max      int64
}

func shedPeerNetwork(s peerNetworkService, max int) *peerNetworkShedder {
	return &peerNetworkShedder{peerNetworkService: s, max: int64(max)}
}

func (p *peerNetworkShedder) Configuration(ctx context.Context, in peersessions.NetworkConfigRequest) (peersessions.NetworkConfigResult, error) {
	if p.inflight.Add(1) > p.max && p.max > 0 { // 0 (a router built without config): unbounded
		p.inflight.Add(-1)
		return peersessions.NetworkConfigResult{}, errPeerNetworkBusy
	}
	defer p.inflight.Add(-1)
	return p.peerNetworkService.Configuration(ctx, in)
}

// peerNetworkBusyRetryAfter is the Retry-After in seconds: 2 s spread over [1, 3) s (shared/backoff.Jitter), so the
// shed machines do not come back as one wave.
func peerNetworkBusyRetryAfter() string {
	return strconv.Itoa(int(backoff.Jitter(2*time.Second).Round(time.Second) / time.Second))
}

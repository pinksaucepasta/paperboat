package edgehttp

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
	"golang.org/x/time/rate"
)

// throttledRoute is one hostname's pacer: the token bucket its workspace's routes share on this edge.
type throttledRoute struct {
	bytes    *rate.Limiter
	resetsAt time.Time
}

// Throttles slows the routes of workspaces that used their fair-use bandwidth. One token bucket per
// workspace is shared by all its routes on this edge (Tailscale's DERP applies one per client the same way:
// derpserver.go rateLimit, x/time/rate, waiting rather than dropping); it shapes and never refuses.
// Full speed resumes when the server stops listing the workspace or at its reset time, whichever is first.
type Throttles struct {
	mu         sync.Mutex
	workspaces map[string]*rate.Limiter
	byHost     atomic.Pointer[map[string]throttledRoute]
}

func newThrottles() *Throttles { return &Throttles{workspaces: make(map[string]*rate.Limiter)} }

// Set replaces the throttled set with the server's latest list. A workspace that stays listed keeps its bucket (and
// its tokens) and takes the rate of its entry: the server divides the plan's rate among the edges that carry it.
func (t *Throttles) Set(entries []connectorprotocol.IngressThrottle) {
	t.mu.Lock()
	defer t.mu.Unlock()
	next := make(map[string]*rate.Limiter, len(entries))
	byHost := make(map[string]throttledRoute, len(entries))
	for _, e := range entries {
		// A workspace's browser terminals share one bucket of their own, apart from its allowance's.
		key := e.Workspace
		if e.Terminal {
			key = "terminal:" + key
		}
		limiter := next[key]
		if limiter == nil {
			if limiter = t.workspaces[key]; limiter == nil {
				limiter = rate.NewLimiter(rate.Limit(e.BytesPerSecond), ingressPacingChunk)
			} else if limiter.Limit() != rate.Limit(e.BytesPerSecond) {
				limiter.SetLimit(rate.Limit(e.BytesPerSecond))
			}
			next[key] = limiter
		}
		byHost[e.Hostname] = throttledRoute{bytes: limiter, resetsAt: e.ResetsAt}
	}
	t.workspaces = next
	t.byHost.Store(&byHost)
}

// pacer returns the bucket that paces hostname, or nil when its workspace runs at full speed.
func (t *Throttles) pacer(hostname string, now time.Time) (*rate.Limiter, bool) {
	if t == nil {
		return nil, false
	}
	routes := t.byHost.Load()
	if routes == nil {
		return nil, false
	}
	route, ok := (*routes)[hostname]
	if !ok || (!route.resetsAt.IsZero() && !now.Before(route.resetsAt)) {
		return nil, false
	}
	return route.bytes, true
}

// Wait blocks until n bytes of hostname's traffic may pass; at once when its workspace is not throttled.
func (t *Throttles) Wait(ctx context.Context, hostname string, n int) error {
	limiter, ok := t.pacer(hostname, time.Now())
	if !ok {
		return nil
	}
	for n > 0 {
		chunk := min(n, limiter.Burst())
		if err := limiter.WaitN(ctx, chunk); err != nil {
			return err
		}
		n -= chunk
	}
	return nil
}

// Reserve takes n bytes of hostname's traffic from its workspace's bucket and returns how long the caller should wait before
// sending them (zero when the workspace is not throttled). The tokens are taken at once: a caller that sends early anyway leaves
// the workspace in debt, which later traffic repays.
func (t *Throttles) Reserve(hostname string, n int) time.Duration {
	now := time.Now()
	limiter, ok := t.pacer(hostname, now)
	if !ok {
		return 0
	}
	var delay time.Duration
	for n > 0 {
		chunk := min(n, limiter.Burst())
		delay = max(delay, limiter.ReserveN(now, chunk).DelayFrom(now))
		n -= chunk
	}
	return delay
}

// Charge counts n bytes of hostname's traffic against its workspace's bucket without waiting: interactive frames pass at once
// and their bytes are repaid by the bulk traffic behind them.
func (t *Throttles) Charge(hostname string, n int) { _ = t.Reserve(hostname, n) }

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
	config     IngressLimitConfig
	workspaces map[string]*rate.Limiter
	byHost     atomic.Pointer[map[string]throttledRoute]
}

func newThrottles(config IngressLimitConfig) *Throttles {
	return &Throttles{config: config, workspaces: make(map[string]*rate.Limiter)}
}

// configure changes the throttled rate of current and future workspaces.
func (t *Throttles) configure(config IngressLimitConfig, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.config = config
	for _, limiter := range t.workspaces {
		limiter.SetLimitAt(now, config.ByteRate)
		limiter.SetBurstAt(now, config.ByteBurst)
	}
}

// Set replaces the throttled set with the server's latest list. A workspace that stays listed keeps its bucket.
func (t *Throttles) Set(entries []connectorprotocol.IngressThrottle) {
	t.mu.Lock()
	defer t.mu.Unlock()
	next := make(map[string]*rate.Limiter, len(entries))
	byHost := make(map[string]throttledRoute, len(entries))
	for _, e := range entries {
		limiter := next[e.Workspace]
		if limiter == nil {
			if limiter = t.workspaces[e.Workspace]; limiter == nil {
				limiter = rate.NewLimiter(t.config.ByteRate, t.config.ByteBurst)
			}
			next[e.Workspace] = limiter
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

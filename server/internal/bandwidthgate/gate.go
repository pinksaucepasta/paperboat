// Package bandwidthgate answers one question for hot paths that must never wait on billing:
// is every workspace an account can use out of bandwidth allowance? It is one read-only
// statement (MVCC: no write, no row lock, so it cannot queue behind usage debits) with a
// short timeout and a per-account cache, and any failure answers "not denied" (fail open):
// connectivity never depends on billing; flows are still metered and charged when admitted.
package bandwidthgate

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/db"
)

const (
	defaultLookupTimeout = 150 * time.Millisecond
	answerTTL            = 10 * time.Second
	failureTTL           = 5 * time.Second
	cacheMax             = 4096
)

type entry struct {
	denied bool
	until  time.Time
}

// Gate is safe for concurrent use.
type Gate struct {
	store   *db.DB
	timeout time.Duration
	lookup  func(context.Context, string, time.Time) (bool, error)
	mu      sync.Mutex
	cache   map[string]entry
}

func New(store *db.DB) *Gate {
	g := &Gate{store: store, timeout: defaultLookupTimeout}
	g.lookup = g.Lookup
	return g
}

// SetLookupTimeout changes how long one lookup may take before the gate fails open; tests
// whose database is slower than a production one use it.
func (g *Gate) SetLookupTimeout(d time.Duration) { g.timeout = d }

// Denied reports whether every workspace available to the account is out of allowance.
func (g *Gate) Denied(ctx context.Context, account string, at time.Time) bool {
	if g == nil || account == "" {
		return false
	}
	now := time.Now()
	g.mu.Lock()
	if e, ok := g.cache[account]; ok && now.Before(e.until) {
		g.mu.Unlock()
		return e.denied
	}
	g.mu.Unlock()
	lookupCtx, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()
	denied, err := g.lookup(lookupCtx, account, at)
	ttl := answerTTL
	if err != nil {
		slog.WarnContext(ctx, "bandwidth gate unavailable; treating the account as within allowance", "error_type", fmt.Sprintf("%T", err))
		denied, ttl = false, failureTTL
	}
	g.mu.Lock()
	if g.cache == nil || len(g.cache) >= cacheMax {
		g.cache = make(map[string]entry)
	}
	g.cache[account] = entry{denied, now.Add(ttl)}
	g.mu.Unlock()
	return denied
}

// DeniedAmong answers for many accounts at once (a relay's connected accounts). Cached answers
// are free; at most maxFresh uncached accounts are looked up, concurrently (16 at a time) and
// within one overall deadline, and any account not answered in time counts as not denied this
// round (it is looked up on a later one). The result lists the denied accounts, sorted.
func (g *Gate) DeniedAmong(ctx context.Context, accounts []string, at time.Time, maxFresh int) []string {
	if g == nil || len(accounts) == 0 {
		return nil
	}
	var denied, fresh []string
	for _, account := range accounts {
		if d, known := g.Cached(account); known {
			if d {
				denied = append(denied, account)
			}
		} else if len(fresh) < maxFresh {
			fresh = append(fresh, account)
		}
	}
	if len(fresh) > 0 {
		ctx, cancel := context.WithTimeout(ctx, 2*g.timeout)
		defer cancel()
		var mu sync.Mutex
		var wg sync.WaitGroup
		slots := make(chan struct{}, 16)
		for _, account := range fresh {
			wg.Add(1)
			slots <- struct{}{}
			go func() {
				defer wg.Done()
				defer func() { <-slots }()
				if g.Denied(ctx, account, at) {
					mu.Lock()
					denied = append(denied, account)
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
	}
	sort.Strings(denied)
	return denied
}

// Cached reports a still-valid cached answer without touching the database.
func (g *Gate) Cached(account string) (denied, known bool) {
	if g == nil {
		return false, false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	e, ok := g.cache[account]
	if !ok || !time.Now().Before(e.until) {
		return false, false
	}
	return e.denied, true
}

// Lookup is the uncached read. A workspace is exhausted when a current usage period's
// consumption (summed over its budget segments) reached the included bytes and, for a paid
// period, no top-up credit remains; a workspace with no period yet is not exhausted. The
// account is denied only when its personal workspace and every active team are exhausted.
func (g *Gate) Lookup(ctx context.Context, account string, at time.Time) (bool, error) {
	var denied bool
	err := g.store.SQL().QueryRowContext(ctx, `WITH ws AS (
 SELECT 'personal:'||$1::text AS id
 UNION
 SELECT 'team:'||m.team_id FROM team_members m JOIN teams t ON t.team_id=m.team_id
 WHERE m.account_id=$1 AND m.active AND t.deleted_at IS NULL
)
SELECT coalesce(bool_and(coalesce((
 SELECT true FROM billing_workspace_usage_periods p
 WHERE p.workspace_id=ws.id AND p.period_start<=$2 AND p.period_end>$2 AND p.included_bytes IS NOT NULL
  AND (SELECT coalesce(sum(q.consumed_bytes),0) FROM billing_workspace_usage_periods q
       WHERE q.workspace_id=ws.id AND q.paid=p.paid AND coalesce(q.budget_start,q.period_start)=coalesce(p.budget_start,p.period_start)) >= p.included_bytes
  AND (NOT p.paid OR coalesce((SELECT sum(t.remaining_bytes) FROM billing_workspace_topups t WHERE t.workspace_id=ws.id AND t.purchased_at<=$2),0)=0)
 LIMIT 1),false)),false)
FROM ws`, account, at).Scan(&denied)
	return denied, err
}

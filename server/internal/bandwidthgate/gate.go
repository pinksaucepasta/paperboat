// Package bandwidthgate answers one question for hot paths that must never wait on billing:
// is every workspace an account can use out of bandwidth allowance? It is one read-only
// statement (MVCC: no write, no row lock, so it cannot queue behind usage debits) with a
// short timeout and a per-account cache. A failure answers "not denied" (fail open) unless the
// account was answered "denied" within knownDeniedHold, and starts one longer background lookup
// that caches the real answer: connectivity never waits on billing, a slow lookup cannot reopen
// an account known to be over its allowance, and a slow database only delays enforcement.
package bandwidthgate

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

const (
	defaultLookupTimeout = 150 * time.Millisecond
	answerTTL            = 10 * time.Second
	failureTTL           = 5 * time.Second
	// knownDeniedHold is how long a failed lookup keeps an account's last "denied"
	// answer: an account known to be over its allowance stays refused while billing
	// is slow (fail closed), but only for a bounded time without a fresh answer.
	knownDeniedHold = 2 * time.Minute
	cacheMax        = 4096
	// backgroundTimeout bounds the lookup a failed hot-path answer starts, so a database too
	// slow for the hot path still settles the account's answer within seconds.
	backgroundTimeout = 5 * time.Second
)

type entry struct {
	denied bool
	until  time.Time
	// deniedAt is when a lookup last answered "denied" (zero if never).
	deniedAt time.Time
}

// Gate is safe for concurrent use.
type Gate struct {
	store   *db.DB
	timeout time.Duration
	lookup  func(context.Context, string, time.Time) (bool, error)
	mu      sync.Mutex
	cache   map[string]entry
	// refreshing holds accounts with a background lookup in flight (at most one each).
	refreshing map[string]bool
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
	previous, ok := g.cache[account]
	if ok && now.Before(previous.until) {
		g.mu.Unlock()
		return previous.denied
	}
	g.mu.Unlock()
	lookupCtx, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()
	denied, err := g.lookup(lookupCtx, account, at)
	ttl := answerTTL
	deniedAt := previous.deniedAt
	switch {
	case err == nil && denied:
		deniedAt = now
	case err != nil && !deniedAt.IsZero() && now.Sub(deniedAt) < knownDeniedHold:
		slog.WarnContext(ctx, "bandwidth gate unavailable; keeping the account's recent denial", "error_type", fmt.Sprintf("%T", err))
		denied, ttl = true, failureTTL
	case err != nil:
		slog.WarnContext(ctx, "bandwidth gate unavailable; treating the account as within allowance", "error_type", fmt.Sprintf("%T", err))
		denied, ttl = false, failureTTL
	}
	if err != nil {
		g.refreshInBackground(account, at)
	}
	g.mu.Lock()
	if g.cache == nil || len(g.cache) >= cacheMax {
		g.cache = make(map[string]entry)
	}
	g.cache[account] = entry{denied: denied, until: now.Add(ttl), deniedAt: deniedAt}
	g.mu.Unlock()
	return denied
}

// refreshInBackground repeats a failed lookup without the hot path's deadline and caches its
// answer, so repeated hot-path timeouts cannot keep an over-allowance account open for good.
func (g *Gate) refreshInBackground(account string, at time.Time) {
	g.mu.Lock()
	if g.refreshing == nil {
		g.refreshing = make(map[string]bool)
	}
	if g.refreshing[account] {
		g.mu.Unlock()
		return
	}
	g.refreshing[account] = true
	g.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), backgroundTimeout)
		defer cancel()
		denied, err := g.lookup(ctx, account, at)
		now := time.Now()
		g.mu.Lock()
		defer g.mu.Unlock()
		delete(g.refreshing, account)
		if err != nil {
			return
		}
		next := entry{denied: denied, until: now.Add(answerTTL), deniedAt: g.cache[account].deniedAt}
		if denied {
			next.deniedAt = now
		}
		if g.cache == nil || len(g.cache) >= cacheMax {
			g.cache = make(map[string]entry)
		}
		g.cache[account] = next
	}()
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
// Lookup answers for a key: a workspace ID ("personal:<account>" or "team:<team>") is denied when
// that workspace is out of allowance; an account ID is denied when every workspace the account
// can use is.
func (g *Gate) Lookup(ctx context.Context, key string, at time.Time) (bool, error) {
	workspaces := `WITH ws AS (
 SELECT 'personal:'||$1::text AS id
 UNION
 SELECT 'team:'||m.team_id FROM team_members m JOIN teams t ON t.team_id=m.team_id
 WHERE m.account_id=$1 AND m.active AND t.deleted_at IS NULL
)`
	if strings.HasPrefix(key, "personal:") || strings.HasPrefix(key, "team:") {
		workspaces = `WITH ws AS (SELECT $1::text AS id)`
	}
	var denied bool
	err := g.store.SQL().QueryRowContext(ctx, workspaces+`
SELECT coalesce(bool_and(coalesce((
 SELECT true FROM billing_workspace_usage_periods p
 WHERE p.workspace_id=ws.id AND p.period_start<=$2 AND p.period_end>$2 AND p.included_bytes IS NOT NULL
  AND (SELECT coalesce(sum(q.consumed_bytes),0) FROM billing_workspace_usage_periods q
       WHERE q.workspace_id=ws.id AND q.paid=p.paid AND coalesce(q.budget_start,q.period_start)=coalesce(p.budget_start,p.period_start)) >= p.included_bytes
  AND (NOT p.paid OR coalesce((SELECT sum(t.remaining_bytes) FROM billing_workspace_topups t WHERE t.workspace_id=ws.id AND t.purchased_at<=$2),0)=0)
 LIMIT 1),false)),false)
FROM ws`, key, at).Scan(&denied)
	return denied, err
}

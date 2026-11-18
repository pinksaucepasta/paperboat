// Package bandwidthgate answers one question for hot paths that must never wait on billing:
// is every workspace an account can use throttled (its fair-use bandwidth used, no operator lift)? It is one read-only
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
	// cacheMax bounds the per-account cache; one entry is about 100 bytes.
	cacheMax = 65536
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

// accountPaid is whether account $1 is on a paid plan: its personal workspace has a paid period, or it is an active member of a
// team with a paid entitlement (the plan whose slower relay speed applies to it).
const accountPaid = `(` + personalPaid + `
 OR EXISTS(SELECT 1 FROM paperboat.team_members m WHERE m.account_id=$1 AND m.active AND paperboat.team_paid_entitled(m.team_id)))`

// personalPaid is whether account $1's personal workspace has a paid period.
const personalPaid = `EXISTS(SELECT 1 FROM paperboat.billing_workspaces w WHERE w.user_id=$1 AND w.team_id IS NULL AND w.product_code IS NOT NULL AND paperboat.billing_workspace_paid_until(w)>now())`

// relayBytes is the managed-relay bytes of workspace expression $w in the calendar month (UTC) of $4.
func relayBytes(workspace string) string {
	return `(SELECT coalesce(sum(e.upload_bytes+e.download_bytes),0) FROM paperboat.resource_bandwidth_events e WHERE e.workspace_id=` + workspace + `
 AND e.mode='relay' AND e.hosting='managed' AND e.interval_end >= date_trunc('month',$4::timestamptz AT TIME ZONE 'UTC') AT TIME ZONE 'UTC')`
}

// NewRelayVolume is a gate with the same cache and fail-safe rules whose question is another: has the account used up its relay
// allowance this calendar month (UTC)? Its personal workspace has carried its plan's included bytes through Paperboat's managed relays
// (freeIncluded, or perSeat on a paid plan), or a paid team it belongs to has carried perSeat times its seats, pooled over all its
// members. A relay then holds the account to the plan's slow speed. A failed lookup answers "not over" (full speed), so a slow
// database never slows an account on its own.
func NewRelayVolume(store *db.DB, freeIncluded, perSeat int64) *Gate {
	g := &Gate{store: store, timeout: defaultLookupTimeout}
	g.lookup = func(ctx context.Context, account string, at time.Time) (bool, error) {
		var over bool
		err := store.SQL().QueryRowContext(ctx, `SELECT `+relayBytes(`'personal:'||$1`)+` >= CASE WHEN `+personalPaid+` THEN $2::bigint ELSE $3::bigint END
 OR EXISTS(SELECT 1 FROM paperboat.team_members m JOIN paperboat.billing_workspaces tw ON tw.team_id=m.team_id AND tw.user_id IS NULL
  WHERE m.account_id=$1 AND m.active AND paperboat.team_paid_entitled(m.team_id)
  AND `+relayBytes(`'team:'||m.team_id`)+` >= $2::bigint * greatest(coalesce((SELECT g.amount FROM paperboat.billing_workspace_grants g WHERE g.workspace_id='team:'||m.team_id AND g.kind='seats' AND g.revoked_at IS NULL AND (g.ends_at IS NULL OR g.ends_at>now()) ORDER BY g.created_at DESC LIMIT 1),tw.quantity),1))`,
			account, perSeat, freeIncluded, at).Scan(&over)
		return over, err
	}
	return g
}

// PaidAccounts is the subset of accounts on a paid plan (see accountPaid).
func PaidAccounts(ctx context.Context, store *db.DB, accounts []string) (map[string]bool, error) {
	paid := map[string]bool{}
	for _, account := range accounts {
		var is bool
		if err := store.SQL().QueryRowContext(ctx, `SELECT `+accountPaid, account).Scan(&is); err != nil {
			return nil, err
		}
		paid[account] = is
	}
	return paid, nil
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
	g.put(account, entry{denied: denied, until: now.Add(ttl), deniedAt: deniedAt}, now)
	g.mu.Unlock()
	return denied
}

// put stores an entry with g.mu held. A full cache drops expired entries, then entries without a recent denial. If
// only recent denials remain, an answer that is not a denial goes uncached (the next call looks it up again) rather
// than reopening another account, and a new denial replaces the one confirmed longest ago. It is never wiped.
func (g *Gate) put(account string, e entry, now time.Time) {
	if g.cache == nil {
		g.cache = make(map[string]entry)
	}
	if _, ok := g.cache[account]; !ok && len(g.cache) >= cacheMax {
		for _, pass := range []func(entry) bool{
			func(o entry) bool { return !now.Before(o.until) && now.Sub(o.deniedAt) >= knownDeniedHold },
			func(o entry) bool { return now.Sub(o.deniedAt) >= knownDeniedHold },
		} {
			for k, o := range g.cache {
				if pass(o) {
					delete(g.cache, k)
					break
				}
			}
			if len(g.cache) < cacheMax {
				break
			}
		}
		if len(g.cache) >= cacheMax {
			if !e.denied {
				return
			}
			oldest := ""
			for k, o := range g.cache {
				if oldest == "" || o.deniedAt.Before(g.cache[oldest].deniedAt) {
					oldest = k
				}
			}
			delete(g.cache, oldest)
		}
	}
	g.cache[account] = e
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
		g.put(account, next, now)
	}()
}

// DeniedAmong answers for many accounts at once (a relay's connected accounts). Cached answers
// are free; at most maxFresh uncached accounts are looked up, concurrently (16 at a time), each
// under its own lookup timeout (Denied's), so an account later in the batch is never failed open
// by the time earlier ones took; the call lasts at most maxFresh/16 timeouts. An account whose
// lookup fails counts as not denied this round (it is looked up on a later one). The result lists
// the denied accounts, sorted.
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
 WHERE m.account_id=$1 AND m.active AND t.deleted_at IS NULL AND NOT t.suspended
)`
	if strings.HasPrefix(key, "personal:") || strings.HasPrefix(key, "team:") {
		workspaces = `WITH ws AS (SELECT $1::text AS id)`
	}
	var denied bool
	err := g.store.SQL().QueryRowContext(ctx, workspaces+`
SELECT coalesce(bool_and(coalesce((
 SELECT true FROM billing_workspace_usage_periods p
 WHERE p.workspace_id=ws.id AND p.period_start<=$2 AND p.period_end>$2 AND p.included_bytes IS NOT NULL AND NOT workspace_throttle_lifted(ws.id)
  AND (SELECT coalesce(sum(q.consumed_bytes),0) FROM billing_workspace_usage_periods q
       WHERE q.workspace_id=ws.id AND q.paid=p.paid AND coalesce(q.budget_start,q.period_start)=coalesce(p.budget_start,p.period_start)) >= p.included_bytes+workspace_granted_bytes(ws.id)
  AND (NOT p.paid OR coalesce((SELECT sum(t.remaining_bytes) FROM billing_workspace_topups t WHERE t.workspace_id=ws.id AND t.purchased_at<=$2 AND t.expires_at>$2),0)=0)
 LIMIT 1),false)),false)
FROM ws`, key, at).Scan(&denied)
	return denied, err
}

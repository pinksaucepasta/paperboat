package peersessions

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// RelayDeniedQuotaExhausted is the typed reason a network refresh carries when hosted relay
// grants were withheld because the account has no bandwidth allowance left anywhere.
const RelayDeniedQuotaExhausted = "bandwidth_quota_exhausted"

const (
	relayQuotaLookupTimeout = 150 * time.Millisecond
	relayQuotaCacheTTL      = 10 * time.Second
	relayQuotaFailureTTL    = 5 * time.Second
	relayQuotaCacheMax      = 4096
)

// ConfigureQuota makes hosted relay grants admission-checked against the account's remaining
// bandwidth. billingURL is the console's Billing page, published to the client so it can tell
// the user where to add bandwidth; it may be empty.
func (s *NetworkService) ConfigureQuota(billingURL string) {
	s.quotaEnabled, s.billingURL = true, billingURL
}

type relayQuotaEntry struct {
	denied bool
	until  time.Time
}

type relayQuotaCache struct {
	mu      sync.Mutex
	entries map[string]relayQuotaEntry
}

func (c *relayQuotaCache) get(account string, now time.Time) (bool, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[account]
	if !ok || !now.Before(entry.until) {
		return false, false
	}
	return entry.denied, true
}

func (c *relayQuotaCache) put(account string, denied bool, until time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil || len(c.entries) >= relayQuotaCacheMax {
		c.entries = make(map[string]relayQuotaEntry)
	}
	c.entries[account] = relayQuotaEntry{denied, until}
}

// relayQuotaWithheld is the network-configuration gate and sits on a hot path: it is a single
// read-only statement (never a write, never a row lock, so it cannot queue behind usage
// debits), bounded by a short timeout, and cached per account. Peer networking must never
// depend on billing: any error or timeout keeps relay grants available (flows are still
// metered and charged when admitted), is logged, and is never returned to the caller.
func (s *NetworkService) relayQuotaWithheld(ctx context.Context, account string, at time.Time) bool {
	if !s.quotaEnabled || account == "" {
		return false
	}
	now := time.Now()
	if denied, ok := s.quotaCache.get(account, now); ok {
		return denied
	}
	lookup := s.quotaLookup
	if lookup == nil {
		lookup = s.lookupRelayQuota
	}
	lookupCtx, cancel := context.WithTimeout(ctx, relayQuotaLookupTimeout)
	defer cancel()
	denied, err := lookup(lookupCtx, account, at)
	if err != nil {
		slog.WarnContext(ctx, "relay quota check unavailable; relay grants stay available", "error_type", fmt.Sprintf("%T", err))
		s.quotaCache.put(account, false, now.Add(relayQuotaFailureTTL))
		return false
	}
	s.quotaCache.put(account, denied, now.Add(relayQuotaCacheTTL))
	return denied
}

// lookupRelayQuota reports whether EVERY workspace available to the account (its personal
// workspace and its active teams) is out of allowance. A grant names no workspace, so one
// workspace with allowance left keeps relays available; traffic is then charged to the
// workspace that admitted each flow. A workspace is exhausted when a current usage period's
// consumption (summed over its budget segments) reached the included bytes and, for a paid
// period, no top-up credit remains. A workspace with no period yet is not exhausted.
func (s *NetworkService) lookupRelayQuota(ctx context.Context, account string, at time.Time) (bool, error) {
	var denied bool
	err := s.store.SQL().QueryRowContext(ctx, `WITH ws AS (
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

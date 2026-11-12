package control

import (
	"context"
	"math/rand/v2"
	"net/netip"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
	"github.com/pinksaucepasta/paperboat/tunnel/internal/route"
)

// IngressAuthority holds the edge's ingress decisions from the server. One fetch runs at a time
// (single-flight); a stale set is served while Run refreshes it in the background, so accepts never
// wait on the network except to admit a route the edge has not heard of yet. While the control
// plane is unreachable the last good set stays (F21-17, "until revoked" for the edge): public
// decisions are renewed locally, restricted ones lapse at their expiry because they stand for a
// person's grant. A decision ends only with a newer snapshot. Prior art: Teleport's proxy serves
// from its event-fed cache while auth is down (lib/cache); cloudflared keeps its last remote config.
type IngressAuthority struct {
	Client       *HTTPClient
	NodeID       string
	ProcessEpoch string
	// Grace, when set and positive, is the remaining window after a handover takeover: a preview miss is asked
	// again after previewRecheckInGrace instead of IngressRefreshInterval, because the server issues the
	// replacement's decision a moment after the host's carrier attaches there.
	Grace         func() time.Duration
	mu            sync.RWMutex // AdmitsPreview, asked on every missed request, only reads
	decisions     []connectorprotocol.IngressDecision
	quotaDenied   map[string]time.Time                        // hostnames withheld only for bandwidth quota (to their reset time), from the same fetch
	blocked       map[string]bool                             // hostnames taken down by an operator (to whether the takedown is legal), from the same fetch
	ipRules       map[string]addressRule                      // owner IP allow/deny lists per hostname, from the same fetch
	http01        map[string]connectorprotocol.HTTP01Response // by token, from the same fetch
	fetched       time.Time
	invalidated   bool
	invalidations uint64 // a fetch started before the latest Invalidate does not clear it
	version       string // of the held set; asked back so an unchanged set comes as fresh times only
	failures      int
	retryAt       time.Time
	lastErr       error
	outOfSync     time.Time // first failed refresh after a good one; zero while in sync
	inflight      chan struct{}
	warm          chan struct{} // closed by the first successful fetch (Warm)
	wake          chan struct{}
	stop          context.CancelFunc
	done          chan struct{}
	previewMisses map[previewAuthorityKey]previewMiss
	lastForced    time.Time // the last forced fetch a preview miss caused, for the grace's global rate limit
}

// previewMiss is one refresh for a publication the edge has not heard of; done closes when it ends.
type previewMiss struct {
	at     time.Time
	done   chan struct{}
	streak int // consecutive misses for this publication; each one doubles how long the next is not rechecked
}

// previewRecheckInGrace is how long a preview miss is remembered during a takeover grace; from the third miss of the
// same publication on it doubles, up to previewMissMaxStreak (0.5 s, 0.5 s, 1 s, 2 s): a decision issued a moment late
// is still found quickly, a name that never comes back stops costing fetches. It is also the
// least time between two forced fetches for all publications together: one fetch answers every name, so held
// connections of many names must not each cause their own.
const (
	previewRecheckInGrace = 500 * time.Millisecond
	previewMissMaxStreak  = 3
)

func (a *IngressAuthority) previewMissTTL(streak int) time.Duration {
	if a.Grace != nil && a.Grace() > 0 {
		return previewRecheckInGrace << max(0, min(streak, previewMissMaxStreak)-1)
	}
	return connectorprotocol.IngressRefreshInterval
}

// ingressRetryMaximum bounds the backoff between failed refreshes.
const ingressRetryMaximum = 30 * time.Second

// A miss is scoped to the exact locally admitted publication and carrier.
// Negative checks share the existing refresh lifetime and snapshot bound.
type previewAuthorityKey struct {
	publication, hostname string
	revision              uint64
	open                  connectorprotocol.StreamOpen
}

// Invalidate fences snapshots fetched before a newly acknowledged route
// becomes locally selectable: the next read refreshes first.
func (a *IngressAuthority) Invalidate() {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.invalidated = true
	a.invalidations++
	a.previewMisses = nil
	a.mu.Unlock()
}

// OutOfSync reports since when refreshes fail while the edge serves its last good decisions.
func (a *IngressAuthority) OutOfSync() (time.Time, bool) {
	if a == nil {
		return time.Time{}, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.outOfSync, !a.outOfSync.IsZero()
}

// Warm is closed once the first fetch of the ingress decisions has been applied: until then a route this edge
// simply has not loaded yet would be denied, so a replacement edge process takes traffic only after it.
func (a *IngressAuthority) Warm() <-chan struct{} {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.warm == nil {
		a.warm = make(chan struct{})
	}
	return a.warm
}

func (a *IngressAuthority) markWarmLocked() {
	if a.warm == nil {
		a.warm = make(chan struct{})
	}
	select {
	case <-a.warm:
	default:
		close(a.warm)
	}
}

func (a *IngressAuthority) Snapshot(ctx context.Context) ([]connectorprotocol.IngressDecision, error) {
	if a == nil || a.Client == nil || ctx == nil {
		return nil, ErrControlInvalid
	}
	return a.snapshot(ctx, false)
}

// Start runs the background refresh until Shutdown.
func (a *IngressAuthority) Start(ctx context.Context) error {
	if a == nil || a.Client == nil || ctx == nil {
		return ErrControlInvalid
	}
	a.mu.Lock()
	if a.stop != nil {
		a.mu.Unlock()
		return ErrControlInvalid
	}
	if a.wake == nil {
		a.wake = make(chan struct{}, 1)
	}
	runCtx, cancel := context.WithCancel(ctx)
	a.stop, a.done = cancel, make(chan struct{})
	wake, done := a.wake, a.done
	a.mu.Unlock()
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		_, _ = a.refresh(runCtx, false)
		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
			case <-wake:
			}
			a.mu.Lock()
			stale := !a.fetched.IsZero() && time.Since(a.fetched) >= connectorprotocol.IngressRefreshInterval
			a.mu.Unlock()
			_, _ = a.refresh(runCtx, stale)
		}
	}()
	return nil
}

func (a *IngressAuthority) Shutdown(ctx context.Context) error {
	a.mu.Lock()
	stop, done := a.stop, a.done
	a.mu.Unlock()
	if stop == nil {
		return nil
	}
	stop()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// snapshot is the accept path. With the background loop running it never fetches while the control
// plane is unhealthy (out of sync or backing off): it serves the last decisions, public ones
// renewed locally past their expiry, restricted ones lapsing, and kicks the loop. Only a healthy
// edge meeting a route it has not heard of (force) or one fenced by Invalidate waits for one
// bounded refresh, so a route just made ready is not refused. Without a loop (tests) it fetches inline.
func (a *IngressAuthority) snapshot(ctx context.Context, force bool) ([]connectorprotocol.IngressDecision, error) {
	a.mu.Lock()
	if a.stop == nil {
		a.mu.Unlock()
		return a.refresh(ctx, force)
	}
	now := time.Now().UTC()
	healthy := a.outOfSync.IsZero() && !now.Before(a.retryAt) && !a.fetched.IsZero()
	if (force || a.invalidated) && healthy {
		a.mu.Unlock()
		return a.refresh(ctx, force)
	}
	stale := a.invalidated || a.fetched.IsZero() || now.Sub(a.fetched) >= connectorprotocol.IngressRefreshInterval
	if stale {
		a.kickLocked()
	}
	defer a.mu.Unlock()
	if a.fetched.IsZero() {
		if a.lastErr != nil {
			return nil, a.lastErr
		}
		return nil, ErrControlUnavailable
	}
	return a.servedLocked(now), nil
}

// refresh returns the current decisions, fetching first when the set is missing, invalidated,
// forced, or holds an expired decision, unless refreshes are backing off after a failure.
func (a *IngressAuthority) refresh(ctx context.Context, force bool) ([]connectorprotocol.IngressDecision, error) {
	for {
		a.mu.Lock()
		now := time.Now().UTC()
		need := force || a.invalidated || a.fetched.IsZero()
		for _, decision := range a.decisions {
			if !now.Before(decision.ExpiresAt) {
				need = true
				break
			}
		}
		if !need || now.Before(a.retryAt) {
			if !a.fetched.IsZero() && now.Sub(a.fetched) >= connectorprotocol.IngressRefreshInterval {
				a.kickLocked()
			}
			defer a.mu.Unlock()
			if a.fetched.IsZero() {
				return nil, a.lastErr
			}
			return a.servedLocked(now), nil
		}
		if a.inflight != nil {
			done := a.inflight
			a.mu.Unlock()
			select {
			case <-done:
				force = false
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		done := make(chan struct{})
		a.inflight = done
		generation := a.invalidations
		a.mu.Unlock()
		// The fetch is shared, so it keeps its own bound, never the starting caller's: a 2 s public TCP reconcile
		// cancelled 2-3 s answers and counted a control failure for every waiter (server deploys 2026-11-12 19:54Z).
		go func() {
			fetch, cancel := context.WithTimeout(context.WithoutCancel(ctx), connectorprotocol.IngressFetchTimeout)
			snapshot, err := a.Client.IngressDecisions(fetch, a.NodeID, a.ProcessEpoch, a.heldVersion())
			cancel()
			a.mu.Lock()
			a.recordLocked(time.Now().UTC(), snapshot, err, generation == a.invalidations)
			a.inflight = nil
			close(done)
			a.mu.Unlock()
		}()
		select {
		case <-done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		a.mu.Lock()
		if a.fetched.IsZero() {
			err := a.lastErr
			a.mu.Unlock()
			return nil, err
		}
		out := a.servedLocked(time.Now().UTC())
		a.mu.Unlock()
		return out, nil
	}
}

func (a *IngressAuthority) recordLocked(now time.Time, snapshot IngressSnapshot, err error, fenced bool) {
	if err != nil {
		a.failures++
		a.lastErr = err
		a.retryAt = now.Add(ingressRetryDelay(a.failures))
		if a.outOfSync.IsZero() && !a.fetched.IsZero() {
			a.outOfSync = now
		}
		return
	}
	if snapshot.Unchanged {
		if err := a.refreshTimesLocked(now, snapshot); err != nil {
			a.version = ""
			a.recordLocked(now, IngressSnapshot{}, err, fenced)
		} else {
			a.markWarmLocked()
			a.fetched = now
			if fenced {
				a.invalidated = false
			}
			a.failures, a.retryAt, a.lastErr, a.outOfSync = 0, time.Time{}, nil, time.Time{}
		}
		return
	}
	a.decisions, a.fetched, a.version = snapshot.Decisions, now, snapshot.Version
	a.markWarmLocked()
	if fenced {
		a.invalidated = false
	}
	a.failures, a.retryAt, a.lastErr, a.outOfSync = 0, time.Time{}, nil, time.Time{}
	a.quotaDenied = make(map[string]time.Time, len(snapshot.QuotaDenied))
	for _, d := range snapshot.QuotaDenied {
		a.quotaDenied[d.Hostname] = d.ResetsAt
	}
	a.blocked = make(map[string]bool, len(snapshot.Blocked))
	for _, b := range snapshot.Blocked {
		a.blocked[b.Hostname] = b.Legal
	}
	a.ipRules = make(map[string]addressRule, len(snapshot.IPRules))
	for _, r := range snapshot.IPRules {
		a.ipRules[r.Hostname] = compileAddressRule(r)
	}
	a.http01 = make(map[string]connectorprotocol.HTTP01Response, len(snapshot.HTTP01))
	for _, r := range snapshot.HTTP01 {
		a.http01[r.Token] = r
	}
}

// heldVersion is the version of the held set, or empty when it must be fetched whole.
func (a *IngressAuthority) heldVersion() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.fetched.IsZero() {
		return ""
	}
	return a.version
}

// refreshTimesLocked gives the held decisions the fresh times of an unchanged answer.
func (a *IngressAuthority) refreshTimesLocked(now time.Time, snapshot IngressSnapshot) error {
	if len(snapshot.Expiries) != len(a.decisions) {
		return ErrControlUnavailable
	}
	next := append([]connectorprotocol.IngressDecision(nil), a.decisions...)
	for i := range next {
		next[i].IssuedAt, next[i].ExpiresAt = snapshot.IssuedAt, snapshot.Expiries[i]
		if next[i].Validate(now) != nil {
			return ErrControlUnavailable
		}
	}
	a.decisions = next
	return nil
}

// HTTP01KeyAuthorization returns the key authorization the last good snapshot
// holds for an http-01 challenge at host. Responses are never served for a
// different hostname than the one the server issued them for.
func (a *IngressAuthority) HTTP01KeyAuthorization(host, token string) (string, bool) {
	if a == nil {
		return "", false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	response, ok := a.http01[token]
	if !ok || response.Hostname != host {
		return "", false
	}
	return response.KeyAuthorization, true
}

// servedLocked copies the decisions. A public decision near its expiry is renewed locally for one
// authority lifetime when refreshes fail or are overdue (it ends only with a newer snapshot); an
// expired restricted one is left out.
func (a *IngressAuthority) servedLocked(now time.Time) []connectorprotocol.IngressDecision {
	out := make([]connectorprotocol.IngressDecision, 0, len(a.decisions))
	for i, d := range a.decisions {
		overdue := !a.outOfSync.IsZero() || !now.Before(d.ExpiresAt)
		if overdue && now.Add(connectorprotocol.IngressRefreshInterval).After(d.ExpiresAt) {
			if d.Binding.Audience != "public" {
				continue
			}
			d.IssuedAt, d.ExpiresAt = now, now.Add(connectorprotocol.IngressAuthorityLifetime)
			a.decisions[i] = d
		}
		out = append(out, d)
	}
	return out
}

func (a *IngressAuthority) kickLocked() {
	if a.wake == nil {
		return
	}
	select {
	case a.wake <- struct{}{}:
	default:
	}
}

// ingressRetryDelay is a full-jitter exponential backoff from 1 s up to ingressRetryMaximum.
func ingressRetryDelay(failures int) time.Duration {
	ceiling := time.Second << min(failures-1, 5)
	if ceiling > ingressRetryMaximum {
		ceiling = ingressRetryMaximum
	}
	return time.Duration(rand.Int64N(int64(ceiling)) + 1)
}

func (a *IngressAuthority) ResolveDecision(ctx context.Context, candidate connectorprotocol.IngressDecision) (connectorprotocol.IngressDecision, error) {
	decisions, err := a.Snapshot(ctx)
	if err != nil {
		return connectorprotocol.IngressDecision{}, err
	}
	now := time.Now().UTC()
	for _, current := range decisions {
		open := connectorprotocol.StreamOpen{Protocol: connectorprotocol.ProtocolName, Version: connectorprotocol.ProtocolVersion, AccountID: candidate.Binding.AccountID, TunnelID: candidate.Binding.TunnelID, ConnectorID: candidate.ConnectorID, SessionID: candidate.SessionID, ProcessGeneration: candidate.ProcessGeneration, Generation: candidate.ConfigGeneration, RouteID: candidate.Binding.RouteID, RequestID: "tcp-authority-refresh", Kind: "tcp_public"}
		if candidate.Revalidate(current, open, a.NodeID, a.ProcessEpoch, now) == nil {
			return current, nil
		}
	}
	return connectorprotocol.IngressDecision{}, connectorprotocol.ErrIngressDenied
}

// AdmitsPreview reports whether the held snapshot still admits a preview publication on hostname (an ephemeral
// decision that has not lapsed; a public one is renewed locally until a newer snapshot ends it). It is how the edge
// tells an app that lost its carrier for a moment from a name the server never admitted. Only decisions issued to
// this edge count: a preview another edge serves is answered 404 here as before.
func (a *IngressAuthority) AdmitsPreview(hostname string) bool {
	if a == nil || hostname == "" {
		return false
	}
	now := time.Now()
	a.mu.RLock()
	defer a.mu.RUnlock()
	for _, d := range a.decisions {
		if d.Binding.Hostname == hostname && d.EdgeNodeID == a.NodeID && d.Binding.Lifecycle == connectorprotocol.TunnelEphemeral && (d.Binding.Audience == "public" || now.Before(d.ExpiresAt)) {
			return true
		}
	}
	return false
}

// QuotaDenied reports whether the last server snapshot withheld the hostname with a typed refusal: its
// workspace is out of bandwidth allowance, or an operator took it down. The edge then still completes
// TLS for that name so the request is answered with the typed page (429, or 451/403), instead of a
// silent connection close.
func (a *IngressAuthority) QuotaDenied(hostname string) bool {
	return a.withheldError(hostname) != nil
}

// withheldError is the typed refusal for a taken-down hostname, or for a quota-denied one with its reset time;
// nil otherwise. A takedown wins over the quota.
func (a *IngressAuthority) withheldError(hostname string) error {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if legal, blocked := a.blocked[hostname]; blocked {
		return connectorprotocol.PublicationBlockedError{Legal: legal}
	}
	resetsAt, denied := a.quotaDenied[hostname]
	if !denied {
		return nil
	}
	return connectorprotocol.IngressQuotaError{ResetsAt: resetsAt}
}

func (a *IngressAuthority) Resolve(ctx context.Context, r route.RouteRule) (connectorprotocol.IngressDecision, error) {
	if a == nil || a.Client == nil || ctx == nil {
		return connectorprotocol.IngressDecision{}, ErrControlInvalid
	}
	decisions, err := a.Snapshot(ctx)
	if err != nil {
		return connectorprotocol.IngressDecision{}, err
	}
	id := r.RouteID
	if id == "" {
		id = r.ID
	}
	for _, d := range decisions {
		if d.Binding.Hostname == r.Hostname && d.Binding.RouteID == id && d.Binding.TunnelID == r.TunnelID && d.Binding.AccountID == r.AccountID && d.ConnectorID == r.ConnectorID && d.SessionID == r.ConnectorSessionID && d.ProcessGeneration == r.ConnectorProcessGeneration && d.ConfigGeneration == r.ConfigGeneration && d.AssignmentGeneration == r.AssignmentGeneration && d.Binding.RouteGeneration == r.RouteGeneration && d.Binding.Audience == r.AccessMode && d.PolicyGeneration == r.ViewerPolicyGeneration && d.Validate(time.Now().UTC()) == nil {
			return d, nil
		}
	}
	// The route exists but its workspace is out of allowance: say so, never forward.
	if err := a.withheldError(r.Hostname); err != nil {
		return connectorprotocol.IngressDecision{}, err
	}
	return connectorprotocol.IngressDecision{}, connectorprotocol.ErrIngressDenied
}

// ResolvePreview matches the ready local carrier against a current server
// publication. A hostname alone cannot supply accounting or forwarding authority.
func (a *IngressAuthority) ResolvePreview(ctx context.Context, publication, hostname string, revision uint64, open connectorprotocol.StreamOpen) (connectorprotocol.IngressDecision, error) {
	if a == nil || a.Client == nil || ctx == nil || connectorprotocol.ValidateIdentifier(publication) != nil || len(hostname) == 0 || len(hostname) > 253 || revision == 0 || open.Validate() != nil {
		return connectorprotocol.IngressDecision{}, ErrControlInvalid
	}
	decisions, err := a.snapshot(ctx, false)
	if err != nil {
		return connectorprotocol.IngressDecision{}, err
	}
	selectDecision := func(decisions []connectorprotocol.IngressDecision) (connectorprotocol.IngressDecision, bool) {
		for _, d := range decisions {
			if d.Binding.Lifecycle == connectorprotocol.TunnelEphemeral && d.Binding.Audience == "public" && d.Binding.PublicationID == publication && d.Binding.Hostname == hostname && d.Binding.RouteGeneration == revision && d.Authorize(d, open, a.NodeID, a.ProcessEpoch, time.Now().UTC()) == nil {
				return d, true
			}
		}
		return connectorprotocol.IngressDecision{}, false
	}
	if d, ok := selectDecision(decisions); ok {
		return d, nil
	}
	// Withheld only because the workspace is out of allowance: refuse with the typed reason,
	// without the readiness refresh below (nothing about origin readiness can change it).
	if err := a.withheldError(hostname); err != nil {
		return connectorprotocol.IngressDecision{}, err
	}
	// A snapshot read before origin readiness may legitimately omit a now-ready
	// local publication. Refresh that authority once; do not retry HTTP traffic.
	keyOpen := open
	keyOpen.RequestID = ""
	key := previewAuthorityKey{publication: publication, hostname: hostname, revision: revision, open: keyOpen}
	a.mu.Lock()
	now := time.Now().UTC()
	for k, miss := range a.previewMisses {
		if now.Sub(miss.at) >= a.previewMissTTL(previewMissMaxStreak) {
			delete(a.previewMisses, k)
		}
	}
	streak := 0
	if miss, checked := a.previewMisses[key]; checked && now.Sub(miss.at) >= a.previewMissTTL(miss.streak) {
		streak = miss.streak + 1 // stale: recheck, but wait longer next time
	} else if checked {
		a.mu.Unlock()
		// Another request is refreshing for this publication: share its answer, never a second fetch.
		select {
		case <-miss.done:
		case <-ctx.Done():
			return connectorprotocol.IngressDecision{}, ctx.Err()
		}
		if decisions, err = a.snapshot(ctx, false); err == nil {
			if d, ok := selectDecision(decisions); ok {
				return d, nil
			}
		}
		return connectorprotocol.IngressDecision{}, connectorprotocol.ErrIngressDenied
	}
	if len(a.previewMisses) >= maximumIngressDecisions {
		a.mu.Unlock()
		return connectorprotocol.IngressDecision{}, ErrControlUnavailable
	}
	if a.previewMisses == nil {
		a.previewMisses = make(map[previewAuthorityKey]previewMiss)
	}
	miss := previewMiss{at: now, done: make(chan struct{}), streak: streak}
	a.previewMisses[key] = miss
	force := true
	if a.Grace != nil && a.Grace() > 0 {
		if now.Sub(a.lastForced) < previewRecheckInGrace {
			force = false // another name's miss fetched just now; its snapshot is as fresh as a new one would be
		} else {
			a.lastForced = now
		}
	}
	a.mu.Unlock()
	defer close(miss.done)
	if decisions, err = a.snapshot(ctx, force); err != nil {
		return connectorprotocol.IngressDecision{}, err
	}
	if d, ok := selectDecision(decisions); ok {
		a.mu.Lock()
		if current, ok := a.previewMisses[key]; ok && current.done == miss.done {
			delete(a.previewMisses, key)
		}
		a.mu.Unlock()
		return d, nil
	}
	if err := a.withheldError(hostname); err != nil {
		return connectorprotocol.IngressDecision{}, err
	}
	return connectorprotocol.IngressDecision{}, connectorprotocol.ErrIngressDenied
}

// addressRule is one hostname's owner IP lists, parsed.
type addressRule struct{ allow, deny []netip.Prefix }

// compileAddressRule parses the lists; an entry that does not parse matches nothing in allow (so an
// allow list never widens) and everything in deny (so a deny list never narrows).
func compileAddressRule(r connectorprotocol.IngressIPRule) addressRule {
	var rule addressRule
	for _, entry := range r.Allow {
		if prefix, err := netip.ParsePrefix(entry); err == nil {
			rule.allow = append(rule.allow, prefix)
		} else if len(rule.allow) == 0 {
			rule.allow = append(rule.allow, netip.Prefix{}) // keeps the list non-empty: nothing is allowed by it
		}
	}
	for _, entry := range r.Deny {
		prefix, err := netip.ParsePrefix(entry)
		if err != nil {
			prefix = netip.MustParsePrefix("::/0")
			rule.deny = append(rule.deny, netip.MustParsePrefix("0.0.0.0/0"))
		}
		rule.deny = append(rule.deny, prefix)
	}
	return rule
}

// AddressAllowed reports whether the owner's IP lists for hostname let address in: no deny entry
// matches it, and an allow entry does when the allow list is not empty. A hostname without lists
// lets every address in; an address that does not parse is refused only when lists exist.
func (a *IngressAuthority) AddressAllowed(hostname, address string) bool {
	if a == nil {
		return true
	}
	a.mu.Lock()
	rule, ok := a.ipRules[hostname]
	a.mu.Unlock()
	if !ok {
		return true
	}
	addr, err := netip.ParseAddr(address)
	if err != nil {
		return false
	}
	addr = addr.Unmap().WithZone("")
	for _, prefix := range rule.deny {
		if prefix.Contains(addr) {
			return false
		}
	}
	if len(rule.allow) == 0 {
		return true
	}
	for _, prefix := range rule.allow {
		if prefix.IsValid() && prefix.Contains(addr) {
			return true
		}
	}
	return false
}

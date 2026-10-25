package control

import (
	"context"
	"math/rand/v2"
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
	Client        *HTTPClient
	NodeID        string
	ProcessEpoch  string
	mu            sync.Mutex
	decisions     []connectorprotocol.IngressDecision
	quotaDenied   map[string]bool                             // hostnames withheld only for bandwidth quota, from the same fetch
	http01        map[string]connectorprotocol.HTTP01Response // by token, from the same fetch
	fetched       time.Time
	invalidated   bool
	invalidations uint64 // a fetch started before the latest Invalidate does not clear it
	failures      int
	retryAt       time.Time
	lastErr       error
	outOfSync     time.Time // first failed refresh after a good one; zero while in sync
	inflight      chan struct{}
	wake          chan struct{}
	stop          context.CancelFunc
	done          chan struct{}
	previewMisses map[previewAuthorityKey]previewMiss
}

// previewMiss is one refresh for a publication the edge has not heard of; done closes when it ends.
type previewMiss struct {
	at   time.Time
	done chan struct{}
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
			_, _ = a.snapshot(runCtx, stale)
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

// snapshot returns the current decisions, refreshing first when the set is missing, invalidated,
// forced, or holds an expired decision, unless refreshes are backing off after a failure.
func (a *IngressAuthority) snapshot(ctx context.Context, force bool) ([]connectorprotocol.IngressDecision, error) {
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
		fetch, cancel := context.WithTimeout(ctx, 2*time.Second)
		snapshot, err := a.Client.IngressDecisions(fetch, a.NodeID, a.ProcessEpoch)
		cancel()
		a.mu.Lock()
		a.recordLocked(time.Now().UTC(), snapshot, err, generation == a.invalidations)
		a.inflight = nil
		close(done)
		if a.fetched.IsZero() {
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
	a.decisions, a.fetched = snapshot.Decisions, now
	if fenced {
		a.invalidated = false
	}
	a.failures, a.retryAt, a.lastErr, a.outOfSync = 0, time.Time{}, nil, time.Time{}
	a.quotaDenied = make(map[string]bool, len(snapshot.QuotaDenied))
	for _, d := range snapshot.QuotaDenied {
		a.quotaDenied[d.Hostname] = true
	}
	a.http01 = make(map[string]connectorprotocol.HTTP01Response, len(snapshot.HTTP01))
	for _, r := range snapshot.HTTP01 {
		a.http01[r.Token] = r
	}
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

// servedLocked copies the decisions. Out of sync, a public decision near its expiry is renewed
// locally for one authority lifetime and an expired restricted one is left out.
func (a *IngressAuthority) servedLocked(now time.Time) []connectorprotocol.IngressDecision {
	out := make([]connectorprotocol.IngressDecision, 0, len(a.decisions))
	for i, d := range a.decisions {
		if !a.outOfSync.IsZero() && now.Add(connectorprotocol.IngressRefreshInterval).After(d.ExpiresAt) {
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

// QuotaDenied reports whether the hostname's route exists but the last server snapshot left it out
// only because its workspace is out of bandwidth allowance. The edge then still completes TLS for
// that name so the request can be refused with the typed 429, instead of a silent connection close.
func (a *IngressAuthority) QuotaDenied(hostname string) bool {
	if a == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.quotaDenied[hostname]
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
	a.mu.Lock()
	exhausted := a.quotaDenied[r.Hostname]
	a.mu.Unlock()
	if exhausted {
		return connectorprotocol.IngressDecision{}, connectorprotocol.ErrIngressQuotaExhausted
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
	if a.QuotaDenied(hostname) {
		return connectorprotocol.IngressDecision{}, connectorprotocol.ErrIngressQuotaExhausted
	}
	// A snapshot read before origin readiness may legitimately omit a now-ready
	// local publication. Refresh that authority once; do not retry HTTP traffic.
	keyOpen := open
	keyOpen.RequestID = ""
	key := previewAuthorityKey{publication: publication, hostname: hostname, revision: revision, open: keyOpen}
	a.mu.Lock()
	now := time.Now().UTC()
	for k, miss := range a.previewMisses {
		if now.Sub(miss.at) >= connectorprotocol.IngressRefreshInterval {
			delete(a.previewMisses, k)
		}
	}
	if miss, checked := a.previewMisses[key]; checked {
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
	miss := previewMiss{at: now, done: make(chan struct{})}
	a.previewMisses[key] = miss
	a.mu.Unlock()
	defer close(miss.done)
	if decisions, err = a.snapshot(ctx, true); err != nil {
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
	if a.QuotaDenied(hostname) {
		return connectorprotocol.IngressDecision{}, connectorprotocol.ErrIngressQuotaExhausted
	}
	return connectorprotocol.IngressDecision{}, connectorprotocol.ErrIngressDenied
}

package control

import (
	"context"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat-tunnel/internal/route"
	"github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
)

// IngressAuthority caches only the server's bounded decision lifetime. Fetches
// coalesce per edge process; a failed refresh cannot extend cached authority.
type IngressAuthority struct {
	Client        *HTTPClient
	NodeID        string
	ProcessEpoch  string
	mu            sync.Mutex
	decisions     []connectorprotocol.IngressDecision
	quotaDenied   map[string]bool // hostnames withheld only for bandwidth quota, from the same fetch
	fetched       time.Time
	previewMisses map[previewAuthorityKey]time.Time
}

// A miss is scoped to the exact locally admitted publication and carrier.
// Negative checks share the existing refresh lifetime and snapshot bound.
type previewAuthorityKey struct {
	publication, hostname string
	revision              uint64
	open                  connectorprotocol.StreamOpen
}

// Invalidate fences snapshots fetched before a newly acknowledged route
// becomes locally selectable. It serializes with an in-flight refresh.
func (a *IngressAuthority) Invalidate() {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.fetched = time.Time{}
	a.previewMisses = nil
	a.mu.Unlock()
}

func (a *IngressAuthority) Snapshot(ctx context.Context) ([]connectorprotocol.IngressDecision, error) {
	if a == nil || a.Client == nil || ctx == nil {
		return nil, ErrControlInvalid
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.snapshotLocked(ctx, false)
}

func (a *IngressAuthority) snapshotLocked(ctx context.Context, force bool) ([]connectorprotocol.IngressDecision, error) {
	now := time.Now().UTC()
	// A node heartbeat or publication deadline can shorten a decision below
	// the normal refresh interval. New connections must fetch current authority
	// rather than receive a cached decision that the TLS/HTTP gate will reject.
	for _, decision := range a.decisions {
		if !now.Before(decision.ExpiresAt) {
			force = true
			break
		}
	}
	if force || a.fetched.IsZero() || now.Sub(a.fetched) >= connectorprotocol.IngressRefreshInterval {
		fetch, cancel := context.WithTimeout(ctx, 2*time.Second)
		decisions, denied, err := a.Client.IngressDecisions(fetch, a.NodeID, a.ProcessEpoch)
		cancel()
		if err != nil {
			a.decisions, a.quotaDenied = nil, nil
			a.fetched = time.Time{}
			return nil, err
		}
		a.decisions, a.fetched = decisions, now
		a.quotaDenied = make(map[string]bool, len(denied))
		for _, d := range denied {
			a.quotaDenied[d.Hostname] = true
		}
	}
	return append([]connectorprotocol.IngressDecision(nil), a.decisions...), nil
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
	a.mu.Lock()
	defer a.mu.Unlock()
	before := a.fetched
	decisions, err := a.snapshotLocked(ctx, false)
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
	if a.quotaDenied[hostname] {
		return connectorprotocol.IngressDecision{}, connectorprotocol.ErrIngressQuotaExhausted
	}
	// A snapshot read before origin readiness may legitimately omit a now-ready
	// local publication. Refresh that authority once; do not retry HTTP traffic.
	now := time.Now().UTC()
	for k, checked := range a.previewMisses {
		if now.Sub(checked) >= connectorprotocol.IngressRefreshInterval {
			delete(a.previewMisses, k)
		}
	}
	keyOpen := open
	keyOpen.RequestID = ""
	key := previewAuthorityKey{publication: publication, hostname: hostname, revision: revision, open: keyOpen}
	if _, checked := a.previewMisses[key]; checked {
		return connectorprotocol.IngressDecision{}, connectorprotocol.ErrIngressDenied
	}
	if len(a.previewMisses) >= maximumIngressDecisions {
		return connectorprotocol.IngressDecision{}, ErrControlUnavailable
	}
	if a.previewMisses == nil {
		a.previewMisses = make(map[previewAuthorityKey]time.Time)
	}
	a.previewMisses[key] = now
	if before == a.fetched {
		decisions, err = a.snapshotLocked(ctx, true)
		if err != nil {
			return connectorprotocol.IngressDecision{}, err
		}
		if d, ok := selectDecision(decisions); ok {
			delete(a.previewMisses, key)
			return d, nil
		}
		if a.quotaDenied[hostname] {
			return connectorprotocol.IngressDecision{}, connectorprotocol.ErrIngressQuotaExhausted
		}
	}
	return connectorprotocol.IngressDecision{}, connectorprotocol.ErrIngressDenied
}

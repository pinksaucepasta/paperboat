package edgehttp

import (
	"context"
	"errors"
	"github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
	"github.com/pinksaucepasta/paperboat/shared/nodemesh"
	runtimewire "github.com/pinksaucepasta/paperboat/shared/runtimeattachment"
	"strings"

	"github.com/pinksaucepasta/paperboat/edge/internal/route"
)

// CompositeRouteMatcher combines independent authoritative route owners. A
// request is accepted only when exactly one owner can acquire it; overlap is
// a fail-closed conflict rather than an order-dependent result.
type CompositeRouteMatcher struct {
	matchers []RouteMatcher
}

func NewCompositeRouteMatcher(matchers ...RouteMatcher) *CompositeRouteMatcher {
	filtered := make([]RouteMatcher, 0, len(matchers))
	for _, matcher := range matchers {
		if matcher != nil {
			filtered = append(filtered, matcher)
		}
	}
	return &CompositeRouteMatcher{matchers: filtered}
}

func (m *CompositeRouteMatcher) Match(host, requestPath string) (route.RouteMatch, error) {
	if m == nil {
		return route.RouteMatch{}, route.ErrNoMatch
	}
	var matched route.RouteMatch
	count := 0
	for _, matcher := range m.matchers {
		candidate, err := matcher.Match(host, requestPath)
		if err != nil {
			if errors.Is(err, route.ErrNoMatch) {
				continue
			}
			return route.RouteMatch{}, err
		}
		matched = candidate
		count++
	}
	if count == 0 {
		return route.RouteMatch{}, route.ErrNoMatch
	}
	if count != 1 {
		return route.RouteMatch{}, route.ErrMatchConflict
	}
	return matched, nil
}

func (m *CompositeRouteMatcher) Acquire(ctx context.Context, host, requestPath string) (*route.StreamLease, route.RouteMatch, error) {
	if m == nil {
		return nil, route.RouteMatch{}, route.ErrNoMatch
	}
	var leases []*route.StreamLease
	var matched route.RouteMatch
	for _, matcher := range m.matchers {
		candidateLease, candidate, err := matcher.Acquire(ctx, host, requestPath)
		if err != nil {
			if errors.Is(err, route.ErrNoMatch) {
				continue
			}
			for _, lease := range leases {
				_ = lease.Close()
			}
			return nil, route.RouteMatch{}, err
		}
		leases = append(leases, candidateLease)
		matched = candidate
	}
	if len(leases) == 0 {
		return nil, route.RouteMatch{}, route.ErrNoMatch
	}
	if len(leases) != 1 {
		for _, lease := range leases {
			_ = lease.Close()
		}
		return nil, route.RouteMatch{}, route.ErrMatchConflict
	}
	return leases[0], matched, nil
}

// PreviewCarrierRouteMatcher adapts the authenticated preview registry to the
// policy's generation-fenced request boundary. It is exact-host only because
// preview route ownership and expiry are server-issued admission facts.
type PreviewCarrierRouteMatcher struct {
	registry *DataCarrierPreviewRegistry
}

func NewPreviewCarrierRouteMatcher(registry *DataCarrierPreviewRegistry) *PreviewCarrierRouteMatcher {
	return &PreviewCarrierRouteMatcher{registry: registry}
}

func (m *PreviewCarrierRouteMatcher) Match(host, requestPath string) (route.RouteMatch, error) {
	preview, _, ok := m.lookup(host, requestPath)
	if !ok {
		return route.RouteMatch{}, route.ErrNoMatch
	}
	match := previewRouteMatch(preview)
	match.Path = requestPath
	return match, nil
}

func (m *PreviewCarrierRouteMatcher) Acquire(ctx context.Context, host, requestPath string) (*route.StreamLease, route.RouteMatch, error) {
	preview, entryDone, ok := m.lookup(host, requestPath)
	if !ok {
		return nil, route.RouteMatch{}, route.ErrNoMatch
	}
	if ctx == nil {
		ctx = context.Background()
	}
	streamContext, cancel := context.WithCancelCause(ctx)
	match := previewRouteMatch(preview)
	match.Path = requestPath
	lease := route.NewStreamLease(streamContext, match)
	go func() {
		select {
		case <-preview.Server.Done():
			cancel(route.ErrStreamLeaseEnded)
		case <-entryDone:
			cancel(route.ErrStreamLeaseEnded)
		case <-streamContext.Done():
		case <-lease.Done():
			cancel(route.ErrStreamLeaseEnded)
		}
	}()
	return lease, match, nil
}

func (m *PreviewCarrierRouteMatcher) lookup(host, requestPath string) (DataCarrierPreviewRoute, <-chan struct{}, bool) {
	if m == nil || m.registry == nil || !validPreviewMatcherPath(requestPath) {
		return DataCarrierPreviewRoute{}, nil, false
	}
	preview, entryDone, ok := m.registry.acquire(host)
	if !ok || preview.Server == nil || preview.Kind != dataCarrierPreviewRouteKind && preview.Kind != dataCarrierPreviewPrivateRouteKind && preview.Kind != runtimewire.RouteKind {
		return DataCarrierPreviewRoute{}, nil, false
	}
	select {
	case <-preview.Server.Done():
		return DataCarrierPreviewRoute{}, nil, false
	default:
		return preview, entryDone, true
	}
}

func previewRouteMatch(preview DataCarrierPreviewRoute) route.RouteMatch {
	generation := preview.AttachmentGeneration
	if generation == 0 {
		generation = preview.Identity.Generation
	}
	// An endpoint route has no lease on the edge (its admission requires generation 0); the server's
	// preview_routes view and hostd bind it as generation 1, so grants for it name generation 1.
	leaseGeneration := preview.LeaseGeneration
	if preview.EndpointRoute && leaseGeneration == 0 {
		leaseGeneration = 1
	}
	kind := route.PreviewHTTPSWSS
	if preview.Kind == dataCarrierPreviewPrivateRouteKind {
		kind = route.Kind(dataCarrierPreviewPrivateRouteKind)
	}
	rule := route.RouteRule{ID: preview.RouteID, RouteID: preview.RouteID, RouteGeneration: preview.Revision, Revision: preview.Revision, Generation: generation, AssignmentID: preview.OperationID, AssignmentGeneration: leaseGeneration, ResourceKind: "preview", SessionGeneration: leaseGeneration, Environment: preview.PreviewID, AccountID: preview.Identity.AccountID, HostID: preview.Identity.HostID, TunnelID: preview.PreviewID, ConnectorID: preview.Identity.ConnectorID, ConnectorSessionID: preview.Identity.SessionID, ConnectorProcessGeneration: preview.Identity.ProcessGeneration, ConfigGeneration: preview.Identity.Generation, Node: preview.EdgeNodeID, EdgeProcessEpoch: preview.EdgeProcessEpoch, Kind: kind, MatchType: route.MatchExact, Hostname: preview.Hostname, PathPrefix: "/", Target: preview.Endpoint, Protocol: "https", OriginScheme: "http", AccessMode: preview.AccessMode, PreserveHost: true, DesiredState: "active", ObservedState: "ready"}
	if preview.Kind == runtimewire.RouteKind {
		rule.Kind = route.Kind(runtimewire.RouteKind)
		rule.ResourceKind = "runtime"
		rule.TunnelID = preview.Identity.TunnelID
		rule.Target = ""
	}
	return route.RouteMatch{Rule: rule, Host: preview.Hostname, Path: "/", Generation: generation}
}

func validPreviewMatcherPath(requestPath string) bool {
	return requestPath != "" && strings.HasPrefix(requestPath, "/") && !strings.ContainsAny(requestPath, "\r\n\x00?#") && !strings.Contains(requestPath, "//")
}

// Held lists the hostnames the matchers that can say (the carrier-backed preview and runtime matchers) serve now.
func (m *CompositeRouteMatcher) Held() []string {
	if m == nil {
		return nil
	}
	var held []string
	for _, matcher := range m.matchers {
		if owner, ok := matcher.(interface{ Held() []string }); ok {
			held = append(held, owner.Held()...)
		}
	}
	return held
}

// Held lists the preview and runtime hostnames whose carrier is attached.
func (m *PreviewCarrierRouteMatcher) Held() []string {
	if m == nil {
		return nil
	}
	return m.registry.HeldHosts()
}

func (m *CompositeRouteMatcher) HasHTTPSHostname(host string) bool {
	if m == nil {
		return false
	}
	for _, matcher := range m.matchers {
		if owner, ok := matcher.(interface{ HasHTTPSHostname(string) bool }); ok && owner.HasHTTPSHostname(host) {
			return true
		}
	}
	return false
}
func (m *PreviewCarrierRouteMatcher) HasHTTPSHostname(host string) bool {
	_, _, ok := m.lookup(host, "/")
	return ok
}

// HasAdmittedHTTPSHostname identifies live server-admitted resources whose HTTP
// requests require independent browser grants. Expiry cannot revoke an admitted
// carrier; its owner must withdraw the route.
func (m *PreviewCarrierRouteMatcher) HasAdmittedHTTPSHostname(host string) bool {
	value, _, ok := m.lookup(host, "/")
	return ok && value.AttachmentGeneration != 0 &&
		(value.Kind == runtimewire.RouteKind || value.Kind == dataCarrierPreviewPrivateRouteKind)
}

// AuthorizePublicHTTPS refreshes only an exact live public preview admission.
// The caller must still inspect the full current ingress snapshot for conflicts.
func (m *PreviewCarrierRouteMatcher) AuthorizePublicHTTPS(ctx context.Context, host string, authorize func(context.Context, DataCarrierPreviewRoute) (connectorprotocol.IngressDecision, error)) error {
	admission, entryDone, ok := m.lookup(host, "/")
	if !ok || admission.Kind != dataCarrierPreviewRouteKind || admission.AccessMode != "public" || authorize == nil {
		return connectorprotocol.ErrIngressDenied
	}
	if _, err := authorize(ctx, admission); err != nil {
		return err
	}
	select {
	case <-entryDone:
		return connectorprotocol.ErrIngressDenied
	default:
	}
	current, currentDone, ok := m.lookup(host, "/")
	if !ok || currentDone != entryDone || !sameDataCarrierPreviewRoute(admission, current) {
		return connectorprotocol.ErrIngressDenied
	}
	return nil
}

// HasAdmittedHTTPSHostname grants TLS ownership only to a current restricted
// HTTP assignment backed by an eligible authenticated carrier. Public routes
// still require their current public ingress decision.
func (r *DataCarrierRouteRegistry) HasAdmittedHTTPSHostname(host string, assignments *route.Registry) bool {
	if r == nil {
		return false
	}
	rules := assignments.HTTPSRules(host)
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, rule := range rules {
		if rule.Kind != route.TunnelHTTPSWSS || rule.AccessMode != "private" && rule.AccessMode != "team" {
			continue
		}
		for _, entry := range r.byKey {
			if readyRouteReplica(entry, rule) {
				return true
			}
		}
	}
	return false
}

// HeldHosts lists the holder keys of the active HTTPS tunnel rules and TLS-passthrough tunnel rules that have a ready
// replica attached here: what this node serves that no preview registry knows. A rule is announced as its hostname,
// or as its one-label wildcard key; other patterns are left out.
func (r *DataCarrierRouteRegistry) HeldHosts(assignments *route.Registry) []string {
	if r == nil || assignments == nil {
		return nil
	}
	_, rules, ok := assignments.CanonicalSnapshot()
	if !ok {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	byRoute := make(map[string][]*dataCarrierRouteEntry)
	for _, entry := range r.byKey {
		for _, id := range entry.state.HealthyRoutes {
			byRoute[id] = append(byRoute[id], entry)
		}
	}
	var hosts []string
	for _, rule := range rules {
		if rule.Kind != route.TunnelHTTPSWSS && rule.Kind != route.TunnelTLS {
			continue
		}
		key, ok := heldKey(rule)
		if !ok {
			continue
		}
		for _, entry := range byRoute[carrierRouteID(rule)] {
			if readyRouteReplica(entry, rule) {
				hosts = append(hosts, key)
				break
			}
		}
	}
	return hosts
}

// heldKey is the holder key a rule is announced under: its lower-case hostname, or the wildcard key of a one-label wildcard
// route (WildcardSuffix holds the suffix without "*."). A wildcard suffix that is not a valid name yields no key.
func heldKey(rule route.RouteRule) (string, bool) {
	if rule.MatchType == route.MatchOneLabelWildcard {
		key := nodemesh.WildcardKey(strings.ToLower(rule.WildcardSuffix))
		return key, nodemesh.ValidKey(key) == nil
	}
	if rule.Hostname == "" || strings.ContainsAny(rule.Hostname, "*?") {
		return "", false
	}
	return strings.ToLower(rule.Hostname), true
}

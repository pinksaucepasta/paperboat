package edgehttp

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/edge/internal/route"
	"github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
)

var ErrPrivateConnectionInvalid = errors.New("invalid private access connection")

type privateConnectionEntry struct {
	ingress *connectorprotocol.IngressDecision
	request connectorprotocol.PrivateAccessRequest
	expires time.Time
	token   uint64
	// flight is the in-flight revalidation; retryAt is when a failed one may be tried again.
	flight  *privateRevalidation
	retryAt time.Time
}
type PrivateAccessConnectionRegistry struct {
	mu      sync.Mutex
	next    uint64
	maximum int
	entries map[string]privateConnectionEntry
	closed  bool
	now     func() time.Time
	// Revalidate asks the server for a current decision once the admitted one expired (set before
	// serving). A connection outlives its IngressAuthorityLifetime decision; without it an expired one is denied.
	Revalidate func(context.Context, connectorprotocol.IngressDecision) (connectorprotocol.IngressDecision, error)
}

func NewPrivateAccessConnectionRegistry(maximum int) (*PrivateAccessConnectionRegistry, error) {
	if maximum < 1 || maximum > 4096 {
		return nil, ErrPrivateConnectionInvalid
	}
	return &PrivateAccessConnectionRegistry{maximum: maximum, entries: make(map[string]privateConnectionEntry), now: func() time.Time { return time.Now().UTC() }}, nil
}
func (r *PrivateAccessConnectionRegistry) Register(address string, request connectorprotocol.PrivateAccessRequest, expires time.Time) (uint64, error) {
	if r == nil || net.ParseIP(hostOnly(address)) == nil || request.Validate(r.now()) != nil || !expires.Equal(request.ExpiresAt) || !expires.After(r.now()) {
		return 0, ErrPrivateConnectionInvalid
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_, replacing := r.entries[address]
	if r.closed || !replacing && len(r.entries) >= r.maximum {
		return 0, ErrPrivateConnectionInvalid
	}
	r.next++
	r.entries[address] = privateConnectionEntry{request: request, expires: expires, token: r.next}
	return r.next, nil
}
func (r *PrivateAccessConnectionRegistry) Remove(address string, token uint64) {
	if r == nil {
		return
	}
	r.mu.Lock()
	if v, ok := r.entries[address]; ok && v.token == token {
		delete(r.entries, address)
	}
	r.mu.Unlock()
}
func (r *PrivateAccessConnectionRegistry) Authorize(address string, match route.RouteMatch) (int, bool) {
	_, status, ok := r.authorizeRequest(address, match)
	return status, ok
}

type privateAccessRequestContextKey struct{}

func (r *PrivateAccessConnectionRegistry) authorizeRequest(address string, match route.RouteMatch) (connectorprotocol.PrivateAccessRequest, int, bool) {
	if r == nil {
		return connectorprotocol.PrivateAccessRequest{}, 503, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.entries[address]
	if !ok {
		return connectorprotocol.PrivateAccessRequest{}, 401, false
	}
	q := entry.request
	if !privateAccessRouteMatches(q, match) {
		return connectorprotocol.PrivateAccessRequest{}, 403, false
	}
	return q, 200, true
}

func privateAccessRouteMatches(q connectorprotocol.PrivateAccessRequest, match route.RouteMatch) bool {
	rule := match.Rule
	routeID, routeGeneration := rule.RouteID, rule.RouteGeneration
	if routeID == "" {
		routeID = rule.ID
	}
	if routeGeneration == 0 {
		routeGeneration = rule.Revision
	}
	// q.MachineID is the authenticated accessor machine. rule.HostID is the
	// connector host serving the route. Same-account private access explicitly
	// permits those to differ; the signed grant and accessor carrier identity
	// bind q.MachineID before this connection reaches private ingress.
	resourceBinding := q.ConnectorID == rule.ConnectorID
	if q.ResourceKind == "preview" {
		resourceBinding = q.OperationID == rule.AssignmentID
	}
	if !resourceBinding || q.Host != match.Host || q.ResourceKind != rule.ResourceKind || q.RouteID != routeID || q.RouteGeneration != routeGeneration || q.AccountID != rule.AccountID || q.ResourceID != rule.TunnelID && q.ResourceID != rule.Environment || q.CarrierSessionID != rule.ConnectorSessionID || q.ProcessGeneration != rule.ConnectorProcessGeneration || q.ConfigGeneration != rule.ConfigGeneration || q.SessionGeneration != rule.SessionGeneration || q.AssignmentGeneration != rule.AssignmentGeneration || q.EdgeNodeID != rule.Node || q.EdgeProcessEpoch != rule.EdgeProcessEpoch {
		return false
	}
	return true
}
func hostOnly(address string) string {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return ""
	}
	return host
}

type registeredPrivateConnection struct {
	net.Conn
	registry *PrivateAccessConnectionRegistry
	address  string
	token    uint64
	once     sync.Once
	closeErr error
}

func (c *registeredPrivateConnection) Close() error {
	c.once.Do(func() { c.registry.Remove(c.address, c.token); c.closeErr = c.Conn.Close() })
	return c.closeErr
}

// RegisterIngress stores only independently server-authorized native evidence.
func (r *PrivateAccessConnectionRegistry) RegisterIngress(address string, request connectorprotocol.PrivateAccessRequest, expires time.Time, decision connectorprotocol.IngressDecision) (uint64, error) {
	if decision.Validate(r.now()) != nil || decision.NativeAuthorization == nil || decision.NativeAuthorization.Request != request {
		return 0, ErrPrivateConnectionInvalid
	}
	token, err := r.Register(address, request, expires)
	if err != nil {
		return 0, err
	}
	evidence := *decision.NativeAuthorization
	decision.NativeAuthorization = &evidence
	r.mu.Lock()
	entry, ok := r.entries[address]
	if ok && entry.token == token {
		entry.ingress = &decision
		r.entries[address] = entry
	}
	r.mu.Unlock()
	if !ok {
		return 0, ErrPrivateConnectionInvalid
	}
	return token, nil
}

type privateIngressDecisionKey struct{}

// privateRevalidation is one in-flight server revalidation shared by every request waiting on it.
type privateRevalidation struct {
	done chan struct{}
	next connectorprotocol.IngressDecision
	ok   bool
}

const (
	// privateRevalidateBackoff is how long a transport failure denies without asking the server again.
	privateRevalidateBackoff = 2 * time.Second
	privateRevalidateTimeout = 10 * time.Second
)

func (r *PrivateAccessConnectionRegistry) ingress(ctx context.Context, address string, match route.RouteMatch) (connectorprotocol.IngressDecision, bool) {
	r.mu.Lock()
	entry, ok := r.entries[address]
	now := r.now()
	if !ok || entry.ingress == nil || !privateAccessRouteMatches(entry.request, match) || entry.ingress.ValidateAdmitted(now) != nil {
		r.mu.Unlock()
		return connectorprotocol.IngressDecision{}, false
	}
	current := *entry.ingress
	if current.Validate(now) == nil {
		r.mu.Unlock()
		return current, true
	}
	// An expired decision with no way to refresh it, or inside the backoff after a failed refresh, is denied.
	if r.Revalidate == nil || now.Before(entry.retryAt) {
		r.mu.Unlock()
		return connectorprotocol.IngressDecision{}, false
	}
	flight := entry.flight
	if flight == nil {
		flight = &privateRevalidation{done: make(chan struct{})}
		entry.flight = flight
		r.entries[address] = entry
		// Detached from this request: a client that gives up must not fail the requests waiting with it.
		go r.revalidate(context.WithoutCancel(ctx), address, entry, current, flight)
	}
	r.mu.Unlock()
	select {
	case <-flight.done:
		return flight.next, flight.ok
	case <-ctx.Done():
		return connectorprotocol.IngressDecision{}, false
	}
}

// revalidate asks the server for a current decision for the same grant and request, as it does for each
// preview request. The answer serves this connection's next requests until it expires too, so revocation
// lands within one decision lifetime. One call is in flight per connection; a transport failure is
// not retried for privateRevalidateBackoff and an explicit deny holds for the rest of the connection.
func (r *PrivateAccessConnectionRegistry) revalidate(ctx context.Context, address string, entry privateConnectionEntry, current connectorprotocol.IngressDecision, flight *privateRevalidation) {
	ctx, cancel := context.WithTimeout(ctx, privateRevalidateTimeout)
	defer cancel()
	next, err := r.Revalidate(ctx, current)
	ok := err == nil && next.Validate(r.now()) == nil && next.NativeAuthorization != nil && next.NativeAuthorization.Request == entry.request
	if ok {
		evidence := *next.NativeAuthorization
		next.NativeAuthorization = &evidence
		flight.next = next
	}
	flight.ok = ok
	r.mu.Lock()
	if stored, found := r.entries[address]; found && stored.token == entry.token {
		stored.flight = nil
		switch {
		case ok:
			stored.ingress = &next
		case err == nil || errors.Is(err, connectorprotocol.ErrIngressDenied):
			stored.retryAt = stored.expires
		default:
			stored.retryAt = r.now().Add(privateRevalidateBackoff)
		}
		r.entries[address] = stored
	}
	r.mu.Unlock()
	close(flight.done)
}

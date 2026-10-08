package edgehttp

import (
	"errors"
	"net"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat-tunnel/internal/connectorprotocol"
	"github.com/pinksaucepasta/paperboat-tunnel/internal/route"
)

var ErrPrivateConnectionInvalid = errors.New("invalid private access connection")

type privateConnectionEntry struct {
	ingress *connectorprotocol.IngressDecision
	request connectorprotocol.PrivateAccessRequest
	expires time.Time
	token   uint64
}
type PrivateAccessConnectionRegistry struct {
	mu      sync.Mutex
	next    uint64
	maximum int
	entries map[string]privateConnectionEntry
	closed  bool
	now     func() time.Time
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
	if !entry.expires.After(r.now()) {
		delete(r.entries, address)
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

func (r *PrivateAccessConnectionRegistry) ingress(address string, match route.RouteMatch) (connectorprotocol.IngressDecision, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.entries[address]
	if !ok || entry.ingress == nil || !entry.expires.After(r.now()) || !privateAccessRouteMatches(entry.request, match) || entry.ingress.Validate(entry.ingress.IssuedAt) != nil {
		return connectorprotocol.IngressDecision{}, false
	}
	return *entry.ingress, true
}

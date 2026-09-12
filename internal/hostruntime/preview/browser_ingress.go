package preview

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/connectorprotocol"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/connector"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hoststate"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/tunnelmanager"
	"github.com/pinksaucepasta/paperboat/internal/inspector"
)

// previewCaptureIdentity returns the product inspector identity for a lease:
// owner-addressed by lease ID (the carrier route is an opaque server hash
// owners never see) with the stable attachment route generation as the
// uniform triple. The attachment route generation — not the lease generation,
// which advances on heartbeats — is what the server returns as current
// authority, so records stay current across renewals and go stale exactly
// when the attachment is replaced. Without attachment context (direct test
// carriers), the lease generation is the honest fallback.
func previewCaptureIdentity(lease Lease, routeGeneration uint64) *tunnelmanager.CaptureIdentity {
	if strings.TrimSpace(lease.ID) == "" || lease.LeaseDeadline.IsZero() {
		return nil
	}
	generation := routeGeneration
	if generation == 0 {
		if lease.Generation < 1 {
			return nil
		}
		generation = uint64(lease.Generation)
	}
	return &tunnelmanager.CaptureIdentity{
		ResourceID:         lease.ID,
		ResourceGeneration: generation,
		RouteGeneration:    generation,
		TargetGeneration:   generation,
		ExpiresAt:          lease.LeaseDeadline,
	}
}

// normalizePreviewTarget keeps the loopback origin in its canonical dial form.
func normalizePreviewTarget(target LeaseTarget) LeaseTarget {
	if host, port, err := net.SplitHostPort(target.Address); err == nil && strings.EqualFold(host, "localhost") {
		target.Address = net.JoinHostPort("127.0.0.1", port)
	}
	return target
}

func isPreviewHTTPScheme(scheme string) bool {
	switch strings.ToLower(strings.TrimSpace(scheme)) {
	case "http", "https", "h2c":
		return true
	default:
		return false
	}
}

// peekHTTPExchange reports whether the stream starts with an HTTP request
// line without consuming bytes, so non-HTTP traffic keeps the exact raw path.
func peekHTTPExchange(reader *bufio.Reader) bool {
	if reader == nil {
		return false
	}
	head, err := reader.Peek(8)
	if err != nil {
		return false
	}
	for _, method := range []string{"GET ", "POST ", "PUT ", "DELETE ", "PATCH ", "HEAD ", "OPTIONS "} {
		if strings.HasPrefix(string(head), method) {
			return true
		}
	}
	return false
}

func (c *DataCarrierPreviewCarrier) forwardBrowser(ctx context.Context, stream *connector.DataCarrierStream, open connectorprotocol.StreamOpen, lease Lease) error {
	defer stream.Close()
	if c.browserIngress == nil || c.browserRouteGeneration == 0 || lease.AccessMode == "public" {
		return connectorprotocol.ErrIngressDenied
	}
	endpoint, err := url.Parse(lease.Endpoint)
	if err != nil || endpoint.Scheme != "https" || endpoint.Hostname() == "" {
		return connectorprotocol.ErrIngressDenied
	}
	target := normalizePreviewTarget(lease.Target)
	verification := "not_applicable"
	if target.Scheme == "https" {
		verification = "system"
	}
	dialWait := c.dialWait
	if lease.LazyLifecycle != nil && dialWait > LazyOriginConnectTimeout {
		dialWait = LazyOriginConnectTimeout
	}
	route := c.previewHTTPRoute(open.RouteID, target, verification, dialWait)
	authority := func(ctx context.Context, open connectorprotocol.StreamOpen, claimed connectorprotocol.IngressDecision) (connectorprotocol.IngressDecision, error) {
		current, err := c.browserIngress(ctx, open, claimed)
		b := current.Binding
		if err != nil || b.Lifecycle != connectorprotocol.TunnelEphemeral || b.AccountID != lease.AccountID || b.HostID != c.identity.HostID || b.ResourceGeneration != c.browserRouteGeneration || b.RouteGeneration != c.browserRouteGeneration || b.PublicationID != lease.ID || b.PublicationGeneration != b.ResourceGeneration || b.TargetID != open.RouteID || b.TargetGeneration != b.ResourceGeneration || b.Hostname != strings.ToLower(endpoint.Hostname()) || b.PathPrefix != "/" || b.Audience != lease.AccessMode || b.OriginScheme != target.Scheme || b.OriginAddress != target.Address || b.TLSVerification != verification || b.TLSServerName != "" || b.CAReference != "" || b.MTLSCredentialReference != "" {
			return connectorprotocol.IngressDecision{}, connectorprotocol.ErrIngressDenied
		}
		return current, nil
	}
	transport := c.previewHTTPTransport(target)
	defer transport.CloseIdleConnections()
	c.registerReplay(route, target, previewCaptureIdentity(lease, c.browserRouteGeneration))
	// Capture runs through the shared inspector choke point under the
	// lease-addressed product identity. Registry stays nil here: the carrier
	// registers replay with a per-invocation transport,
	// since this stream's transport closes when the stream ends.
	forwarder := tunnelmanager.OriginStreamForwarder{Transport: transport, IngressAuthority: authority, Inspector: c.inspector, CaptureIdentity: previewCaptureIdentity(lease, c.browserRouteGeneration)}
	return forwarder.ForwardIngressHTTP(ctx, stream, open, route)
}

// previewHTTPRoute builds the origin route for a lease target. One helper
// serves browser, public and replay registration so destination and TLS
// policy cannot drift between them.
func (c *DataCarrierPreviewCarrier) previewHTTPRoute(routeID string, target LeaseTarget, verification string, dialWait time.Duration) hoststate.TunnelConfigRoute {
	maxStreams := 0
	if c != nil {
		maxStreams = c.max
	}
	return hoststate.TunnelConfigRoute{ID: routeID, Protocol: "http", OriginScheme: target.Scheme, OriginAddress: target.Address, TLSVerification: verification, PreserveHost: true, ConnectTimeoutMs: int32(dialWait / time.Millisecond), IdleTimeoutMs: 90000, MaxConcurrentStreams: int32(maxStreams), DesiredState: "active"}
}

// previewHTTPTransport returns a fresh origin transport for a lease target.
// Each replay invocation uses its own transport because per-stream transports
// close with their stream; the caller owns idle-connection cleanup.
func (c *DataCarrierPreviewCarrier) previewHTTPTransport(target LeaseTarget) *tunnelmanager.OriginHTTPTransport {
	transport := &tunnelmanager.OriginHTTPTransport{}
	if c != nil && c.dialer != nil {
		transport.Dialer = previewBrowserDialer{carrier: c, target: target}
	}
	return transport
}

// registerReplay publishes the current target-bound replay binding for a
// lease. Destination and TLS policy come from the bound route and cannot be
// overridden per request.
func (c *DataCarrierPreviewCarrier) registerReplay(route hoststate.TunnelConfigRoute, target LeaseTarget, identity *tunnelmanager.CaptureIdentity) {
	if c == nil || c.registry == nil || identity == nil {
		return
	}
	bound := route
	forward := func(forwardCtx context.Context, method, requestURI string, header http.Header, body []byte) (int, http.Header, []byte, bool, error) {
		replayTransport := c.previewHTTPTransport(target)
		defer replayTransport.CloseIdleConnections()
		return tunnelmanager.ReplayViaTransport(forwardCtx, replayTransport, bound, method, requestURI, header, body)
	}
	// Best-effort: registration failure only disables replay, never traffic.
	_ = c.registry.Register(identity.ResourceID, inspector.ReplayBinding{
		ResourceGeneration: identity.ResourceGeneration,
		RouteGeneration:    identity.RouteGeneration,
		TargetGeneration:   identity.TargetGeneration,
		ExpiresAt:          identity.ExpiresAt,
		Available:          func() bool { return !c.isClosed() },
		Forward:            forward,
	})
}

type previewBrowserDialer struct {
	carrier *DataCarrierPreviewCarrier
	target  LeaseTarget
}

func (d previewBrowserDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	target := d.target
	// OriginHTTPTransport owns TLS verification; the injected dialer supplies
	// the socket only, matching its ordinary DialContext contract.
	if target.Scheme == "https" {
		target.Scheme = "http"
	}
	connection, err := d.carrier.dialer(ctx, target)
	if err != nil {
		return nil, err
	}
	if socket, ok := connection.(net.Conn); ok {
		return socket, nil
	}
	if connection != nil {
		_ = connection.Close()
	}
	return nil, ErrDataCarrierPreviewOrigin
}

package preview

import (
	"context"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/connectorprotocol"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/connector"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hoststate"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/tunnelmanager"
)

func (c *DataCarrierPreviewCarrier) forwardBrowser(ctx context.Context, stream *connector.DataCarrierStream, open connectorprotocol.StreamOpen, lease Lease) error {
	defer stream.Close()
	if c.browserIngress == nil || c.browserRouteGeneration == 0 || lease.AccessMode == "public" {
		return connectorprotocol.ErrIngressDenied
	}
	endpoint, err := url.Parse(lease.Endpoint)
	if err != nil || endpoint.Scheme != "https" || endpoint.Hostname() == "" {
		return connectorprotocol.ErrIngressDenied
	}
	target := lease.Target
	if host, port, err := net.SplitHostPort(target.Address); err == nil && strings.EqualFold(host, "localhost") {
		target.Address = net.JoinHostPort("127.0.0.1", port)
	}
	verification := "not_applicable"
	if target.Scheme == "https" {
		verification = "system"
	}
	dialWait := c.dialWait
	if lease.LazyLifecycle != nil && dialWait > LazyOriginConnectTimeout {
		dialWait = LazyOriginConnectTimeout
	}
	route := hoststate.TunnelConfigRoute{ID: open.RouteID, Protocol: "http", OriginScheme: target.Scheme, OriginAddress: target.Address, TLSVerification: verification, PreserveHost: true, ConnectTimeoutMs: int32(dialWait / time.Millisecond), IdleTimeoutMs: 90000, MaxConcurrentStreams: int32(c.max), DesiredState: "active"}
	authority := func(ctx context.Context, open connectorprotocol.StreamOpen, claimed connectorprotocol.IngressDecision) (connectorprotocol.IngressDecision, error) {
		current, err := c.browserIngress(ctx, open, claimed)
		b := current.Binding
		if err != nil || b.Lifecycle != connectorprotocol.TunnelEphemeral || b.AccountID != lease.AccountID || b.HostID != c.identity.HostID || b.ResourceGeneration != c.browserRouteGeneration || b.RouteGeneration != c.browserRouteGeneration || b.PublicationID != lease.ID || b.PublicationGeneration != b.ResourceGeneration || b.TargetID != open.RouteID || b.TargetGeneration != b.ResourceGeneration || b.Hostname != strings.ToLower(endpoint.Hostname()) || b.PathPrefix != "/" || b.Audience != lease.AccessMode || b.OriginScheme != target.Scheme || b.OriginAddress != target.Address || b.TLSVerification != verification || b.TLSServerName != "" || b.CAReference != "" || b.MTLSCredentialReference != "" {
			return connectorprotocol.IngressDecision{}, connectorprotocol.ErrIngressDenied
		}
		return current, nil
	}
	transport := &tunnelmanager.OriginHTTPTransport{}
	if c.dialer != nil {
		transport.Dialer = previewBrowserDialer{carrier: c, target: target}
	}
	defer transport.CloseIdleConnections()
	forwarder := tunnelmanager.OriginStreamForwarder{Transport: transport, IngressAuthority: authority}
	return forwarder.ForwardIngressHTTP(ctx, stream, open, route)
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

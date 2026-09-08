package tunnelmanager

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/connectorprotocol"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/connector"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hoststate"
)

type ingressBindingKey struct{}

func (f OriginStreamForwarder) admitIngress(parent context.Context, stream io.ReadWriteCloser, open connectorprotocol.StreamOpen, route hoststate.TunnelConfigRoute) (context.Context, context.CancelFunc, error) {
	ctx, cancel := context.WithCancel(parent)
	fail := func(err error) (context.Context, context.CancelFunc, error) { cancel(); return nil, nil, err }
	// A peer cannot hold origin admission indefinitely with a partial preface.
	stopRead := time.AfterFunc(10*time.Second, func() { _ = stream.Close() })
	decision, err := connectorprotocol.ReadIngressDecision(stream, time.Now().UTC())
	stopRead.Stop()
	if err != nil {
		return fail(err)
	}
	if open.Kind == "http_browser" && decision.Binding.Audience == "public" {
		return fail(connectorprotocol.ErrIngressDenied)
	}
	if carrierStream, ok := stream.(*connector.DataCarrierStream); ok {
		target := carrierStream.EdgeTarget()
		if target.EdgeID != decision.EdgeNodeID || target.ProcessEpoch != decision.EdgeProcessEpoch {
			return fail(connectorprotocol.ErrIngressDenied)
		}
	}
	lookup, stopLookup := context.WithDeadline(ctx, decision.ExpiresAt)
	current, err := f.IngressAuthority(lookup, open, decision)
	stopLookup()
	if err != nil || decision.Authorize(current, open, current.EdgeNodeID, current.EdgeProcessEpoch, time.Now().UTC()) != nil || !ingressRouteMatches(current.Binding, route) {
		return fail(connectorprotocol.ErrIngressDenied)
	}
	ctx = context.WithValue(ctx, ingressBindingKey{}, current.Binding)
	expire := time.AfterFunc(time.Until(current.ExpiresAt), func() { cancel(); _ = stream.Close() })
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer expire.Stop()
		tick := time.NewTicker(connectorprotocol.IngressRefreshInterval)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				refresh, stop := context.WithDeadline(ctx, current.ExpiresAt)
				next, err := f.IngressAuthority(refresh, open, decision)
				stop()
				prior := current
				// Refresh grants time only; every independent authority dimension
				// must remain identical for an existing application stream.
				prior.IssuedAt, prior.ExpiresAt = next.IssuedAt, next.ExpiresAt
				if err != nil || prior.Authorize(next, open, next.EdgeNodeID, next.EdgeProcessEpoch, time.Now().UTC()) != nil {
					cancel()
					_ = stream.Close()
					return
				}
				if ctx.Err() != nil {
					return
				}
				current = next
				expire.Reset(time.Until(next.ExpiresAt))
			}
		}
	}()
	return ctx, func() { cancel(); <-done }, nil
}

func ingressRouteMatches(b connectorprotocol.IngressBinding, r hoststate.TunnelConfigRoute) bool {
	value := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	origin := r.OriginAddress
	if host, port, err := net.SplitHostPort(origin); err == nil && strings.EqualFold(host, "localhost") {
		origin = net.JoinHostPort("127.0.0.1", port)
	}
	return b.RouteID == r.ID && b.Protocol == r.Protocol && b.OriginScheme == r.OriginScheme && b.OriginAddress == origin && b.TLSVerification == r.TLSVerification && b.TLSServerName == value(r.TLSServerName) && b.CAReference == value(r.CAReference) && b.MTLSCredentialReference == value(r.MTLSCredentialReference)
}

func validateIngressHTTPRequest(ctx context.Context, r *http.Request) error {
	b, ok := ctx.Value(ingressBindingKey{}).(connectorprotocol.IngressBinding)
	if !ok {
		return nil
	}
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if !strings.EqualFold(host, b.Hostname) || r.URL == nil || !strings.HasPrefix(r.URL.Path, b.PathPrefix) || strings.ContainsAny(r.URL.Path, "\\\x00") {
		return connectorprotocol.ErrIngressDenied
	}
	if b.PathPrefix != "/" && !strings.HasSuffix(b.PathPrefix, "/") && r.URL.Path != b.PathPrefix && !strings.HasPrefix(r.URL.Path, b.PathPrefix+"/") {
		return connectorprotocol.ErrIngressDenied
	}
	escaped := strings.ToLower(r.URL.EscapedPath())
	if strings.Contains(escaped, "%2f") || strings.Contains(escaped, "%5c") || strings.Contains(r.URL.Path, "/../") || strings.HasSuffix(r.URL.Path, "/..") {
		return connectorprotocol.ErrIngressDenied
	}
	return nil
}

func (f OriginStreamForwarder) serveTCP(ctx context.Context, stream io.ReadWriteCloser, route hoststate.TunnelConfigRoute) error {
	if f.IngressAuthority == nil || route.Protocol != "tcp" || route.OriginScheme != "tcp" {
		return connectorprotocol.ErrIngressDenied
	}
	dialer := f.Transport.Dialer
	if dialer == nil {
		dialer = &net.Dialer{Timeout: time.Duration(route.ConnectTimeoutMs) * time.Millisecond}
	}
	connection, err := dialer.DialContext(ctx, "tcp", route.OriginAddress)
	if err != nil {
		return ErrOriginUnavailable
	}
	defer connection.Close()
	half, ok := connection.(interface{ CloseWrite() error })
	if !ok {
		return ErrInvalidConfig
	}
	halfStream, ok := stream.(interface{ CloseWrite() error })
	if !ok {
		return ErrInvalidConfig
	}
	stop := context.AfterFunc(ctx, func() { _ = connection.Close(); _ = stream.Close() })
	defer stop()
	results := make(chan error, 2)
	go func() {
		_, err := io.Copy(connection, stream)
		if err == nil {
			err = half.CloseWrite()
		}
		results <- err
	}()
	go func() {
		_, err := io.Copy(stream, connection)
		if err == nil {
			err = halfStream.CloseWrite()
		}
		results <- err
	}()
	first := <-results
	if first != nil {
		_ = connection.Close()
		_ = stream.Close()
	}
	return errors.Join(first, <-results)
}

// ForwardIngressHTTP serves exactly one HTTP request over an independently
// authorized connector stream. The caller owns route concurrency and transport
// lifetime; cancellation closes the stream and any active response or upgrade.
func (f OriginStreamForwarder) ForwardIngressHTTP(ctx context.Context, stream io.ReadWriteCloser, open connectorprotocol.StreamOpen, route hoststate.TunnelConfigRoute) error {
	if f.Transport == nil || f.IngressAuthority == nil || stream == nil || ctx == nil || route.Protocol != "http" || open.Kind != "http_browser" {
		return connectorprotocol.ErrIngressDenied
	}
	defer stream.Close()
	stop := context.AfterFunc(ctx, func() { _ = stream.Close() })
	defer stop()
	authorized, release, err := f.admitIngress(ctx, stream, open, route)
	if err != nil {
		return err
	}
	defer release()
	return f.serveHTTP(authorized, idleOriginStream{ReadWriteCloser: stream, idle: time.Duration(route.IdleTimeoutMs) * time.Millisecond}, route, f.Transport)
}

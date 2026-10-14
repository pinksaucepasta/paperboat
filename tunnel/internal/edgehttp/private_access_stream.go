package edgehttp

import (
	"context"
	"errors"
	yamux "github.com/libp2p/go-yamux/v5"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat-tunnel/internal/connectorprotocol"
	"github.com/pinksaucepasta/paperboat-tunnel/internal/control"
	"github.com/pinksaucepasta/paperboat-tunnel/internal/datacarrier"
	"github.com/pinksaucepasta/paperboat-tunnel/internal/route"
)

var ErrPrivateAccessStreamInvalid = errors.New("invalid private access stream bridge")

type PrivateAccessTarget interface {
	OpenPrivateAccessTarget(context.Context, connectorprotocol.PrivateAccessRequest) (io.ReadWriteCloser, error)
}

type privateAccessTargetWithLifetime interface {
	OpenPrivateAccessTargetWithLifetime(context.Context, context.Context, connectorprotocol.PrivateAccessRequest) (io.ReadWriteCloser, error)
}

type PrivateAccessTargetFunc func(context.Context, connectorprotocol.PrivateAccessRequest) (io.ReadWriteCloser, error)

func (f PrivateAccessTargetFunc) OpenPrivateAccessTarget(ctx context.Context, request connectorprotocol.PrivateAccessRequest) (io.ReadWriteCloser, error) {
	if f == nil {
		return nil, ErrPrivateAccessStreamInvalid
	}
	return f(ctx, request)
}

type PrivateAccessStreamBridgeConfig struct {
	OnFailure        func(context.Context, string, error)
	Authorizer       control.PrivateAccessGrantAuthorizer
	Target           PrivateAccessTarget
	MaximumStreams   int
	AuthorizeTimeout time.Duration
	OpenTimeout      time.Duration
	Clock            func() time.Time
}

type PrivateAccessStreamBridge struct {
	onFailure        func(context.Context, string, error)
	authorizer       control.PrivateAccessGrantAuthorizer
	target           PrivateAccessTarget
	maximum          int
	authorizeTimeout time.Duration
	openTimeout      time.Duration
	clock            func() time.Time
}

func NewPrivateAccessStreamBridge(config PrivateAccessStreamBridgeConfig) (*PrivateAccessStreamBridge, error) {
	if config.Authorizer == nil || config.Target == nil {
		return nil, ErrPrivateAccessStreamInvalid
	}
	if config.MaximumStreams == 0 {
		config.MaximumStreams = 256
	}
	if config.AuthorizeTimeout == 0 {
		config.AuthorizeTimeout = 10 * time.Second
	}
	if config.OpenTimeout == 0 {
		config.OpenTimeout = 10 * time.Second
	}
	if config.MaximumStreams < 1 || config.MaximumStreams > 4096 || config.AuthorizeTimeout <= 0 || config.AuthorizeTimeout > time.Minute || config.OpenTimeout <= 0 || config.OpenTimeout > time.Minute {
		return nil, ErrPrivateAccessStreamInvalid
	}
	if config.Clock == nil {
		config.Clock = func() time.Time { return time.Now().UTC() }
	}
	return &PrivateAccessStreamBridge{onFailure: config.OnFailure, authorizer: config.Authorizer, target: config.Target, maximum: config.MaximumStreams, authorizeTimeout: config.AuthorizeTimeout, openTimeout: config.OpenTimeout, clock: config.Clock}, nil
}

func (b *PrivateAccessStreamBridge) Serve(ctx context.Context, server *datacarrier.Server) error {
	if b == nil || ctx == nil || server == nil {
		return ErrPrivateAccessStreamInvalid
	}
	ctx, cancel := context.WithCancel(ctx)
	permits := make(chan struct{}, b.maximum)
	var streams sync.WaitGroup
	defer func() { cancel(); streams.Wait() }()
	for {
		select {
		case permits <- struct{}{}:
		case <-ctx.Done():
			return ctx.Err()
		case <-server.Done():
			return nil
		}
		stream, metadata, err := server.AcceptAccessStream(ctx)
		if err != nil {
			<-permits
			if requestCanceled(ctx, err) {
				return errors.Join(ctx.Err(), err)
			}
			select {
			case <-server.Done():
				if requestErrorLeaves(err, func(leaf error) bool { return leaf == datacarrier.ErrCarrierClosed || leaf == net.ErrClosed }, true) {
					return nil
				}
				return err
			default:
				return err
			}
		}
		streams.Add(1)
		identity := server.Identity()
		go func() {
			defer streams.Done()
			defer func() { <-permits }()
			b.serveStream(ctx, stream, metadata, identity)
		}()
	}
}

func (b *PrivateAccessStreamBridge) serveStream(parent context.Context, stream *datacarrier.Stream, metadata connectorprotocol.StreamOpen, identity datacarrier.Identity) (result error) {
	if stream == nil {
		return ErrPrivateAccessStreamInvalid
	}
	closeDone := make(chan struct{})
	stopClose := context.AfterFunc(parent, func() { defer close(closeDone); _ = stream.Close() })
	defer func() {
		if !stopClose() {
			<-closeDone
		}
	}()
	phase := "private_stream_open"
	observeCtx := parent
	streamClosed := false
	defer func() {
		if !streamClosed {
			if closeErr := stream.Close(); closeErr != nil {
				result = errors.Join(result, closeErr)
			}
		}
		b.observe(observeCtx, phase, result)
	}()
	now := b.clock().UTC()
	open, err := connectorprotocol.ReadPrivateAccessOpen(stream, now)
	if err != nil || !privateAccessMetadataMatches(metadata, open.Request, identity) {
		writeErr := connectorprotocol.WritePrivateAccessResult(stream, connectorprotocol.PrivateAccessResult{Schema: connectorprotocol.PrivateAccessSchema, Kind: connectorprotocol.PrivateAccessKind, Status: http.StatusUnauthorized})
		return errors.Join(err, writeErr)
	}
	authorizeCtx, cancelAuthorize := context.WithTimeout(parent, b.authorizeTimeout)
	decision, err := b.authorizer.AuthorizePrivateAccessGrant(authorizeCtx, open.Grant, open.Request)
	cancelAuthorize()
	if err != nil {
		phase = "private_stream_authorize"
		writeErr := connectorprotocol.WritePrivateAccessResult(stream, connectorprotocol.PrivateAccessResult{Schema: connectorprotocol.PrivateAccessSchema, Kind: connectorprotocol.PrivateAccessKind, Status: privateAccessErrorStatus(err)})
		if writeErr == nil {
			return err
		}
		return errors.Join(err, writeErr)
	}
	if !decision.Allowed || !decision.ExpiresAt.After(b.clock().UTC()) {
		return connectorprotocol.WritePrivateAccessResult(stream, connectorprotocol.PrivateAccessResult{Schema: connectorprotocol.PrivateAccessSchema, Kind: connectorprotocol.PrivateAccessKind, Status: http.StatusForbidden})
	}
	lifetime, cancelLifetime := context.WithDeadline(parent, decision.ExpiresAt)
	lifetime = context.WithValue(lifetime, privateIngressDecisionKey{}, decision.Ingress)
	observeCtx = lifetime
	defer cancelLifetime()
	openCtx, cancelOpen := context.WithTimeout(lifetime, b.openTimeout)
	var target io.ReadWriteCloser
	if lifetimeTarget, ok := b.target.(privateAccessTargetWithLifetime); ok {
		target, err = lifetimeTarget.OpenPrivateAccessTargetWithLifetime(openCtx, lifetime, open.Request)
	} else {
		target, err = b.target.OpenPrivateAccessTarget(openCtx, open.Request)
	}
	cancelOpen()
	phase = "private_stream_target"
	if err != nil || target == nil {
		if err == nil {
			err = ErrPrivateAccessStreamInvalid
		}
		if target != nil {
			err = errors.Join(err, target.Close())
		}
		return errors.Join(err, connectorprotocol.WritePrivateAccessResult(stream, connectorprotocol.PrivateAccessResult{Schema: connectorprotocol.PrivateAccessSchema, Kind: connectorprotocol.PrivateAccessKind, Status: http.StatusServiceUnavailable}))
	}
	targetClosed := false
	defer func() {
		if !targetClosed {
			if closeErr := target.Close(); closeErr != nil {
				result = errors.Join(result, closeErr)
			}
		}
	}()
	phase = "private_stream_result"
	if err := connectorprotocol.WritePrivateAccessResult(stream, connectorprotocol.PrivateAccessResult{Schema: connectorprotocol.PrivateAccessSchema, Kind: connectorprotocol.PrivateAccessKind, Status: http.StatusOK, ExpiresAt: decision.ExpiresAt}); err != nil {
		return err
	}
	phase = "private_stream_copy"
	copyDone := make(chan error, 2)
	go func() { _, err := io.Copy(target, stream); copyDone <- err }()
	go func() { _, err := io.Copy(stream, target); copyDone <- err }()
	var first error
	completed := 0
	select {
	case first = <-copyDone:
		completed++
	case <-lifetime.Done():
		first = lifetime.Err()
	}
	cancelLifetime()
	closeErr := errors.Join(target.Close(), stream.Close())
	targetClosed = true
	streamClosed = true
	result = errors.Join(first, closeErr)
	for completed < 2 {
		result = errors.Join(result, <-copyDone)
		completed++
	}
	return result
}

func privateAccessMetadataMatches(metadata connectorprotocol.StreamOpen, request connectorprotocol.PrivateAccessRequest, identity datacarrier.Identity) bool {
	wantKind := connectorprotocol.PrivateAccessHTTP
	if request.Protocol == "tcp" {
		wantKind = connectorprotocol.PrivateAccessTCP
	}
	return identity.AccountID == request.AccountID && identity.HostID == request.MachineID && identity.TunnelID == metadata.TunnelID && identity.ConnectorID == metadata.ConnectorID && identity.SessionID == metadata.SessionID && identity.ProcessGeneration == metadata.ProcessGeneration && identity.Generation == metadata.Generation && metadata.Kind == wantKind && metadata.AccountID == request.AccountID && metadata.RouteID == request.RouteID && metadata.SessionID == request.CarrierSessionID && metadata.ProcessGeneration == request.ProcessGeneration && metadata.Generation == request.ConfigGeneration && metadata.RequestID == request.RequestID
}

func privateAccessErrorStatus(err error) int {
	switch {
	case errors.Is(err, control.ErrPrivateAccessGrantUnauthorized):
		return http.StatusUnauthorized
	case errors.Is(err, control.ErrPrivateAccessGrantForbidden):
		return http.StatusForbidden
	default:
		return http.StatusServiceUnavailable
	}
}

type PrivateAccessHTTPTarget struct {
	Address     string
	Dialer      *net.Dialer
	Connections *PrivateAccessConnectionRegistry
}

func (t PrivateAccessHTTPTarget) OpenPrivateAccessTarget(ctx context.Context, request connectorprotocol.PrivateAccessRequest) (io.ReadWriteCloser, error) {
	if ctx == nil || request.Protocol != "http" {
		return nil, ErrPrivateAccessStreamInvalid
	}
	host, port, err := net.SplitHostPort(t.Address)
	ip := net.ParseIP(host)
	if err != nil || ip == nil || !ip.IsLoopback() || port == "" || port == "0" {
		return nil, ErrPrivateAccessStreamInvalid
	}
	dialer := t.Dialer
	if dialer == nil {
		dialer = &net.Dialer{}
	}
	connection, err := dialer.DialContext(ctx, "tcp", t.Address)
	if err != nil {
		return nil, err
	}
	if t.Connections == nil {
		_ = connection.Close()
		return nil, ErrPrivateAccessStreamInvalid
	}
	var token uint64
	if request.ResourceKind == "tunnel" || request.ResourceKind == "preview" {
		d, ok := ctx.Value(privateIngressDecisionKey{}).(*connectorprotocol.IngressDecision)
		if !ok || d == nil {
			_ = connection.Close()
			return nil, ErrPrivateAccessStreamInvalid
		}
		token, err = t.Connections.RegisterIngress(connection.LocalAddr().String(), request, request.ExpiresAt, *d)
	} else {
		token, err = t.Connections.Register(connection.LocalAddr().String(), request, request.ExpiresAt)
	}
	if err != nil {
		_ = connection.Close()
		return nil, err
	}
	return &registeredPrivateConnection{Conn: connection, registry: t.Connections, address: connection.LocalAddr().String(), token: token}, nil
}

var _ PrivateAccessTarget = PrivateAccessHTTPTarget{}

// PrivateAccessRouteTarget keeps HTTP on the private TLS listener and sends raw
// TCP only to the exact authenticated durable private_tcp carrier route.
type PrivateAccessRouteTarget struct {
	HTTP   PrivateAccessTarget
	Routes interface {
		CanonicalSnapshot() (uint64, []route.RouteRule, bool)
	}
	Carriers *DataCarrierRouteRegistry
}

func (t PrivateAccessRouteTarget) OpenPrivateAccessTarget(ctx context.Context, request connectorprotocol.PrivateAccessRequest) (io.ReadWriteCloser, error) {
	return t.OpenPrivateAccessTargetWithLifetime(ctx, ctx, request)
}

func (t PrivateAccessRouteTarget) OpenPrivateAccessTargetWithLifetime(openCtx, lifetimeCtx context.Context, request connectorprotocol.PrivateAccessRequest) (io.ReadWriteCloser, error) {
	if request.Protocol == "http" {
		if t.HTTP == nil {
			return nil, ErrPrivateAccessStreamInvalid
		}
		return t.HTTP.OpenPrivateAccessTarget(openCtx, request)
	}
	if request.Protocol != "tcp" || request.ResourceKind != "tunnel" || request.Audience != "paperboat-tunnel-tcp" || t.Routes == nil || t.Carriers == nil {
		return nil, ErrPrivateAccessStreamInvalid
	}
	_, rules, ok := t.Routes.CanonicalSnapshot()
	if !ok {
		return nil, ErrPrivateAccessStreamInvalid
	}
	matched, err := privateTCPRule(rules, request)
	if err != nil {
		return nil, err
	}
	return t.Carriers.openExactAssignmentWithTimeout(openCtx, lifetimeCtx, matched, request.RequestID)
}

func privateTCPRule(rules []route.RouteRule, request connectorprotocol.PrivateAccessRequest) (route.RouteRule, error) {
	var matched *route.RouteRule
	for i := range rules {
		rule := &rules[i]
		if privateTCPRuleMatches(*rule, request) {
			if matched != nil {
				return route.RouteRule{}, ErrPrivateAccessStreamInvalid
			}
			matched = rule
		}
	}
	if matched == nil {
		return route.RouteRule{}, ErrPrivateAccessStreamInvalid
	}
	return *matched, nil
}

func privateTCPRuleMatches(rule route.RouteRule, request connectorprotocol.PrivateAccessRequest) bool {
	return rule.RouteID == request.RouteID && rule.Kind == route.TunnelPrivateTCP && rule.AccountID == request.AccountID && rule.TunnelID == request.ResourceID && rule.ConnectorID == request.ConnectorID && rule.ConnectorSessionID == request.CarrierSessionID && rule.RouteGeneration == request.RouteGeneration && rule.SessionGeneration == request.SessionGeneration && rule.ConnectorProcessGeneration == request.ProcessGeneration && rule.ConfigGeneration == request.ConfigGeneration && rule.AssignmentGeneration == request.AssignmentGeneration && rule.Node == request.EdgeNodeID && rule.EdgeProcessEpoch == request.EdgeProcessEpoch && rule.AccessMode == "private" && rule.Protocol == "private_tcp"
}

var _ PrivateAccessTarget = PrivateAccessRouteTarget{}

func (b *PrivateAccessStreamBridge) observe(ctx context.Context, phase string, err error) {
	if err == nil || b.onFailure == nil {
		return
	}
	if requestCanceled(ctx, err) || requestErrorLeaves(err, func(leaf error) bool {
		if reset, ok := leaf.(*yamux.StreamError); ok && reset != nil && ctxErr(ctx) == context.Canceled && reset.ErrorCode == 0 {
			return true
		}
		return leaf == io.EOF || leaf == context.Canceled || ctxErr(ctx) == context.Canceled && (leaf == net.ErrClosed || leaf == io.ErrClosedPipe || leaf == datacarrier.ErrCarrierClosed || leaf == yamux.ErrStreamReset || leaf == yamux.ErrStreamClosed || leaf == yamux.ErrSessionShutdown)
	}, true) {
		return
	}
	current := err
	for depth := 0; current != nil && depth < 8; depth++ {
		if attempt, ok := current.(*control.RequestFailure); ok && attempt != nil {
			return
		}
		wrapper, ok := current.(interface{ Unwrap() error })
		if !ok {
			break
		}
		current = wrapper.Unwrap()
	}
	b.onFailure(ctx, phase, err)
}

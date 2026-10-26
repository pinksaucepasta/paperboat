package edgehttp

import (
	"context"
	"errors"
	runtimeadmission "github.com/pinksaucepasta/paperboat/shared/admission"
	"io"
	"net"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
	"github.com/pinksaucepasta/paperboat/tunnel/internal/datacarrier"
	"github.com/pinksaucepasta/paperboat/tunnel/internal/edgeerrors"
)

// ForwardPublicTCP forwards one connection already accepted by the listener
// named in a current server decision. Listener allocation and lifecycle remain
// with the caller. This method takes ownership of connection and closes it on return.
func (r *DataCarrierRouteRegistry) ForwardPublicTCP(ctx context.Context, connection net.Conn, decision connectorprotocol.IngressDecision, currentAuthority func(context.Context, connectorprotocol.IngressDecision) (connectorprotocol.IngressDecision, error)) error {
	if connection != nil {
		defer connection.Close()
	}
	now := time.Now().UTC()
	if r == nil || ctx == nil || connection == nil || currentAuthority == nil || decision.Validate(now) != nil || (decision.Binding.Protocol != "tcp" && decision.Binding.Protocol != "tls") || decision.Binding.Audience != "public" || decision.Binding.ListenerID == "" || decision.Binding.PublicPort == 0 {
		return ErrDataCarrierRouteInvalid
	}
	if decision.Binding.Protocol == "tls" {
		inspected, ok := connection.(*replayTLSConn)
		if !ok || !inspected.sharedListener || inspected.hostname != decision.Binding.Hostname {
			return ErrDataCarrierRouteInvalid
		}
	}
	local, ok := connection.LocalAddr().(*net.TCPAddr)
	if decision.Binding.Protocol == "tcp" && (!ok || local.Port != int(decision.Binding.PublicPort)) {
		return ErrDataCarrierRouteInvalid
	}
	if _, ok := connection.(interface{ CloseWrite() error }); !ok {
		return ErrDataCarrierRouteInvalid
	}
	requestID := "request_" + uuid.NewString()
	open := connectorprotocol.StreamOpen{Protocol: connectorprotocol.ProtocolName, Version: connectorprotocol.ProtocolVersion, AccountID: decision.Binding.AccountID, TunnelID: decision.Binding.TunnelID, ConnectorID: decision.ConnectorID, SessionID: decision.SessionID, ProcessGeneration: decision.ProcessGeneration, Generation: decision.ConfigGeneration, RouteID: decision.Binding.RouteID, RequestID: requestID, Kind: "tcp_public"}
	authority, err := resolvePublicTCPAuthority(ctx, currentAuthority, decision, open)
	if err != nil {
		return err
	}
	lease, err := r.AcquireIngress(ctx, authority)
	if err != nil {
		return err
	}
	defer lease.Release()
	identity := datacarrier.Identity{AccountID: open.AccountID, HostID: authority.Binding.HostID, TunnelID: open.TunnelID, ConnectorID: open.ConnectorID, SessionID: open.SessionID, ProcessGeneration: open.ProcessGeneration, Generation: open.Generation}
	r.mu.RLock()
	entry := r.byKey[carrierRouteIdentityKey(identity)]
	if r.closed || entry == nil || entry.server == nil {
		r.mu.RUnlock()
		return ErrDataCarrierRouteUnavailable
	}
	server, engine := entry.server, entry.state.Engine
	r.mu.RUnlock()
	stream, err := server.OpenStreamWithLifetime(ctx, ctx, open)
	if err != nil {
		return errors.Join(ErrDataCarrierRouteTransport, err)
	}
	defer stream.Close()
	// A public stream on an agent's runtime carrier carries no decision preface (PLAN 3a 4.4):
	// the edge authorized it above and keeps checking its authority while it runs.
	if !engine {
		if err := connectorprotocol.WriteIngressDecision(stream, authority, time.Now().UTC()); err != nil {
			return errors.Join(ErrDataCarrierRouteTransport, err)
		}
	}
	applicationStream := lease.Wrap(ctx, stream)
	defer applicationStream.Close()
	keepPublicTCPAlive(connection)
	return bridgePublicTCP(ctx, connection, applicationStream, authority, open, currentAuthority, r.publicTCPIdle)
}

// publicTCPKeepAlive finds a vanished public client in about a minute instead of leaving its
// stream to the idle timeout. Prior art: cloudflared's tcpKeepAlive default of 30 s.
var publicTCPKeepAlive = net.KeepAliveConfig{Enable: true, Idle: 30 * time.Second, Interval: 10 * time.Second, Count: 3}

func keepPublicTCPAlive(connection net.Conn) {
	if replay, ok := connection.(*replayTLSConn); ok {
		connection = replay.Conn
	}
	if tcp, ok := connection.(*net.TCPConn); ok {
		_ = tcp.SetKeepAliveConfig(publicTCPKeepAlive)
	}
}

func resolvePublicTCPAuthority(ctx context.Context, current func(context.Context, connectorprotocol.IngressDecision) (connectorprotocol.IngressDecision, error), decision connectorprotocol.IngressDecision, open connectorprotocol.StreamOpen) (connectorprotocol.IngressDecision, error) {
	resolveCtx, cancel := context.WithDeadline(ctx, decision.ExpiresAt)
	defer cancel()
	authority, err := current(resolveCtx, decision)
	now := time.Now().UTC()
	if err != nil {
		return connectorprotocol.IngressDecision{}, edgeerrors.Wrap(edgeerrors.CodeServiceUnavailable, "Public TCP authority is unavailable.", "Retry when current authority is available.", errors.Join(connectorprotocol.ErrIngressDenied, err))
	}
	if decision.Authorize(authority, open, decision.EdgeNodeID, decision.EdgeProcessEpoch, now) != nil || (authority.Binding.Protocol != "tcp" && authority.Binding.Protocol != "tls") || authority.Binding.Audience != "public" {
		return connectorprotocol.IngressDecision{}, connectorprotocol.ErrIngressDenied
	}
	return authority, nil
}

func bridgePublicTCP(ctx context.Context, connection net.Conn, stream io.ReadWriteCloser, authority connectorprotocol.IngressDecision, open connectorprotocol.StreamOpen, current func(context.Context, connectorprotocol.IngressDecision) (connectorprotocol.IngressDecision, error), idle time.Duration) error {
	lease, err := runtimeadmission.Admit(ctx, time.Time{}, nil)
	if err != nil {
		return err
	}
	defer lease.Close()
	stopConnection := lease.Bind(connection)
	defer stopConnection()
	stopStream := lease.Bind(stream)
	defer stopStream()
	active := authority
	stopWatch := lease.Poll(func(check context.Context) error {
		refreshed, err := current(check, active)
		if err == connectorprotocol.ErrIngressDenied {
			return runtimeadmission.ErrRevoked
		}
		if err != nil {
			return err
		}
		if active.Revalidate(refreshed, open, active.EdgeNodeID, active.EdgeProcessEpoch, time.Now().UTC()) != nil {
			return runtimeadmission.ErrRevoked
		}
		active = refreshed
		return nil
	})
	defer stopWatch()
	results := make(chan error, 2)
	var last atomic.Int64
	last.Store(time.Now().UnixNano())
	copyDirection := func(destination io.Writer, source io.Reader, closeWrite func() error) {
		_, copyErr := io.Copy(destination, activityReader{source, &last})
		copyErr = errors.Join(copyErr, closeWrite())
		results <- copyErr
	}
	streamCloseWrite := func() error { return nil }
	if closer, ok := stream.(interface{ CloseWrite() error }); ok {
		streamCloseWrite = closer.CloseWrite
	}
	connectionCloseWrite := func() error { return nil }
	if closer, ok := connection.(interface{ CloseWrite() error }); ok {
		connectionCloseWrite = closer.CloseWrite
	}
	go copyDirection(stream, connection, streamCloseWrite)
	go copyDirection(connection, stream, connectionCloseWrite)
	idled := make(chan struct{})
	if idle > 0 {
		stopIdle := closeWhenIdle(idle, &last, idled, func() {
			// Both sides are silent: a FIN each way first, so neither peer sees a reset.
			_ = connectionCloseWrite()
			_ = streamCloseWrite()
			_ = connection.Close()
			_ = stream.Close()
		})
		defer stopIdle()
	}
	first := <-results
	if first != nil {
		_ = connection.Close()
		_ = stream.Close()
	}
	second := <-results
	lease.Close()
	stopWatch()
	if errors.Is(context.Cause(lease.Context()), runtimeadmission.ErrRevoked) {
		return errors.Join(connectorprotocol.ErrIngressDenied, first, second)
	}
	select {
	case <-idled:
		return nil
	default:
	}
	return errors.Join(ctx.Err(), first, second)
}

// activityReader records the time of every read that carried data.
type activityReader struct {
	io.Reader
	last *atomic.Int64
}

func (r activityReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if n > 0 {
		r.last.Store(time.Now().UnixNano())
	}
	return n, err
}

// closeWhenIdle runs shut once no data has moved for idle, then closes idled. The returned
// function stops the watch.
func closeWhenIdle(idle time.Duration, last *atomic.Int64, idled chan struct{}, shut func()) func() {
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		timer := time.NewTimer(idle)
		defer timer.Stop()
		for {
			select {
			case <-stop:
				return
			case <-timer.C:
			}
			if quiet := time.Since(time.Unix(0, last.Load())); quiet < idle {
				timer.Reset(idle - quiet)
				continue
			}
			close(idled)
			shut()
			return
		}
	}()
	return func() { close(stop); <-done }
}

package tunnelmanager

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/connectorprotocol"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/connector"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hoststate"
)

const (
	maximumOriginRequestHeaderBytes  = 64 << 10
	maximumOriginResponseHeaderBytes = 64 << 10
)

// OriginStreamForwarder owns the one accept loop for a durable tunnel data
// carrier. It binds every stream to the authenticated carrier identity and an
// exact active route before any origin dial occurs.
type OriginStreamForwarder struct {
	Transport        *OriginHTTPTransport
	IngressAuthority func(context.Context, connectorprotocol.StreamOpen, connectorprotocol.IngressDecision) (connectorprotocol.IngressDecision, error)
}

type RunningOriginStreams struct {
	cancel    context.CancelFunc
	done      chan struct{}
	once      sync.Once
	wg        sync.WaitGroup
	transport *OriginHTTPTransport
}

func (f OriginStreamForwarder) Start(parent context.Context, active *connector.ActiveDataCarrier, routes []hoststate.TunnelConfigRoute) (*RunningOriginStreams, error) {
	if parent == nil || active == nil || f.Transport == nil || len(routes) == 0 {
		return nil, ErrInvalidConfig
	}
	byID := make(map[string]hoststate.TunnelConfigRoute, len(routes))
	for _, route := range routes {
		if route.DesiredState == "active" {
			byID[route.ID] = route
		}
	}
	if len(byID) == 0 {
		return nil, ErrInvalidConfig
	}
	identity, ok := active.Identity()
	if !ok {
		return nil, ErrInvalidConfig
	}
	ctx, cancel := context.WithCancel(parent)
	transport := f.Transport.newGeneration()
	running := &RunningOriginStreams{cancel: cancel, done: make(chan struct{}), transport: transport}
	go func() {
		f.serve(ctx, active, identity, byID, running, transport)
		running.wg.Wait()
		close(running.done)
	}()
	return running, nil
}

func (f OriginStreamForwarder) serve(ctx context.Context, active *connector.ActiveDataCarrier, identity connector.DataCarrierIdentity, routes map[string]hoststate.TunnelConfigRoute, running *RunningOriginStreams, transport *OriginHTTPTransport) {
	permits := make(map[string]chan struct{}, len(routes))
	for id, route := range routes {
		permits[id] = make(chan struct{}, max(1, int(route.MaxConcurrentStreams)))
	}
	for {
		stream, open, err := active.AcceptStream(ctx)
		if err != nil {
			return
		}
		if open.Validate() != nil {
			open, err = connectorprotocol.ReadStreamOpen(stream)
			if err != nil {
				_ = stream.Close()
				continue
			}
		}
		if open.AccountID != identity.AccountID || open.TunnelID != identity.TunnelID || open.ConnectorID != identity.ConnectorID || open.SessionID != identity.SessionID || open.ProcessGeneration != identity.ProcessGeneration || open.Generation != identity.Generation {
			_ = stream.Close()
			continue
		}
		route, ok := routes[open.RouteID]
		if ok && open.Kind == "connector_ready" && f.IngressAuthority != nil {
			// Local origin probing has already passed before this generation
			// starts accepting streams. This confirms that exact ready carrier;
			// it cannot forward payloads or initiate an origin probe.
			stopWrite := time.AfterFunc(2*time.Second, func() { _ = stream.Close() })
			_, _ = stream.Write([]byte{1})
			stopWrite.Stop()
			_ = stream.Close()
			continue
		}
		if !ok || !(route.Protocol == "http" && validOriginStreamKind(open.Kind) || route.Protocol == "tcp" && open.Kind == "tcp_public" && f.IngressAuthority != nil) {
			_ = stream.Close()
			continue
		}
		if open.Kind == "http_browser" && f.IngressAuthority == nil {
			_ = stream.Close()
			continue
		}
		permit := permits[route.ID]
		select {
		case permit <- struct{}{}:
		default:
			_ = stream.Close()
			continue
		}
		running.wg.Add(1)
		go func() {
			defer running.wg.Done()
			defer func() { <-permit }()
			defer stream.Close()
			stopCancel := context.AfterFunc(ctx, func() { _ = stream.Close() })
			defer stopCancel()
			streamCtx := ctx
			if f.IngressAuthority != nil {
				authorized, release, err := f.admitIngress(ctx, stream, open, route)
				if err != nil {
					return
				}
				defer release()
				streamCtx = authorized
			}
			bounded := idleOriginStream{ReadWriteCloser: stream, idle: time.Duration(route.IdleTimeoutMs) * time.Millisecond}
			if route.Protocol == "tcp" {
				_ = f.serveTCP(streamCtx, bounded, route)
			} else {
				_ = f.serveHTTP(streamCtx, bounded, route, transport)
			}
		}()
	}
}

func validOriginStreamKind(kind string) bool {
	switch kind {
	case "http_browser", "http", "https", "h2c", "websocket", "sse", "grpc":
		return true
	default:
		return false
	}
}

func (f OriginStreamForwarder) serveHTTP(ctx context.Context, stream io.ReadWriteCloser, route hoststate.TunnelConfigRoute, transport *OriginHTTPTransport) error {
	reader := bufio.NewReader(&originHeaderReader{reader: stream, remaining: maximumOriginRequestHeaderBytes})
	request, err := http.ReadRequest(reader)
	if err != nil {
		return errors.Join(ErrOriginRequestInvalid, err)
	}
	defer request.Body.Close()
	request = request.WithContext(ctx)
	if err := validateIngressHTTPRequest(ctx, request); err != nil {
		return err
	}
	response, err := transport.RoundTrip(ctx, route, request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusSwitchingProtocols {
		upgraded, ok := response.Body.(io.ReadWriteCloser)
		if !ok || !validWebSocketUpgrade(request) || !headerHasToken(response.Header, "Connection", "upgrade") || !strings.EqualFold(response.Header.Get("Upgrade"), "websocket") {
			return ErrOriginRequestInvalid
		}
		headers := *response
		headers.Body = nil
		headers.ContentLength = 0
		if err := headers.Write(stream); err != nil {
			return err
		}
		return bridgeOriginUpgrade(ctx, stream, reader, upgraded)
	}
	return response.Write(stream)
}

func bridgeOriginUpgrade(ctx context.Context, stream io.ReadWriteCloser, client io.Reader, origin io.ReadWriteCloser) error {
	results := make(chan error, 2)
	go func() { _, err := io.Copy(origin, client); results <- err }()
	go func() { _, err := io.Copy(stream, origin); results <- err }()
	first := <-results
	_ = origin.Close()
	_ = stream.Close()
	second := <-results
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return errors.Join(first, second)
}

func (r *RunningOriginStreams) Close(ctx context.Context) error {
	if r == nil || ctx == nil {
		return ErrInvalidConfig
	}
	r.once.Do(r.cancel)
	if r.transport != nil {
		r.transport.CloseIdleConnections()
	}
	select {
	case <-r.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type originHeaderReader struct {
	reader    io.Reader
	remaining int
	complete  bool
	window    [4]byte
	seen      int
}

func (r *originHeaderReader) Read(payload []byte) (int, error) {
	if r.complete {
		return r.reader.Read(payload)
	}
	if r.remaining <= 0 {
		return 0, ErrOriginRequestInvalid
	}
	if len(payload) > r.remaining {
		payload = payload[:r.remaining]
	}
	n, err := r.reader.Read(payload)
	r.remaining -= n
	for _, value := range payload[:n] {
		if r.seen < len(r.window) {
			r.window[r.seen] = value
			r.seen++
		} else {
			copy(r.window[:], r.window[1:])
			r.window[len(r.window)-1] = value
		}
		if r.seen >= len(r.window) && r.window == [4]byte{'\r', '\n', '\r', '\n'} {
			r.complete = true
			break
		}
	}
	return n, err
}

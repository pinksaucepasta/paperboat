package tunnelmanager

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/connectorprotocol"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/connector"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hoststate"
	"github.com/pinksaucepasta/paperboat/internal/inspector"
)

const (
	maximumOriginRequestHeaderBytes  = 64 << 10
	maximumOriginResponseHeaderBytes = 64 << 10
)

// OriginStreamForwarder owns the one accept loop for a durable tunnel data
// carrier. It binds every stream to the authenticated carrier identity and an
// exact active route before any origin dial occurs.
//
// Inspector is the optional shared daemon-local capture store for HTTP(S)
// routes in both ephemeral and durable tunnels. When nil, forwarding runs
// exactly as before. When set, enabled resources capture sanitized records
// without blocking, delaying or altering streaming; a full store drops the
// capture, never the traffic. Raw TCP never captures bodies.
//
// Registry is the optional current replay-binding registry owned by the same
// daemon inspector service. When set alongside Inspector, each authorized
// stream refreshes the resource's current target-bound replay binding, so
// deliberate replay rechecks live authority instead of trusting retained
// bytes. A removed route stops refreshing and goes stale within its
// authority lifetime.
//
// CaptureIdentity overrides the capture/replay identity for forwarding paths
// whose carrier route is not the product resource. Ephemeral preview leases
// are owner-addressed by lease ID with the uniform lease generation (matching
// the native private binding pattern); their carrier route is an opaque
// server hash owners never see. When nil, capture keys to route.ID with the
// admitted decision generations (durable tunnels).
type OriginStreamForwarder struct {
	Transport        *OriginHTTPTransport
	IngressAuthority func(context.Context, connectorprotocol.StreamOpen, connectorprotocol.IngressDecision) (connectorprotocol.IngressDecision, error)
	Inspector        *inspector.Store
	InspectorHTTP    AuthenticatedInspectorHTTP
	Registry         *inspector.Registry
	CaptureIdentity  *CaptureIdentity
}

// CaptureIdentity is the product resource identity for inspector capture and
// replay when it differs from the carrier route.
type CaptureIdentity struct {
	ResourceID         string
	ResourceGeneration uint64
	RouteGeneration    uint64
	TargetGeneration   uint64
	ExpiresAt          time.Time
}

func (id *CaptureIdentity) valid() bool {
	return id != nil && strings.TrimSpace(id.ResourceID) != "" && len(id.ResourceID) <= 256 &&
		id.ResourceGeneration > 0 && id.RouteGeneration > 0 && id.TargetGeneration > 0 && !id.ExpiresAt.IsZero()
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
		if open.Kind == connectorprotocol.InspectorHTTP {
			if f.InspectorHTTP == nil {
				_ = stream.Close()
				continue
			}
			if _, ok := routes[open.RouteID]; !ok {
				_ = stream.Close()
				continue
			}
			running.wg.Add(1)
			go func() {
				defer running.wg.Done()
				defer stream.Close()
				streamCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
				defer cancel()
				stop := context.AfterFunc(streamCtx, func() { _ = stream.Close() })
				defer stop()
				_ = ServeInspectorStream(streamCtx, stream, open, f.InspectorHTTP)
			}()
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
	return f.roundTripAndRespond(ctx, stream, reader, route, transport, request)
}

// roundTripAndRespond forwards one parsed request to its origin. Inspector
// capture, when enabled for route.ID, taps bounded body prefixes through
// TeeReaders so streaming, backpressure and cancellation are preserved: the
// taps never buffer a full body, never block forwarding, and a dropped store
// insert never fails the origin exchange.
func (f OriginStreamForwarder) roundTripAndRespond(ctx context.Context, stream io.ReadWriteCloser, reader *bufio.Reader, route hoststate.TunnelConfigRoute, transport *OriginHTTPTransport, request *http.Request) error {
	pending, capture := f.beginCapture(ctx, route, transport)
	if pending == nil {
		response, err := transport.RoundTrip(ctx, route, request)
		if err != nil {
			return err
		}
		defer response.Body.Close()
		return writeOriginResponse(ctx, stream, reader, request, response)
	}
	return f.exchange(ctx, stream, reader, route, transport, request, pending, capture)
}

// ServePublicHTTP forwards one unauthenticated public HTTP request to its
// origin with inspector capture under the caller's product identity. The
// caller authorized the public route itself (public publication is owner
// authority); identity supplies the resource generations and expiry. The
// reader carries any bytes the caller already peeked for protocol sniffing.
// Upgrades and streams are recorded unsupported; malformed HTTP gets a minimal
// 400 without ever presenting a partial capture. A supplied Registry uses the
// caller-owned transport; preview leaves it nil and registers its own lease
// forwarder because its per-stream transport closes after this exchange.
func (f OriginStreamForwarder) ServePublicHTTP(ctx context.Context, stream io.ReadWriteCloser, reader *bufio.Reader, route hoststate.TunnelConfigRoute, identity CaptureIdentity) error {
	if f.Transport == nil || f.Inspector == nil || stream == nil || ctx == nil || reader == nil || route.Protocol != "http" || !identity.valid() {
		return ErrOriginRequestInvalid
	}
	request, err := http.ReadRequest(bufio.NewReader(&originHeaderReader{reader: reader, remaining: maximumOriginRequestHeaderBytes}))
	if err != nil {
		_, _ = io.WriteString(stream, "HTTP/1.1 400 Bad Request\r\nConnection: close\r\nContent-Length: 0\r\n\r\n")
		return errors.Join(ErrOriginRequestInvalid, err)
	}
	defer request.Body.Close()
	f.CaptureIdentity = &identity
	return f.roundTripAndRespond(ctx, stream, reader, route, f.Transport, request.WithContext(ctx))
}

// exchange forwards one parsed request with bounded capture taps. Forwarding
// never waits for capture; a dropped store insert never fails the exchange.
func (f OriginStreamForwarder) exchange(ctx context.Context, stream io.ReadWriteCloser, reader *bufio.Reader, route hoststate.TunnelConfigRoute, transport *OriginHTTPTransport, request *http.Request, pending *inspector.Pending, capture *inspectorCapture) error {
	requestTap := pending.RequestBodyTap()
	var rawPrefix []byte
	if pending.RawEnabled() {
		rawPrefix = buildRawRequest(request, nil)
	}
	rawTap := pending.RawRequestTap(rawPrefix)
	originalBody := request.Body
	tracked := &captureRequestReader{reader: io.TeeReader(originalBody, io.MultiWriter(requestTap, rawTap)), expected: request.ContentLength}
	request.Body = &tapReadCloser{Reader: tracked, closer: originalBody}
	response, err := transport.RoundTrip(ctx, route, request)
	if err != nil {
		capture.finishError(request, err)
		return err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusSwitchingProtocols {
		capture.finishUpgrade(request, response)
		return writeOriginResponse(ctx, stream, reader, request, response)
	}
	responseTap := pending.ResponseBodyTap()
	originalResponse := response.Body
	response.Body = &tapReadCloser{Reader: responseTap.TapReader(originalResponse), closer: originalResponse}
	writeErr := writeOriginResponse(ctx, stream, reader, request, response)
	capture.finish(request, response, requestTap, responseTap, rawTap, !tracked.complete(), writeErr)
	return writeErr
}

// The HTTP transport may return an early response before consuming the entire
// request. Such a request cannot become replayable from its captured prefix.
type captureRequestReader struct {
	reader   io.Reader
	expected int64
	read     atomic.Int64
	eof      atomic.Bool
}

func (r *captureRequestReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.read.Add(int64(n))
	if err == io.EOF {
		r.eof.Store(true)
	}
	return n, err
}
func (r *captureRequestReader) complete() bool {
	return r.eof.Load() || (r.expected >= 0 && r.read.Load() == r.expected)
}

// tapReadCloser forwards reads through a capture tap while preserving the
// original body's Close for transport connection reuse.
type tapReadCloser struct {
	io.Reader
	closer io.Closer
}

func (t *tapReadCloser) Close() error {
	if t == nil || t.closer == nil {
		return nil
	}
	return t.closer.Close()
}

// inspectorCapture carries one in-flight capture across the origin exchange.
type inspectorCapture struct {
	store   *inspector.Store
	pending *inspector.Pending
	binding *connectorprotocol.IngressBinding
	// identity overrides binding generations when the product resource
	// differs from the carrier route (ephemeral preview leases).
	identity *CaptureIdentity
}

func (f OriginStreamForwarder) beginCapture(ctx context.Context, route hoststate.TunnelConfigRoute, transport *OriginHTTPTransport) (*inspector.Pending, *inspectorCapture) {
	none := &inspectorCapture{}
	if f.Inspector == nil || route.Protocol != "http" {
		return nil, none
	}
	resourceID := route.ID
	if f.CaptureIdentity.valid() {
		resourceID = f.CaptureIdentity.ResourceID
	}
	pending, err := f.Inspector.TryBegin(resourceID)
	if err != nil {
		return nil, none
	}
	capture := &inspectorCapture{store: f.Inspector, pending: pending}
	if f.CaptureIdentity.valid() {
		claimed := *f.CaptureIdentity
		capture.identity = &claimed
	} else if binding, ok := ctx.Value(ingressBindingKey{}).(connectorprotocol.IngressBinding); ok {
		claimed := binding
		capture.binding = &claimed
	}
	if f.Registry != nil && transport != nil {
		if gens, expiry, ok := f.replayAuthority(ctx, capture); ok {
			bound := route
			// Best-effort registration: a failed registration only disables
			// replay for this resource, never the traffic or its capture.
			_ = f.Registry.Register(resourceID, inspector.ReplayBinding{
				ResourceGeneration: gens[0],
				RouteGeneration:    gens[1],
				TargetGeneration:   gens[2],
				ExpiresAt:          expiry,
				Available:          func() bool { transport.mu.Lock(); defer transport.mu.Unlock(); return !transport.closed },
				Forward: func(forwardCtx context.Context, method, requestURI string, header http.Header, body []byte) (int, http.Header, []byte, bool, error) {
					return ReplayViaTransport(forwardCtx, transport, bound, method, requestURI, header, body)
				},
			})
		}
	}
	return pending, capture
}

// replayAuthority resolves the current replay generations and expiry: the
// explicit capture identity when set, else the admitted decision binding.
func (f OriginStreamForwarder) replayAuthority(ctx context.Context, capture *inspectorCapture) ([3]uint64, time.Time, bool) {
	if capture != nil && capture.identity != nil {
		return [3]uint64{capture.identity.ResourceGeneration, capture.identity.RouteGeneration, capture.identity.TargetGeneration}, capture.identity.ExpiresAt, true
	}
	if capture == nil || capture.binding == nil {
		return [3]uint64{}, time.Time{}, false
	}
	// The target snapshot lasts only as long as raw capture can be retained.
	// A fresh inspector decision (not the original ingress grant) authorizes
	// every replay; local transport closure separately fences removal.
	expiry := capture.pending.StartedAt.Add(inspector.RawRetention)
	return [3]uint64{capture.binding.ResourceGeneration, capture.binding.RouteGeneration, capture.binding.TargetGeneration}, expiry, true
}

// ReplayViaTransport sends one deliberate replay request to the same current
// origin through the route's transport and TLS policy. The caller supplies
// only parsed method/target/headers/body; destination and trust policy come
// from the bound route and can never be overridden per request. It is
// exported so ephemeral preview forwarding can reuse the exact same replay
// path with its own per-lease transport instead of a per-stream one.
func ReplayViaTransport(ctx context.Context, transport *OriginHTTPTransport, route hoststate.TunnelConfigRoute, method, requestURI string, header http.Header, body []byte) (int, http.Header, []byte, bool, error) {
	if ctx == nil || transport == nil || method == "" || !strings.HasPrefix(requestURI, "/") || int64(len(body)) > inspector.RawMaxBytes {
		return 0, nil, nil, false, ErrOriginRequestInvalid
	}
	target := strings.TrimSpace(requestURI)
	if len(target) > inspector.URLMaxBytes {
		return 0, nil, nil, false, ErrOriginRequestInvalid
	}
	request, err := http.NewRequestWithContext(ctx, method, "http://paperboat-replay.invalid"+target, bytes.NewReader(body))
	if err != nil {
		return 0, nil, nil, false, ErrOriginRequestInvalid
	}
	request.Header = header.Clone()
	request.Host = header.Get("Host")
	request.Header.Del("Host")
	response, err := transport.RoundTrip(ctx, route, request)
	if err != nil {
		return 0, nil, nil, false, err
	}
	defer response.Body.Close()
	bounded, err := io.ReadAll(io.LimitReader(response.Body, inspector.BodyMaxBytes+1))
	if err != nil {
		return 0, nil, nil, false, errors.Join(ErrOriginUnavailable, err)
	}
	truncated := int64(len(bounded)) > inspector.BodyMaxBytes
	if truncated {
		bounded = bounded[:inspector.BodyMaxBytes]
	}
	return response.StatusCode, response.Header.Clone(), bounded, truncated, nil
}

func (c *inspectorCapture) finishError(request *http.Request, _ error) {
	if c == nil || c.store == nil || c.pending == nil {
		return
	}
	completed := inspector.CompletedCapture{
		Method:         request.Method,
		RawURL:         requestURLString(request),
		RequestHeaders: request.Header,
		ErrorCode:      "origin_unavailable",
		StartedAt:      c.pending.StartedAt,
		FinishedAt:     time.Now().UTC(),
	}
	c.applyGenerations(&completed)
	_, _ = c.store.Finish(c.pending, completed)
}

// finishUpgrade records a WebSocket upgrade without body bytes: streams are
// explicitly unsupported for redacted body display and replay.
func (c *inspectorCapture) finishUpgrade(request *http.Request, response *http.Response) {
	if c == nil || c.store == nil || c.pending == nil {
		return
	}
	completed := inspector.CompletedCapture{
		Method:              request.Method,
		RawURL:              requestURLString(request),
		RequestHeaders:      request.Header,
		ResponseHeaders:     response.Header,
		RequestUnsupported:  true,
		ResponseUnsupported: true,
		ResponseStatus:      response.StatusCode,
		StartedAt:           c.pending.StartedAt,
		FinishedAt:          time.Now().UTC(),
	}
	c.applyGenerations(&completed)
	_, _ = c.store.Finish(c.pending, completed)
}

func (c *inspectorCapture) finish(request *http.Request, response *http.Response, requestTap, responseTap, rawTap *inspector.BodyTap, incomplete bool, writeErr error) {
	if c == nil || c.store == nil || c.pending == nil {
		return
	}
	requestBody, requestTruncated := requestTap.Take()
	responseBody, responseTruncated := responseTap.Take()
	raw, rawTruncated := rawTap.Take()
	completed := inspector.CompletedCapture{
		Method:            request.Method,
		RawURL:            requestURLString(request),
		RequestHeaders:    request.Header,
		ResponseHeaders:   response.Header,
		RequestBody:       requestBody,
		RequestTruncated:  requestTruncated,
		ResponseBody:      responseBody,
		ResponseTruncated: responseTruncated,
		ResponseStatus:    response.StatusCode,
		StartedAt:         c.pending.StartedAt,
		FinishedAt:        time.Now().UTC(),
		RawRequest:        raw, RawTruncated: rawTruncated, RequestIncomplete: incomplete,
		RequestUnsupported:  unsupportedInspectorStream(request.Header),
		ResponseUnsupported: unsupportedInspectorStream(response.Header),
	}
	if writeErr != nil {
		completed.ErrorCode = "forwarding_failed"
	}
	c.applyGenerations(&completed)
	_, _ = c.store.Finish(c.pending, completed)
}

// applyGenerations stamps the record with the product resource generations:
// the explicit capture identity when set, else the admitted binding.
func unsupportedInspectorStream(header http.Header) bool {
	contentType := strings.ToLower(header.Get("Content-Type"))
	return strings.HasPrefix(contentType, "text/event-stream") || strings.HasPrefix(contentType, "application/grpc")
}

func (c *inspectorCapture) applyGenerations(completed *inspector.CompletedCapture) {
	if c == nil || completed == nil {
		return
	}
	if c.identity != nil {
		completed.ResourceGeneration = c.identity.ResourceGeneration
		completed.RouteGeneration = c.identity.RouteGeneration
		completed.TargetGeneration = c.identity.TargetGeneration
		return
	}
	applyBindingGenerations(completed, c.binding)
}

func applyBindingGenerations(completed *inspector.CompletedCapture, binding *connectorprotocol.IngressBinding) {
	if completed == nil || binding == nil {
		return
	}
	completed.ResourceGeneration = binding.ResourceGeneration
	completed.RouteGeneration = binding.RouteGeneration
	completed.TargetGeneration = binding.TargetGeneration
}

func requestURLString(request *http.Request) string {
	if request == nil || request.URL == nil {
		return ""
	}
	url := *request.URL
	if url.Host == "" {
		url.Host = request.Host
	}
	if url.Scheme == "" {
		url.Scheme = "http"
	}
	return url.String()
}

// buildRawRequest assembles the exact raw request prefix for future replay,
// capped at the v1 raw budget. It uses original unsanitized bytes and is
// stored separately, never returned by ordinary list/detail reads.
func buildRawRequest(request *http.Request, body []byte) []byte {
	if request == nil {
		return nil
	}
	var raw bytes.Buffer
	method := request.Method
	if method == "" {
		method = "GET"
	}
	target := request.URL.RequestURI()
	if target == "" {
		target = "/"
	}
	raw.WriteString(method + " " + target + " HTTP/1.1\r\n")
	if request.Host != "" {
		raw.WriteString("Host: " + request.Host + "\r\n")
	}
	for name, values := range request.Header {
		for _, value := range values {
			line := name + ": " + value + "\r\n"
			if int64(raw.Len()+len(line)) > inspector.RawMaxBytes {
				break
			}
			raw.WriteString(line)
		}
	}
	raw.WriteString("\r\n")
	remaining := int(inspector.RawMaxBytes) - raw.Len()
	if remaining > 0 && len(body) > 0 {
		if len(body) > remaining {
			raw.Write(body[:remaining])
		} else {
			raw.Write(body)
		}
	}
	return raw.Bytes()
}

func writeOriginResponse(ctx context.Context, stream io.ReadWriteCloser, reader *bufio.Reader, request *http.Request, response *http.Response) error {
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

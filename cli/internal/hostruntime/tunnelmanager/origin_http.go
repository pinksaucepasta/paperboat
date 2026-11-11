package tunnelmanager

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hoststate"
	sharedprotocol "github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
	"golang.org/x/net/http/httpguts"
	"golang.org/x/net/http2"
)

var ErrOriginRequestInvalid = errors.New("invalid origin HTTP request")

// OriginHTTPTransport forwards one already edge-normalized request to its
// route origin. Public Host and TLS SNI are independent: Host is preserved by
// default or explicitly overridden, while SNI comes only from tls_server_name
// or the origin address. Paperboat-internal and hop-by-hop headers never cross
// this final trust boundary.
type OriginHTTPTransport struct {
	Dialer  OriginDialer
	TLS     OriginTLSProvider
	Observe func(OriginProbeObservation)
	// KeepControlPlaneHeaders lets X-Paperboat-* headers through in both directions. It is for the
	// machine's own runtime listener, where the control plane sends an operation id and reads a file
	// name; a user's origin never sees or sets those headers.
	KeepControlPlaneHeaders bool

	mu         sync.Mutex
	transports map[string]originRoundTripper
	closed     bool
}

type originRoundTripper interface {
	http.RoundTripper
	CloseIdleConnections()
}

func (t *OriginHTTPTransport) RoundTrip(ctx context.Context, route hoststate.TunnelConfigRoute, request *http.Request) (*http.Response, error) {
	if ctx == nil || request == nil || request.URL == nil || route.Protocol != "http" || route.DesiredState != "active" {
		return nil, ErrOriginRequestInvalid
	}
	switch route.OriginScheme {
	case "http", "h2c", "https", "unix", "file":
	default:
		return nil, ErrOriginRequestInvalid
	}
	if route.ConnectTimeoutMs < 100 || route.IdleTimeoutMs < 1000 {
		return nil, ErrOriginRequestInvalid
	}
	publicHost := request.Header.Get("X-Forwarded-Host")
	if publicHost == "" {
		publicHost = request.Host
	}
	route = withPublicServerName(route, publicHost)
	transport, err := t.transportFor(ctx, route)
	if err != nil {
		return nil, err
	}
	out := request.Clone(ctx)
	out.RequestURI = ""
	out.URL.Scheme = route.OriginScheme
	if route.OriginScheme == "h2c" || route.OriginScheme == "unix" || route.OriginScheme == "file" {
		out.URL.Scheme = "http"
	}
	out.URL.Host = route.OriginAddress
	if route.OriginScheme == "unix" || route.OriginScheme == "file" {
		out.URL.Host = "paperboat-origin.invalid"
	}
	if route.HostOverride != nil {
		out.Host = *route.HostOverride
	} else if !route.PreserveHost {
		out.Host = route.OriginAddress
	}
	webSocketUpgrade := validWebSocketUpgrade(out)
	sanitizeOriginRequestHeaders(out.Header, webSocketUpgrade, t.KeepControlPlaneHeaders)
	response, err := transport.RoundTrip(out)
	if err != nil {
		return nil, errors.Join(ErrOriginUnavailable, err)
	}
	if t.Observe != nil && sharedprotocol.IsSelfRedirect(out.Method, out.Header.Get("X-Forwarded-Host"), out.URL.RequestURI(), response) {
		t.Observe(OriginProbeObservation{RouteID: route.ID, Code: OriginRedirectLoop})
	}
	responseUpgrade := webSocketUpgrade && response.StatusCode == http.StatusSwitchingProtocols && headerHasToken(response.Header, "Connection", "upgrade") && strings.EqualFold(strings.TrimSpace(response.Header.Get("Upgrade")), "websocket")
	sanitizeOriginResponseHeaders(response.Header, responseUpgrade, t.KeepControlPlaneHeaders)
	if publicHost != "" && !strings.EqualFold(out.Host, publicHost) {
		restorePublicHost(response.Header, out.Host, publicHost)
	}
	return response, nil
}

func (t *OriginHTTPTransport) transportFor(ctx context.Context, route hoststate.TunnelConfigRoute) (originRoundTripper, error) {
	if t == nil {
		return nil, ErrInvalidConfig
	}
	key := originTransportKey(route)
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil, ErrOriginUnavailable
	}
	if transport := t.transports[key]; transport != nil {
		t.mu.Unlock()
		return transport, nil
	}
	t.mu.Unlock()
	dialer := t.Dialer
	if dialer == nil {
		dialer = &LocalhostDialer{Dialer: net.Dialer{Timeout: time.Duration(route.ConnectTimeoutMs) * time.Millisecond, KeepAlive: 30 * time.Second}}
	}
	var transport originRoundTripper
	standard := &http.Transport{
		Proxy:                  nil,
		DialContext:            dialer.DialContext,
		ForceAttemptHTTP2:      route.OriginScheme == "https",
		DisableCompression:     true,
		IdleConnTimeout:        originIdleConnTimeout,
		TLSHandshakeTimeout:    time.Duration(route.ConnectTimeoutMs) * time.Millisecond,
		ExpectContinueTimeout:  time.Second,
		MaxIdleConnsPerHost:    2,
		MaxResponseHeaderBytes: maximumOriginResponseHeaderBytes,
	}
	if route.OriginScheme == "unix" || route.OriginScheme == "file" {
		// A "file" origin is served in process; only the preview carrier's dialer can reach it.
		if route.OriginScheme == "file" && t.Dialer == nil {
			return nil, ErrInvalidConfig
		}
		standard.DialContext = func(dialCtx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(dialCtx, route.OriginScheme, route.OriginAddress)
		}
	}
	if route.OriginScheme == "https" {
		prober := NetworkOriginProber{TLS: t.TLS}
		tlsConfig, err := prober.originTLSConfig(ctx, route)
		if err != nil {
			return nil, errors.Join(ErrOriginUnavailable, err)
		}
		standard.TLSClientConfig = tlsConfig
	}
	transport = standard
	if route.OriginScheme == "h2c" {
		transport = &http2.Transport{AllowHTTP: true, MaxHeaderListSize: maximumOriginResponseHeaderBytes, DialTLSContext: func(dialCtx context.Context, network, _ string, _ *tls.Config) (net.Conn, error) {
			return dialer.DialContext(dialCtx, network, route.OriginAddress)
		}}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		transport.CloseIdleConnections()
		return nil, ErrOriginUnavailable
	}
	if current := t.transports[key]; current != nil {
		transport.CloseIdleConnections()
		return current, nil
	}
	if t.transports == nil {
		t.transports = make(map[string]originRoundTripper)
	}
	if len(t.transports) >= maximumOriginTransports { // wildcard routes key by visitor-chosen SNI
		for oldKey, old := range t.transports {
			old.CloseIdleConnections()
			delete(t.transports, oldKey)
			break
		}
	}
	t.transports[key] = transport
	return transport, nil
}

// originIdleConnTimeout drops a pooled origin connection before Node's 5 s keep-alive closes it, so a
// POST never lands on a connection the origin is closing (Go retries only idempotent requests).
const originIdleConnTimeout = 4 * time.Second

// maximumOriginTransports bounds the pooled origin transports of one generation (one per route
// and SNI); the oldest-chosen entry is dropped at random when full.
const maximumOriginTransports = 256

// withPublicServerName makes an https route to localhost or an IP literal present the public
// hostname as SNI: neither can carry a real certificate name, and Caddy, Traefik and per-site
// nginx choose the site and certificate by SNI. An explicit tls_server_name or a DNS-name origin
// keeps its own name. The transport is keyed by TLSServerName, so no shared transport changes.
func withPublicServerName(route hoststate.TunnelConfigRoute, publicHost string) hoststate.TunnelConfigRoute {
	if route.OriginScheme != "https" || route.TLSServerName != nil || publicHost == "" || !sharedprotocol.OriginHostIsLocalhostOrIP(route.OriginAddress) {
		return route
	}
	if host, _, err := net.SplitHostPort(publicHost); err == nil {
		publicHost = host
	}
	route.TLSServerName = &publicHost
	return route
}

func originTransportKey(route hoststate.TunnelConfigRoute) string {
	field := func(value *string) string {
		if value == nil {
			return "-"
		}
		return *value
	}
	return strings.Join([]string{route.ID, route.OriginScheme, route.OriginAddress, strconv.FormatBool(route.PreserveHost), field(route.HostOverride), route.TLSVerification, field(route.TLSServerName), field(route.CAReference), field(route.MTLSCredentialReference), field(route.TLSPin), strconv.FormatInt(int64(route.ConnectTimeoutMs), 10), strconv.FormatInt(int64(route.IdleTimeoutMs), 10)}, "\x00")
}

func (t *OriginHTTPTransport) newGeneration() *OriginHTTPTransport {
	if t == nil {
		return nil
	}
	return &OriginHTTPTransport{Dialer: t.Dialer, TLS: t.TLS, Observe: t.Observe, KeepControlPlaneHeaders: t.KeepControlPlaneHeaders, transports: make(map[string]originRoundTripper)}
}

func (t *OriginHTTPTransport) CloseIdleConnections() {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.closed = true
	transports := t.transports
	t.transports = nil
	t.mu.Unlock()
	for _, transport := range transports {
		transport.CloseIdleConnections()
	}
}

func sanitizeOriginRequestHeaders(header http.Header, preserveWebSocketUpgrade, keepControlPlane bool) {
	if header == nil {
		return
	}
	// "TE: trailers" survives: a gRPC client says so, and strict servers (grpc-java, Envoy) refuse without it.
	trailers := httpguts.HeaderValuesContainsToken(header["Te"], "trailers")
	connectionTokens := strings.Split(header.Get("Connection"), ",")
	for _, token := range connectionTokens {
		if token = strings.TrimSpace(token); token != "" {
			header.Del(token)
		}
	}
	for _, name := range []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "TE", "Trailer", "Transfer-Encoding", "Upgrade"} {
		header.Del(name)
	}
	if trailers {
		header.Set("Te", "trailers")
	}
	for name := range header {
		lower := strings.ToLower(name)
		control := strings.HasPrefix(lower, "x-paperboat-")
		if control && keepControlPlane {
			continue
		}
		forwarded := strings.HasPrefix(lower, "x-forwarded-") || lower == "forwarded" || lower == "x-real-ip"
		if control || strings.HasPrefix(lower, "paperboat-") && lower != "paperboat-visitor-assertion" || forwarded && !edgeForwardedHeaders[lower] {
			header.Del(name)
		}
	}
	sanitizeCookieHeader(header)
	if preserveWebSocketUpgrade {
		header.Set("Connection", "Upgrade")
		header.Set("Upgrade", "websocket")
	}
}

// edgeForwardedHeaders are the forwarding headers the edge sets from its own connection, replacing
// whatever the visitor sent. Only the authenticated edge carrier feeds this transport, so exactly these
// cross the final trust boundary; any other X-Forwarded-* name (server, prefix) is dropped.
var edgeForwardedHeaders = map[string]bool{"forwarded": true, "x-forwarded-for": true, "x-forwarded-host": true, "x-forwarded-proto": true, "x-forwarded-port": true, "x-real-ip": true}

func sanitizeOriginResponseHeaders(header http.Header, preserveWebSocketUpgrade, keepControlPlane bool) {
	if header == nil {
		return
	}
	for _, token := range strings.Split(header.Get("Connection"), ",") {
		if token = strings.TrimSpace(token); token != "" {
			header.Del(token)
		}
	}
	for _, name := range []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "TE", "Trailer", "Transfer-Encoding", "Upgrade"} {
		header.Del(name)
	}
	for name := range header {
		lower := strings.ToLower(name)
		if (strings.HasPrefix(lower, "x-paperboat-") && !keepControlPlane) || strings.HasPrefix(lower, "paperboat-") {
			header.Del(name)
		}
	}
	values := header.Values("Set-Cookie")
	header.Del("Set-Cookie")
	for _, value := range values {
		name := strings.TrimSpace(strings.SplitN(value, "=", 2)[0])
		if !isReservedPaperboatCookie(name) {
			header.Add("Set-Cookie", value)
		}
	}
	if preserveWebSocketUpgrade {
		header.Set("Connection", "Upgrade")
		header.Set("Upgrade", "websocket")
	}
}

// restorePublicHost undoes a route's Host rewrite in the origin's answer, as ngrok does: an app that
// was told it is originHost (localhost:3000) redirects there and scopes cookies to it, which the
// visitor's browser cannot reach or keep. A Location on originHost moves to https://publicHost and a
// cookie Domain naming originHost is dropped, so the cookie binds to the public host.
func restorePublicHost(header http.Header, originHost, publicHost string) {
	if location, err := url.Parse(header.Get("Location")); err == nil && location.Host != "" && strings.EqualFold(location.Host, originHost) {
		location.Scheme, location.Host = "https", publicHost
		header.Set("Location", location.String())
	}
	originName := originHost
	if name, _, err := net.SplitHostPort(originHost); err == nil {
		originName = name
	}
	cookies := header.Values("Set-Cookie")
	for index, cookie := range cookies {
		parts := strings.Split(cookie, ";")
		kept := parts[:1]
		for _, attribute := range parts[1:] {
			name, value, _ := strings.Cut(strings.TrimSpace(attribute), "=")
			if strings.EqualFold(name, "domain") && strings.EqualFold(strings.TrimPrefix(value, "."), originName) {
				continue
			}
			kept = append(kept, attribute)
		}
		cookies[index] = strings.Join(kept, ";")
	}
}

func sanitizeCookieHeader(header http.Header) {
	values := header.Values("Cookie")
	header.Del("Cookie")
	var kept []string
	for _, value := range values {
		for _, cookie := range strings.Split(value, ";") {
			cookie = strings.TrimSpace(cookie)
			name := strings.TrimSpace(strings.SplitN(cookie, "=", 2)[0])
			if cookie != "" && !isReservedPaperboatCookie(name) {
				kept = append(kept, cookie)
			}
		}
	}
	if len(kept) > 0 {
		header.Set("Cookie", strings.Join(kept, "; "))
	}
}

func isReservedPaperboatCookie(name string) bool {
	lower := strings.ToLower(strings.TrimSpace(name))
	lower = strings.TrimPrefix(lower, "__host-")
	lower = strings.TrimPrefix(lower, "__secure-")
	return strings.HasPrefix(lower, "paperboat-") || strings.HasPrefix(lower, "paperboat_")
}

func validWebSocketUpgrade(request *http.Request) bool {
	if request == nil || request.Method != http.MethodGet || !headerHasToken(request.Header, "Connection", "upgrade") || !strings.EqualFold(strings.TrimSpace(request.Header.Get("Upgrade")), "websocket") || strings.TrimSpace(request.Header.Get("Sec-WebSocket-Version")) != "13" {
		return false
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(request.Header.Get("Sec-WebSocket-Key")))
	return err == nil && len(key) == 16
}

func headerHasToken(header http.Header, name, token string) bool {
	for _, value := range header.Values(name) {
		for _, candidate := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(candidate), token) {
				return true
			}
		}
	}
	return false
}

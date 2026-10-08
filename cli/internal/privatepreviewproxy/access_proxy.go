package privatepreviewproxy

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/idna"
	"golang.org/x/net/netutil"
)

const (
	AccessMatchExact            = "exact"
	AccessMatchManagedExact     = "managed_exact"
	AccessMatchOneLabelWildcard = "one_label_wildcard"

	defaultAccessMaximumConnections = 128
	defaultAccessMaximumRoutes      = 4096
	defaultAccessHeaderTimeout      = 10 * time.Second
	defaultAccessIdleTimeout        = 2 * time.Minute
	defaultAccessOpenTimeout        = 15 * time.Second
	maximumAccessHeaderBytes        = 32 << 10
)

var (
	ErrAccessProxyInvalid           = errors.New("invalid private access proxy configuration")
	ErrAccessAuthentication         = errors.New("Paperboat authentication is required")
	ErrAccessForbidden              = errors.New("Paperboat private access is not allowed")
	ErrAccessTemporarilyUnavailable = errors.New("Paperboat private access is temporarily unavailable")
)

// AccessRoute is safe local routing metadata. A snapshot contains only routes
// that the current machine session may attempt. Open still reauthorizes the
// concrete host so a cached PAC response cannot grant access after revocation.
type AccessRoute struct {
	MatchType      string
	Hostname       string
	WildcardSuffix string
}

// AccessSource is owned by stable hostd. Snapshot drives only the narrow PAC
// selection. Open must check the current renewable machine session and open a
// fresh authenticated, route-bound carrier stream for the concrete hostname.
type AccessSource interface {
	Snapshot(context.Context) ([]AccessRoute, error)
	Open(context.Context, string) (io.ReadWriteCloser, error)
}

type AccessProxyConfig struct {
	ListenPort         uint16
	Source             AccessSource
	MaximumConnections int
	MaximumRoutes      int
	HeaderTimeout      time.Duration
	IdleTimeout        time.Duration
	OpenTimeout        time.Duration
}

// AccessProxy is a literal-loopback HTTP CONNECT proxy and PAC endpoint. It
// never terminates browser TLS and never reads browser cookies or credentials.
type AccessProxy struct {
	ProxyAddress string
	PACURL       string

	listener   net.Listener
	server     *http.Server
	source     AccessSource
	maximum    int
	openWait   time.Duration
	runContext context.Context
	cancel     context.CancelFunc
	done       chan error

	snapshotGate chan struct{}
	pacMu        sync.RWMutex
	pacBodies    map[string][]byte
	mu           sync.Mutex
	active       map[io.Closer]struct{}
	once         sync.Once
}

// StartAccessProxy starts one hostd-owned proxy. The PAC URL and proxy address
// are safe to publish locally; neither is an access credential.
func StartAccessProxy(ctx context.Context, config AccessProxyConfig) (*AccessProxy, error) {
	if config.MaximumConnections == 0 {
		config.MaximumConnections = defaultAccessMaximumConnections
	}
	if config.MaximumRoutes == 0 {
		config.MaximumRoutes = defaultAccessMaximumRoutes
	}
	if config.HeaderTimeout == 0 {
		config.HeaderTimeout = defaultAccessHeaderTimeout
	}
	if config.IdleTimeout == 0 {
		config.IdleTimeout = defaultAccessIdleTimeout
	}
	if config.OpenTimeout == 0 {
		config.OpenTimeout = defaultAccessOpenTimeout
	}
	if ctx == nil || config.Source == nil || config.MaximumConnections < 1 || config.MaximumConnections > 4096 || config.MaximumRoutes < 1 || config.MaximumRoutes > defaultAccessMaximumRoutes || config.HeaderTimeout <= 0 || config.HeaderTimeout > time.Minute || config.IdleTimeout <= 0 || config.IdleTimeout > time.Hour || config.OpenTimeout <= 0 || config.OpenTimeout > time.Minute {
		return nil, ErrAccessProxyInvalid
	}
	listener, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(config.ListenPort))))
	if err != nil {
		return nil, err
	}
	runContext, cancel := context.WithCancel(ctx)
	proxy := &AccessProxy{
		listener: listener, source: config.Source, maximum: config.MaximumRoutes,
		openWait: config.OpenTimeout, runContext: runContext, cancel: cancel, done: make(chan error, 1),
		active: make(map[io.Closer]struct{}), snapshotGate: make(chan struct{}, 1),
	}
	address := listener.Addr().String()
	proxy.ProxyAddress = address
	proxy.PACURL = "http://" + address + "/proxy.pac"
	proxy.server = &http.Server{
		Handler:           proxy,
		ReadHeaderTimeout: config.HeaderTimeout,
		IdleTimeout:       config.IdleTimeout,
		MaxHeaderBytes:    maximumAccessHeaderBytes,
	}
	limited := netutil.LimitListener(listener, config.MaximumConnections)
	go func() {
		err := proxy.server.Serve(limited)
		if errors.Is(err, http.ErrServerClosed) || errors.Is(err, net.ErrClosed) {
			err = nil
		}
		proxy.done <- err
		close(proxy.done)
	}()
	go func() {
		<-runContext.Done()
		_ = proxy.Close()
	}()
	return proxy, nil
}

func (p *AccessProxy) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if p == nil || p.source == nil || request == nil || request.URL == nil {
		writeAccessProxyError(writer, http.StatusServiceUnavailable)
		return
	}
	if request.Method == http.MethodGet && (request.URL.Path == "/proxy.pac" || strings.HasPrefix(request.URL.Path, "/proxy-")) {
		p.servePAC(writer, request)
		return
	}
	if request.Method != http.MethodConnect {
		writer.Header().Set("Allow", http.MethodConnect+", "+http.MethodGet)
		writeAccessProxyError(writer, http.StatusMethodNotAllowed)
		return
	}
	p.serveConnect(writer, request)
}

func (p *AccessProxy) servePAC(writer http.ResponseWriter, request *http.Request) {
	if request.URL.RawQuery != "" || request.Header.Get("Authorization") != "" || request.Header.Get("Proxy-Authorization") != "" || request.Header.Get("Cookie") != "" {
		writeAccessProxyError(writer, http.StatusBadRequest)
		return
	}
	if request.URL.Path != "/proxy.pac" {
		p.pacMu.RLock()
		payload, ok := p.pacBodies[request.URL.Path]
		p.pacMu.RUnlock()
		if !ok {
			writeAccessProxyError(writer, http.StatusNotFound)
			return
		}
		writePAC(writer, payload)
		return
	}
	routes, err := p.snapshot(request.Context())
	if err != nil {
		writeAccessProxyError(writer, accessHTTPStatus(err))
		return
	}
	normalized, err := normalizeAccessRoutes(routes, p.maximum)
	if err != nil {
		writeAccessProxyError(writer, http.StatusServiceUnavailable)
		return
	}
	payload, err := renderPAC(p.ProxyAddress, normalized)
	if err != nil {
		writeAccessProxyError(writer, http.StatusServiceUnavailable)
		return
	}
	writePAC(writer, payload)
}

func (p *AccessProxy) serveConnect(writer http.ResponseWriter, request *http.Request) {
	if request.Header.Get("Authorization") != "" || request.Header.Get("Proxy-Authorization") != "" || request.Header.Get("Cookie") != "" {
		writeAccessProxyError(writer, http.StatusBadRequest)
		return
	}
	host, port, err := normalizeConnectAuthority(request.Host)
	if err != nil || port != "443" {
		writeAccessProxyError(writer, http.StatusBadRequest)
		return
	}
	routes, err := p.snapshot(request.Context())
	if err != nil {
		writeAccessProxyError(writer, accessHTTPStatus(err))
		return
	}
	normalized, err := normalizeAccessRoutes(routes, p.maximum)
	if err != nil {
		writeAccessProxyError(writer, http.StatusServiceUnavailable)
		return
	}
	if !accessHostAllowed(host, normalized) {
		writeAccessProxyError(writer, http.StatusForbidden)
		return
	}
	// Opening is bounded, but the admitted stream belongs to this CONNECT
	// connection until client disconnect, grant expiry or daemon shutdown.
	connectionContext, cancel := context.WithCancel(p.runContext)
	handedOff := false
	defer func() {
		if !handedOff {
			cancel()
		}
	}()
	stopRequest := context.AfterFunc(request.Context(), cancel)
	openingTimer := time.AfterFunc(p.openWait, cancel)
	remote, err := p.source.Open(connectionContext, host)
	if !openingTimer.Stop() {
		cancel()
		err = context.DeadlineExceeded
	}
	if !stopRequest() {
		cancel()
	}
	if err == nil {
		err = connectionContext.Err()
	}
	if err != nil || remote == nil {
		if remote != nil {
			_ = remote.Close()
		}
		writeAccessProxyError(writer, accessHTTPStatus(err))
		return
	}
	hijacker, ok := writer.(http.Hijacker)
	if !ok {
		_ = remote.Close()
		writeAccessProxyError(writer, http.StatusServiceUnavailable)
		return
	}
	local, buffered, err := hijacker.Hijack()
	if err != nil {
		_ = remote.Close()
		return
	}
	if err := writeConnectEstablished(buffered); err != nil {
		_ = local.Close()
		_ = remote.Close()
		return
	}
	p.track(local, true)
	p.track(remote, true)
	handedOff = true
	go func() {
		defer cancel()
		_ = copyBoth(local, remote)
		p.track(local, false)
		p.track(remote, false)
		_ = local.Close()
		_ = remote.Close()
	}()
}

func writeConnectEstablished(writer *bufio.ReadWriter) error {
	if _, err := writer.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return err
	}
	return writer.Flush()
}

func (p *AccessProxy) track(closer io.Closer, add bool) {
	p.mu.Lock()
	if add {
		if p.runContext.Err() != nil {
			p.mu.Unlock()
			_ = closer.Close()
			return
		}
		p.active[closer] = struct{}{}
	} else {
		delete(p.active, closer)
	}
	p.mu.Unlock()
}

func (p *AccessProxy) Wait() error {
	if p == nil || p.done == nil {
		return ErrAccessProxyInvalid
	}
	return <-p.done
}

func (p *AccessProxy) Close() error {
	if p == nil {
		return nil
	}
	var result error
	p.once.Do(func() {
		p.cancel()
		shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := p.server.Shutdown(shutdownContext); err != nil && !errors.Is(err, net.ErrClosed) {
			result = errors.Join(result, err)
		}
		cancel()
		if err := p.listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			result = errors.Join(result, err)
		}
		p.mu.Lock()
		active := make([]io.Closer, 0, len(p.active))
		for closer := range p.active {
			active = append(active, closer)
		}
		p.mu.Unlock()
		for _, closer := range active {
			if err := closer.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
				result = errors.Join(result, err)
			}
		}
	})
	return result
}

func normalizeAccessRoutes(routes []AccessRoute, maximum int) ([]AccessRoute, error) {
	if len(routes) > maximum {
		return nil, ErrAccessProxyInvalid
	}
	result := make([]AccessRoute, 0, len(routes))
	seen := make(map[string]struct{}, len(routes))
	for _, route := range routes {
		matchType := strings.ToLower(strings.TrimSpace(route.MatchType))
		if matchType == "" {
			matchType = AccessMatchExact
		}
		normalized := AccessRoute{MatchType: matchType}
		switch matchType {
		case AccessMatchExact, AccessMatchManagedExact:
			host, err := normalizeAccessHost(route.Hostname)
			if err != nil || route.WildcardSuffix != "" {
				return nil, ErrAccessProxyInvalid
			}
			normalized.Hostname = host
		case AccessMatchOneLabelWildcard:
			suffix := route.WildcardSuffix
			if suffix == "" && strings.HasPrefix(route.Hostname, "*.") {
				suffix = strings.TrimPrefix(route.Hostname, "*.")
			}
			host, err := normalizeAccessHost(suffix)
			if err != nil || !strings.Contains(host, ".") {
				return nil, ErrAccessProxyInvalid
			}
			normalized.WildcardSuffix = host
		default:
			return nil, ErrAccessProxyInvalid
		}
		key := normalized.Hostname
		if normalized.MatchType == AccessMatchOneLabelWildcard {
			key = "*." + normalized.WildcardSuffix
		}
		if _, exists := seen[key]; exists {
			return nil, ErrAccessProxyInvalid
		}
		seen[key] = struct{}{}
		result = append(result, normalized)
	}
	sort.Slice(result, func(left, right int) bool {
		leftKey := result[left].MatchType + "\x00" + result[left].Hostname + result[left].WildcardSuffix
		rightKey := result[right].MatchType + "\x00" + result[right].Hostname + result[right].WildcardSuffix
		return leftKey < rightKey
	})
	return result, nil
}

func normalizeAccessHost(value string) (string, error) {
	if strings.TrimSpace(value) != value || value == "" || strings.ContainsAny(value, "\r\n\x00 /?#:*") || net.ParseIP(value) != nil {
		return "", ErrAccessProxyInvalid
	}
	value = strings.TrimSuffix(value, ".")
	ascii, err := idna.Lookup.ToASCII(value)
	if err != nil || ascii == "" || len(ascii) > 253 || strings.Contains(ascii, "..") {
		return "", ErrAccessProxyInvalid
	}
	for _, label := range strings.Split(ascii, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", ErrAccessProxyInvalid
		}
	}
	return strings.ToLower(ascii), nil
}

func normalizeConnectAuthority(authority string) (string, string, error) {
	if strings.TrimSpace(authority) != authority || strings.ContainsAny(authority, "\r\n\x00/@") {
		return "", "", ErrAccessProxyInvalid
	}
	host, port, err := net.SplitHostPort(authority)
	if err != nil {
		return "", "", ErrAccessProxyInvalid
	}
	host, err = normalizeAccessHost(host)
	if err != nil {
		return "", "", err
	}
	return host, port, nil
}

func accessHostAllowed(host string, routes []AccessRoute) bool {
	for _, route := range routes {
		switch route.MatchType {
		case AccessMatchExact, AccessMatchManagedExact:
			if host == route.Hostname {
				return true
			}
		case AccessMatchOneLabelWildcard:
			if strings.HasSuffix(host, "."+route.WildcardSuffix) {
				prefix := strings.TrimSuffix(host, "."+route.WildcardSuffix)
				if prefix != "" && !strings.Contains(prefix, ".") {
					return true
				}
			}
		}
	}
	return false
}

func renderPAC(proxyAddress string, routes []AccessRoute) ([]byte, error) {
	encodedProxy, err := json.Marshal("PROXY " + proxyAddress)
	if err != nil {
		return nil, err
	}
	var builder strings.Builder
	builder.WriteString("function FindProxyForURL(url, host) {\n")
	builder.WriteString("  host = host.toLowerCase().replace(/\\.$/, '');\n")
	for _, route := range routes {
		switch route.MatchType {
		case AccessMatchExact, AccessMatchManagedExact:
			encodedHost, _ := json.Marshal(route.Hostname)
			fmt.Fprintf(&builder, "  if (host === %s) return %s;\n", encodedHost, encodedProxy)
		case AccessMatchOneLabelWildcard:
			encodedSuffix, _ := json.Marshal(route.WildcardSuffix)
			fmt.Fprintf(&builder, "  if (dnsDomainIs(host, '.' + %s) && dnsDomainLevels(host) === dnsDomainLevels(%s) + 1) return %s;\n", encodedSuffix, encodedSuffix, encodedProxy)
		}
	}
	builder.WriteString("  return 'DIRECT';\n}\n")
	return []byte(builder.String()), nil
}

func accessHTTPStatus(err error) int {
	switch {
	case errors.Is(err, ErrAccessAuthentication):
		return http.StatusUnauthorized
	case errors.Is(err, ErrAccessForbidden):
		return http.StatusForbidden
	default:
		return http.StatusServiceUnavailable
	}
}

func writeAccessProxyError(writer http.ResponseWriter, status int) {
	message := "Paperboat private access is unavailable.\n"
	switch status {
	case http.StatusUnauthorized:
		message = "Paperboat authentication is required.\n"
	case http.StatusForbidden:
		message = "Paperboat private access is not allowed.\n"
	case http.StatusBadRequest:
		message = "Invalid proxy request.\n"
	case http.StatusMethodNotAllowed:
		message = "Proxy method is not allowed.\n"
	}
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.WriteHeader(status)
	_, _ = io.WriteString(writer, message)
}

// snapshot serializes route discovery across PAC requests, admission and monitoring.
func (p *AccessProxy) snapshot(ctx context.Context) ([]AccessRoute, error) {
	select {
	case p.snapshotGate <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-p.snapshotGate }()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return p.source.Snapshot(ctx)
}

// publishPAC retains the installed and candidate revisions for refresh rollback.
// Its digest covers only sorted routing tuples, never authority or expiration.
func (p *AccessProxy) publishPAC(ctx context.Context, installed string) (string, error) {
	routes, err := p.snapshot(ctx)
	if err != nil {
		return "", err
	}
	normalized, err := normalizeAccessRoutes(routes, p.maximum)
	if err != nil {
		return "", err
	}
	payload, err := renderPAC(p.ProxyAddress, normalized)
	if err != nil {
		return "", err
	}
	tuples, err := json.Marshal(normalized)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(tuples)
	path := fmt.Sprintf("/proxy-%x.pac", digest)
	p.pacMu.Lock()
	defer p.pacMu.Unlock()
	if p.pacBodies == nil {
		p.pacBodies = make(map[string][]byte)
	}
	if _, exists := p.pacBodies[path]; !exists {
		p.pacBodies[path] = payload
	}
	installedPath := strings.TrimPrefix(installed, "http://"+p.ProxyAddress)
	for key := range p.pacBodies {
		if key != path && key != installedPath {
			delete(p.pacBodies, key)
		}
	}
	return "http://" + p.ProxyAddress + path, nil
}

func writePAC(writer http.ResponseWriter, payload []byte) {
	writer.Header().Set("Cache-Control", "no-store, max-age=0")
	writer.Header().Set("Content-Type", "application/x-ns-proxy-autoconfig")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write(payload)
}

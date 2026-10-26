package splitdns

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"html"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/shared/supportref"
)

// ProxyConfig configures explicit local browser service routes.
type ProxyConfig struct {
	Domain           string
	HTTPListenAddr   string // e.g. "127.100.0.1:80" or ":80" or custom
	HTTPSListenAddr  string // e.g. "127.100.0.1:443" or ":443" or custom
	Routes           map[string]BrowserRoute
	DeniedHosts      map[string]bool
	DialContext      func(context.Context, string, string) (net.Conn, error)
	IssueCertificate func(context.Context, string) (tls.Certificate, error)
	RevocationList   func(context.Context, string) ([]byte, []byte, error)
	// AwaitRoute is asked when a TLS hello names <port>.<machine> of a known machine whose port has no
	// route yet (a service that just started): it may refresh the machine's services and returns once
	// a route can have arrived, within ctx. Nil fails such a hello at once.
	AwaitRoute func(ctx context.Context, hostname string) bool
}

// awaitRouteTimeout bounds how long a TLS hello waits for a just-started service's route.
const awaitRouteTimeout = 6 * time.Second

// Proxy serves only explicitly registered browser hostnames.
type Proxy struct {
	domain           string
	mu               sync.RWMutex
	routes           map[string]BrowserRoute
	deniedHosts      map[string]bool
	machines         map[string][]string
	revocationList   func(context.Context, string) ([]byte, []byte, error)
	httpAddr         string
	httpsAddr        string
	httpServer       *http.Server
	httpsServer      *http.Server
	certCacheMu      sync.RWMutex
	transport        *http.Transport
	issueCertificate func(context.Context, string) (tls.Certificate, error)
	awaitRoute       func(context.Context, string) bool
	httpListener     net.Listener
	httpsListener    net.Listener
	portServers      []*http.Server
	portListeners    []net.Listener
	servedPorts      map[int]int // local listener port -> published machine port
	started          bool
	stopErr          error
}

// NewProxy creates a router for registered browser services.
func NewProxy(cfg ProxyConfig) (*Proxy, error) {
	if cfg.DialContext == nil {
		return nil, errors.New("splitdns proxy: authorized DialContext is required")
	}
	httpAddr := cfg.HTTPListenAddr
	if httpAddr == "" {
		httpAddr = "127.100.0.1:80"
	}
	httpsAddr := cfg.HTTPSListenAddr
	if httpsAddr == "" {
		httpsAddr = "127.100.0.1:443"
	}

	domain, err := NormalizeBrowserDomain(cfg.Domain)
	if err != nil {
		return nil, err
	}
	routes, denied, machines, err := browserRouteTable(cfg.Routes, cfg.DeniedHosts, domain)
	if err != nil {
		return nil, err
	}
	p := &Proxy{
		domain:           domain,
		routes:           routes,
		deniedHosts:      denied,
		machines:         machines,
		revocationList:   cfg.RevocationList,
		httpAddr:         httpAddr,
		httpsAddr:        httpsAddr,
		transport:        &http.Transport{DialContext: cfg.DialContext, ForceAttemptHTTP2: false, MaxIdleConns: 64, MaxIdleConnsPerHost: 8, IdleConnTimeout: 30 * time.Second, ResponseHeaderTimeout: 15 * time.Second},
		issueCertificate: cfg.IssueCertificate,
		awaitRoute:       cfg.AwaitRoute,
	}
	return p, nil
}

// browserRouteTable validates explicit routes and denied hostnames and indexes the routed names by
// machine. NewProxy and SetRoutes share it, so a running proxy accepts exactly what a new one would.
func browserRouteTable(explicit map[string]BrowserRoute, deniedHosts map[string]bool, domain string) (map[string]BrowserRoute, map[string]bool, map[string][]string, error) {
	routes := make(map[string]BrowserRoute, len(explicit))
	for host, route := range explicit {
		_, label, parseErr := ParseBrowserHostname(strings.TrimPrefix(host, "*."), domain)
		wildcard := strings.HasPrefix(host, "*.")
		if wildcard {
			_, parseErr = BrowserWildcardPattern(strings.TrimSuffix(strings.TrimPrefix(host, "*."), "."+domain), domain)
		}
		if parseErr != nil || (!wildcard && label == "") || !route.Address.IsValid() || route.Port < 1 || route.Port > 65535 {
			return nil, nil, nil, errors.New("invalid explicit browser route")
		}
		routes[host] = route
	}
	for host, route := range routes {
		if !strings.HasPrefix(host, "*.") {
			continue
		}
		machine, _, _ := ParseBrowserHostname(strings.TrimPrefix(host, "*."), domain)
		numeric, err := BrowserHostname(machine, route.Port, domain)
		if err != nil || routes[numeric] != route {
			return nil, nil, nil, errors.New("browser proxy requires the same authorized numeric route")
		}
	}
	denied := make(map[string]bool, len(deniedHosts))
	for host := range deniedHosts {
		_, label, err := ParseBrowserHostname(host, domain)
		if err != nil || label == "" || NumericBrowserLabel(label) {
			return nil, nil, nil, errors.New("invalid denied browser hostname")
		}
		if _, ok := routes[host]; ok {
			return nil, nil, nil, errors.New("browser hostname both denied and routed")
		}
		denied[host] = true
	}
	machines := make(map[string][]string)
	for host := range routes {
		if strings.HasPrefix(host, "*.") {
			continue
		}
		_, base, _ := strings.Cut(host, ".")
		machines[base] = append(machines[base], host)
	}
	for base := range machines {
		sort.Strings(machines[base])
	}
	return routes, denied, machines, nil
}

// SetRoutes replaces the routes and denied hostnames of a running proxy without stopping its listeners,
// so a service that just started is reachable at once instead of after a gateway restart (Lane H:
// private-https to a fresh port saw TLS internal errors and refused connections during the rebuild).
// Cached certificates are issued per request, so names no longer routed are refused at the next hello.
func (p *Proxy) SetRoutes(explicit map[string]BrowserRoute, deniedHosts map[string]bool) error {
	routes, denied, machines, err := browserRouteTable(explicit, deniedHosts, p.domain)
	if err != nil {
		return err
	}
	p.certCacheMu.Lock()
	p.routes, p.deniedHosts, p.machines = routes, denied, machines
	p.certCacheMu.Unlock()
	return nil
}

// RemovePortListener stops serving the per-port listener bound to port, if there is one.
func (p *Proxy) RemovePortListener(port int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for index, listener := range p.portListeners {
		local, ok := listener.Addr().(*net.TCPAddr)
		if !ok || local.Port != port {
			continue
		}
		p.certCacheMu.Lock()
		delete(p.servedPorts, port)
		p.certCacheMu.Unlock()
		err := p.portServers[index].Close()
		p.portServers = append(p.portServers[:index], p.portServers[index+1:]...)
		p.portListeners = append(p.portListeners[:index], p.portListeners[index+1:]...)
		return err
	}
	return nil
}

// ServeHTTP implements http.Handler to dynamically reverse-proxy to target port.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, CRLPathPrefix) {
		p.serveCRL(w, r)
		return
	}
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}

	host = strings.TrimSuffix(strings.ToLower(host), ".")
	routeHost := host
	// https://<machine>.<domain>:<port> arrives on a listener bound to that port; the
	// listener's port selects the same numeric route as https://<port>.<machine>.<domain>.
	if port, ok := p.servedLocalPort(r); ok {
		numeric, err := BrowserHostname(strings.TrimSuffix(host, "."+p.domain), port, p.domain)
		if err != nil || !strings.HasSuffix(host, "."+p.domain) {
			http.Error(w, "Unknown or mismatched Paperboat browser host", http.StatusMisdirectedRequest)
			return
		}
		routeHost = numeric
	}
	p.certCacheMu.RLock()
	route, found := ResolveBrowserRoute(p.routes, routeHost, p.domain)
	if p.deniedHosts[routeHost] {
		found = false
	}
	services, machine := p.machines[host]
	if routeHost != host {
		machine = false
	}
	p.certCacheMu.RUnlock()
	if routeHost != host && !found {
		http.Error(w, "No service is published on this port of the machine", http.StatusNotFound)
		return
	}
	if (!found && !machine) || (r.TLS != nil && !strings.EqualFold(strings.TrimSuffix(r.TLS.ServerName, "."), host)) {
		http.Error(w, "Unknown or mismatched Paperboat browser host", http.StatusMisdirectedRequest)
		return
	}
	if machine {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if r.URL.Path != "/" || r.URL.RawQuery != "" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'; base-uri 'none'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if r.Method == http.MethodHead {
			return
		}
		alias := strings.TrimSuffix(host, "."+p.domain)
		_, _ = fmt.Fprintf(w, "<!doctype html><title>%s — Paperboat</title><h1>%s</h1><ul>", html.EscapeString(alias), html.EscapeString(alias))
		for _, name := range services {
			port, _, _ := strings.Cut(name, ".")
			label := "Port " + port
			if _, err := strconv.Atoi(port); err != nil {
				label = port
			}
			_, _ = fmt.Fprintf(w, `<li><a href="https://%s/">%s</a></li>`, html.EscapeString(name), html.EscapeString(label))
		}
		_, _ = fmt.Fprint(w, "</ul>")
		return
	}
	targetIP, targetPort := route.Address, route.Port
	reference := supportref.New()
	r = r.WithContext(supportref.WithContext(r.Context(), reference))
	w.Header().Set(supportref.Header, reference)

	targetURL := &url.URL{Scheme: "http", Host: net.JoinHostPort(targetIP.String(), strconv.Itoa(targetPort))}

	proxy := httputil.NewSingleHostReverseProxy(targetURL)
	proxy.Transport = p.transport
	proxy.Director = nil
	proxy.Rewrite = func(request *httputil.ProxyRequest) {
		request.SetURL(targetURL)
		request.Out.Host = request.In.Host
		// Rewrite strips caller-provided Forwarded/X-Forwarded-* metadata.
		// Recreate the chain from the actual local request and TLS state only.
		request.SetXForwarded()
		request.Out.Header.Set(supportref.Header, reference)
	}
	proxy.ModifyResponse = func(response *http.Response) error {
		response.Header.Set(supportref.Header, reference)
		return nil
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		fault := errorreport.Current().ObserveFailure(r.Context(), "paperboat-daemon", "local_gateway", "local_gateway", "local_gateway_failed", err)
		w.Header().Set(supportref.Header, fault.SupportReference)
		http.Error(w, gatewayFailureMessage(fault.Stage)+" Support reference: "+fault.SupportReference, http.StatusBadGateway)
	}

	proxy.ServeHTTP(w, r)
}

func gatewayFailureMessage(stage string) string {
	switch stage {
	case "grant_issue":
		return "Paperboat could not authorize this service. Check that the machine and port are still available to your account, then retry."
	case "peer_authority", "peer_connect":
		return "Paperboat could not connect to the machine. Check that it is online, then retry."
	case "stream_open":
		return "Paperboat could not open the service connection. Retry; if this continues, run `pb doctor`."
	case "target_connect":
		return "Paperboat could not reach the service on the machine. Check that the application is listening on this port, then retry."
	default:
		return "Paperboat could not complete the service request. Retry; if this continues, run `pb doctor`."
	}
}

// GetCertificate serves a locally trusted certificate only for an active route.
func (p *Proxy) GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	serverName := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(hello.ServerName)), ".")
	permitted, machine := p.routedName(serverName)
	if !permitted && !machine && p.awaitRoute != nil && p.knownMachinePort(serverName) {
		// A service that just started is announced before its route reaches this gateway: wait for it
		// rather than failing the handshake (Lane H: TLS internal error on a fresh port).
		ctx, cancel := context.WithTimeout(hello.Context(), awaitRouteTimeout)
		if p.awaitRoute(ctx, serverName) {
			permitted, machine = p.routedName(serverName)
		}
		cancel()
	}
	if !permitted && !machine {
		return nil, errors.New("TLS name has no active browser route")
	}
	if p.issueCertificate == nil {
		return nil, errors.New("trusted browser certificate provider is unavailable")
	}
	cert, err := p.issueCertificate(hello.Context(), serverName)
	if err != nil {
		return nil, err
	}
	if !certificateUsable(&cert, serverName, time.Now()) {
		return nil, errors.New("browser certificate is expired or does not cover this route")
	}
	return &cert, nil
}

// routedName reports whether name has an active route, or names a machine.
func (p *Proxy) routedName(name string) (permitted, machine bool) {
	p.certCacheMu.RLock()
	defer p.certCacheMu.RUnlock()
	_, permitted = ResolveBrowserRoute(p.routes, name, p.domain)
	if p.deniedHosts[name] {
		permitted = false
	}
	_, machine = p.machines[name]
	return permitted, machine
}

// knownMachinePort reports whether name is <port>.<machine> for a machine this gateway routes.
func (p *Proxy) knownMachinePort(name string) bool {
	label, base, ok := strings.Cut(name, ".")
	if !ok || !NumericBrowserLabel(label) {
		return false
	}
	p.certCacheMu.RLock()
	defer p.certCacheMu.RUnlock()
	_, known := p.machines[base]
	return known
}

// servedLocalPort reports the local port of a request that arrived on one of the per-port TLS listeners.
func (p *Proxy) servedLocalPort(r *http.Request) (int, bool) {
	address, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if !ok {
		return 0, false
	}
	tcp, ok := address.(*net.TCPAddr)
	if !ok {
		return 0, false
	}
	p.certCacheMu.RLock()
	defer p.certCacheMu.RUnlock()
	port, served := p.servedPorts[tcp.Port]
	return port, served
}

// AddPortListener serves TLS for https://<machine>.<domain>:<port> on a protected listener bound
// to that port. The listener must be added after the proxy has started; Stop closes it.
func (p *Proxy) AddPortListener(port int, listener net.Listener) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.started || p.httpsServer == nil {
		return errors.New("splitdns proxy: port listeners need a started HTTPS proxy")
	}
	local, ok := listener.Addr().(*net.TCPAddr)
	if !ok || port < 1024 || port > 65535 {
		return errors.New("splitdns proxy: invalid port listener")
	}
	p.certCacheMu.Lock()
	if _, exists := p.servedPorts[local.Port]; exists {
		p.certCacheMu.Unlock()
		return errors.New("splitdns proxy: duplicate port listener")
	}
	if p.servedPorts == nil {
		p.servedPorts = map[int]int{}
	}
	// The protected listener is bound to the published port itself; the lookup is keyed by the
	// listener's own port so a request can never select a port other than the one it arrived on.
	p.servedPorts[local.Port] = port
	p.certCacheMu.Unlock()
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: p.GetCertificate}
	bound := tls.NewListener(listener, tlsConfig)
	server := &http.Server{Handler: p, TLSConfig: tlsConfig, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10}
	p.portServers = append(p.portServers, server)
	p.portListeners = append(p.portListeners, bound)
	go func() { _ = server.Serve(bound) }()
	return nil
}

// Start starts the HTTP and HTTPS reverse-proxy servers.
func (p *Proxy) Start() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.started {
		return nil
	}

	httpListener, err := net.Listen("tcp", p.httpAddr)
	if err != nil {
		return fmt.Errorf("listen HTTP proxy: %w", err)
	}
	var httpsListener net.Listener
	if p.issueCertificate != nil {
		httpsListener, err = net.Listen("tcp", p.httpsAddr)
		if err != nil {
			_ = httpListener.Close()
			p.httpListener = nil
			return fmt.Errorf("listen HTTPS proxy: %w", err)
		}
	}
	return p.startListenersLocked(httpListener, httpsListener)
}

// StartListeners serves on already-protected listeners supplied by the
// privileged machine guard. Listener ownership remains with Proxy until Stop.
func (p *Proxy) StartListeners(httpListener, httpsListener net.Listener) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.started {
		return nil
	}
	if httpListener == nil || httpsListener == nil || p.issueCertificate == nil {
		return errors.New("splitdns proxy: protected HTTP/HTTPS listeners and trusted certificates are required")
	}
	return p.startListenersLocked(httpListener, httpsListener)
}

func (p *Proxy) startListenersLocked(httpListener, httpsListener net.Listener) error {
	p.stopErr = nil
	p.httpListener = httpListener
	p.httpServer = &http.Server{Handler: p, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10}
	if httpsListener != nil {
		tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: p.GetCertificate}
		p.httpsListener = tls.NewListener(httpsListener, tlsConfig)
		p.httpsServer = &http.Server{Handler: p, TLSConfig: tlsConfig, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10}
	}
	p.started = true
	httpServer, httpBoundListener := p.httpServer, p.httpListener
	go func() { _ = httpServer.Serve(httpBoundListener) }()
	if p.httpsServer != nil {
		httpsServer, httpsBoundListener := p.httpsServer, p.httpsListener
		go func() { _ = httpsServer.Serve(httpsBoundListener) }()
	}
	return nil
}

func certificateUsable(cert *tls.Certificate, name string, now time.Time) bool {
	if cert == nil || len(cert.Certificate) == 0 {
		return false
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	return err == nil && !now.Before(leaf.NotBefore) && now.Before(leaf.NotAfter) && leaf.VerifyHostname(name) == nil
}

// Stop stops the proxy servers.
type proxyShutdownFailure struct{ cause error }

func (proxyShutdownFailure) Error() string   { return "local browser proxy shutdown failed" }
func (e proxyShutdownFailure) Unwrap() error { return e.cause }

// Only normal closed-listener teardown may be suppressed; independent release
// faults still belong to the shutdown owner, even when joined with net.ErrClosed.
func proxyClosedOnly(err error) (pure bool) {
	defer func() {
		if recover() != nil {
			pure = false
		}
	}()
	pending := []error{err}
	leaves := 0
	for count := 0; len(pending) > 0; count++ {
		if count >= 16 {
			return false
		}
		current := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if current == nil {
			return false
		}
		if current == net.ErrClosed {
			leaves++
			continue
		}
		if joined, ok := current.(interface{ Unwrap() []error }); ok {
			children := joined.Unwrap()
			if len(children) == 0 || len(children) > 16-len(pending) {
				return false
			}
			pending = append(pending, children...)
			continue
		}
		if wrapped, ok := current.(interface{ Unwrap() error }); ok {
			pending = append(pending, wrapped.Unwrap())
			continue
		}
		return false
	}
	return leaves > 0
}

func (p *Proxy) Stop(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.started {
		return p.stopErr
	}
	var errs []error
	record := func(err error) {
		if err != nil && !proxyClosedOnly(err) {
			errs = append(errs, err)
		}
	}
	for _, server := range append([]*http.Server{p.httpServer, p.httpsServer}, p.portServers...) {
		if server != nil {
			if err := server.Shutdown(ctx); err != nil {
				record(err)
				record(server.Close())
			}
		}
	}
	if p.httpListener != nil {
		record(p.httpListener.Close())
		p.httpListener = nil
	}
	if p.httpsListener != nil {
		record(p.httpsListener.Close())
		p.httpsListener = nil
	}
	for _, listener := range p.portListeners {
		record(listener.Close())
	}
	p.portServers, p.portListeners = nil, nil
	p.certCacheMu.Lock()
	p.servedPorts = nil
	p.certCacheMu.Unlock()
	p.transport.CloseIdleConnections()
	p.started = false
	if err := errors.Join(errs...); err != nil {
		p.stopErr = proxyShutdownFailure{cause: err}
	}
	return p.stopErr
}

// The numeric HTTP distribution point serves only validated public revocation
// metadata. It never resolves a route or opens an origin connection.
func (p *Proxy) serveCRL(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.TLS != nil || r.URL.RawQuery != "" || r.Host != BrowserGatewayIP || len(r.URL.Path) != len(CRLPathPrefix)+64+4 || p.revocationList == nil {
		http.NotFound(w, r)
		return
	}
	rootDER, der, err := p.revocationList(r.Context(), r.URL.Path)
	if err != nil || CRLPath(rootDER) != r.URL.Path {
		http.NotFound(w, r)
		return
	}
	list, err := ValidateCRL(rootDER, der, time.Now())
	if err != nil {
		http.Error(w, "Local certificate status unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/pkix-crl")
	w.Header().Set("Content-Length", strconv.Itoa(len(der)))
	w.Header().Set("Cache-Control", "max-age="+strconv.FormatInt(int64(time.Until(list.NextUpdate).Seconds()), 10))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.Method == http.MethodGet {
		_, _ = w.Write(der)
	}
}

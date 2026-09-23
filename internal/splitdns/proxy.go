package splitdns

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"
)

const maxProxyCertificateCache = 128

// ProxyConfig configures explicit local browser service routes.
type ProxyConfig struct {
	HTTPListenAddr   string // e.g. "127.100.0.1:80" or ":80" or custom
	HTTPSListenAddr  string // e.g. "127.100.0.1:443" or ":443" or custom
	Routes           map[string]BrowserRoute
	CA               *CA
	Suffix           string
	DialContext      func(context.Context, string, string) (net.Conn, error)
	IssueCertificate func(context.Context, string) (tls.Certificate, error)
}

// Proxy serves only explicitly registered flat browser hostnames.
type Proxy struct {
	mu               sync.RWMutex
	routes           map[string]BrowserRoute
	ca               *CA
	httpAddr         string
	httpsAddr        string
	httpServer       *http.Server
	httpsServer      *http.Server
	certCache        map[string]*tls.Certificate
	certCacheMu      sync.RWMutex
	suffix           string
	transport        *http.Transport
	issueCertificate func(context.Context, string) (tls.Certificate, error)
	httpListener     net.Listener
	httpsListener    net.Listener
	started          bool
}

// NewProxy creates a router for registered flat browser sites.
func NewProxy(cfg ProxyConfig) (*Proxy, error) {
	if cfg.DialContext == nil {
		return nil, errors.New("splitdns proxy: authorized DialContext is required")
	}
	suffix, err := validateSuffix(cfg.Suffix)
	if err != nil {
		return nil, err
	}

	httpAddr := cfg.HTTPListenAddr
	if httpAddr == "" {
		httpAddr = "127.100.0.1:80"
	}
	httpsAddr := cfg.HTTPSListenAddr
	if httpsAddr == "" {
		httpsAddr = "127.100.0.1:443"
	}

	routes := make(map[string]BrowserRoute, len(cfg.Routes))
	for host, route := range cfg.Routes {
		if !validBrowserHost(host, suffix) || !route.Address.IsValid() || route.Port < 1 || route.Port > 65535 {
			return nil, errors.New("invalid explicit browser route")
		}
		routes[host] = route
	}
	p := &Proxy{
		routes:           routes,
		ca:               cfg.CA,
		httpAddr:         httpAddr,
		httpsAddr:        httpsAddr,
		certCache:        make(map[string]*tls.Certificate),
		suffix:           suffix,
		transport:        &http.Transport{DialContext: cfg.DialContext, ForceAttemptHTTP2: false, MaxIdleConns: 64, MaxIdleConnsPerHost: 8, IdleConnTimeout: 30 * time.Second, ResponseHeaderTimeout: 15 * time.Second},
		issueCertificate: cfg.IssueCertificate,
	}
	return p, nil
}

// ServeHTTP implements http.Handler to dynamically reverse-proxy to target port.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}

	host = strings.TrimSuffix(strings.ToLower(host), ".")
	p.certCacheMu.RLock()
	route, found := p.routes[host]
	p.certCacheMu.RUnlock()
	if !found || (r.TLS != nil && !strings.EqualFold(strings.TrimSuffix(r.TLS.ServerName, "."), host)) {
		http.Error(w, "Unknown or mismatched Paperboat browser host", http.StatusMisdirectedRequest)
		return
	}
	targetIP, targetPort := route.Address, route.Port

	targetURL, err := url.Parse(fmt.Sprintf("http://%s:%d", targetIP.String(), targetPort))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	proxy := httputil.NewSingleHostReverseProxy(targetURL)
	proxy.Transport = p.transport
	proxy.Director = nil
	proxy.Rewrite = func(request *httputil.ProxyRequest) {
		request.SetURL(targetURL)
		request.Out.Host = request.In.Host
		// Rewrite strips caller-provided Forwarded/X-Forwarded-* metadata.
		// Recreate the chain from the actual local request and TLS state only.
		request.SetXForwarded()
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		http.Error(w, fmt.Sprintf("Paperboat Gateway Error dialing %s:%d: %v", targetIP.String(), targetPort, err), http.StatusBadGateway)
	}

	proxy.ServeHTTP(w, r)
}

// GetCertificate dynamically generates/retrieves leaf certificates signed by the local CA.
func (p *Proxy) GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	serverName := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(hello.ServerName)), ".")
	p.certCacheMu.RLock()
	_, permitted := p.routes[serverName]
	cert, ok := p.certCache[serverName]
	p.certCacheMu.RUnlock()
	if !permitted {
		return nil, errors.New("TLS name has no active browser route")
	}
	if ok && certificateUsable(cert, serverName, time.Now()) {
		return cert, nil
	}

	if p.ca == nil && p.issueCertificate == nil {
		return nil, errors.New("no CA configured for dynamic TLS")
	}

	p.certCacheMu.Lock()
	defer p.certCacheMu.Unlock()
	if _, permitted := p.routes[serverName]; !permitted {
		return nil, errors.New("TLS browser route was withdrawn")
	}
	if cert, ok := p.certCache[serverName]; ok && certificateUsable(cert, serverName, time.Now()) {
		return cert, nil
	}
	delete(p.certCache, serverName)
	if len(p.certCache) >= maxProxyCertificateCache {
		return nil, errors.New("dynamic TLS certificate cache is full")
	}

	domains := []string{serverName}

	var tlsCert tls.Certificate
	if p.issueCertificate != nil {
		var err error
		tlsCert, err = p.issueCertificate(hello.Context(), serverName)
		if err != nil {
			return nil, fmt.Errorf("issue certificate for %s: %w", serverName, err)
		}
	} else {
		certPEM, keyPEM, err := p.ca.IssueCertificate(domains)
		if err != nil {
			return nil, fmt.Errorf("issue certificate for %s: %w", serverName, err)
		}
		tlsCert, err = tls.X509KeyPair(certPEM, keyPEM)
		if err != nil {
			return nil, fmt.Errorf("load key pair for %s: %w", serverName, err)
		}
	}

	p.certCache[serverName] = &tlsCert
	return &tlsCert, nil
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
	if p.ca != nil || p.issueCertificate != nil {
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
// privileged device guard. Listener ownership remains with Proxy until Stop.
func (p *Proxy) StartListeners(httpListener, httpsListener net.Listener) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.started {
		return nil
	}
	if httpListener == nil || httpsListener == nil || (p.ca == nil && p.issueCertificate == nil) {
		return errors.New("splitdns proxy: protected HTTP/HTTPS listeners and CA are required")
	}
	return p.startListenersLocked(httpListener, httpsListener)
}

func (p *Proxy) startListenersLocked(httpListener, httpsListener net.Listener) error {
	p.httpListener = httpListener
	p.httpServer = &http.Server{Handler: p, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10}
	if httpsListener != nil {
		tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: p.GetCertificate}
		p.httpsListener = tls.NewListener(httpsListener, tlsConfig)
		p.httpsServer = &http.Server{Handler: p, TLSConfig: tlsConfig, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10}
	}
	p.started = true
	go func() { _ = p.httpServer.Serve(p.httpListener) }()
	if p.httpsServer != nil {
		go func() { _ = p.httpsServer.Serve(p.httpsListener) }()
	}
	return nil
}

func certificateUsable(cert *tls.Certificate, name string, now time.Time) bool {
	if cert == nil || len(cert.Certificate) == 0 {
		return false
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	return err == nil && now.Before(leaf.NotAfter.Add(-time.Hour)) && leaf.VerifyHostname(name) == nil
}

func (p *Proxy) SetSuffix(suffix string) error {
	clean, err := validateSuffix(suffix)
	if err != nil {
		return err
	}
	p.certCacheMu.Lock()
	p.suffix = clean
	p.routes = make(map[string]BrowserRoute)
	p.certCache = make(map[string]*tls.Certificate)
	p.certCacheMu.Unlock()
	return nil
}

// Stop stops the proxy servers.
func (p *Proxy) Stop(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.started {
		return nil
	}

	var errs []error
	if p.httpServer != nil {
		if err := p.httpServer.Shutdown(ctx); err != nil {
			_ = p.httpServer.Close()
			errs = append(errs, err)
		}
	}
	if p.httpsServer != nil {
		if err := p.httpsServer.Shutdown(ctx); err != nil {
			_ = p.httpsServer.Close()
			errs = append(errs, err)
		}
	}
	p.transport.CloseIdleConnections()
	p.started = false
	return errors.Join(errs...)
}

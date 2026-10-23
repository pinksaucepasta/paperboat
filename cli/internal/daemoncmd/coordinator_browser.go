package daemoncmd

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"github.com/pinksaucepasta/paperboat/internal/diagnosticlog"
	"github.com/pinksaucepasta/paperboat/internal/machineguard"
	"github.com/pinksaucepasta/paperboat/internal/splitdns"
)

// prepareBrowserGatewayLocked binds the public wildcard address once. Every
// dial is bound to an exact route from the authenticated service snapshot.
func (c *Coordinator) prepareBrowserGatewayLocked(ctx context.Context, client guardedNameClient, routes map[string]splitdns.BrowserRoute, aliases map[string]string) (web guardedWebRoute, err error) {
	base := splitdns.BrowserGatewayHostname
	if old, ok := c.webRoutes[base]; ok {
		if maps.Equal(old.browserRoutes, routes) {
			return old, nil
		}
		return c.updateBrowserGatewayLocked(ctx, client, old, routes, aliases)
	}
	byAddress, denied, err := browserGatewayRoutes(routes, aliases)
	if err != nil {
		return guardedWebRoute{}, err
	}
	ip := netip.MustParseAddr(splitdns.BrowserGatewayIP)
	httpListener, err := client.Acquire(ctx, base, ip, 80)
	if err != nil {
		return guardedWebRoute{}, coordinatorOperationFailure("browser_http_listener", err)
	}
	var httpsListener net.Listener
	defer func() {
		if err != nil {
			causes := []error{err, lifecycleCleanup(httpListener.Close())}
			if httpsListener != nil {
				causes = append(causes, lifecycleCleanup(httpsListener.Close()))
			}
			err = coordinatorFailure("local browser gateway could not initialize", causes...)
		}
	}()
	httpsListener, err = client.Acquire(ctx, base, ip, 443)
	if err != nil {
		err = coordinatorOperationFailure("browser_https_listener", err)
		return guardedWebRoute{}, err
	}
	if err := client.ReplaceAliases(ctx, aliases); err != nil {
		return guardedWebRoute{}, coordinatorOperationFailure("browser_aliases", err)
	}
	if err := c.issueBrowserRouteCertificates(ctx, client, routes, nil); err != nil {
		return guardedWebRoute{}, err
	}
	state := &browserGatewayState{}
	state.set(routes, byAddress, denied)
	proxy, err := splitdns.NewProxy(splitdns.ProxyConfig{DeniedHosts: denied, Domain: c.localDomain(), Routes: routes, IssueCertificate: func(certCtx context.Context, hostname string) (tls.Certificate, error) {
		current, _, deniedNow := state.get()
		if deniedNow[hostname] {
			return tls.Certificate{}, errors.New("configured browser alias destination is unavailable")
		}
		route, ok := browserRouteForHostDomain(current, hostname, c.localDomain())
		if !ok {
			return tls.Certificate{}, errors.New("unregistered browser certificate route")
		}
		return c.issueBrowserCertificate(certCtx, client, hostname, route.MachineID, route.Port)
	}, RevocationList: func(crlCtx context.Context, _ string) ([]byte, []byte, error) {
		issuer, ok := client.(interface {
			Certificate(context.Context, string) (machineguard.CertificateBundle, error)
		})
		if !ok {
			return nil, nil, errors.New("local certificate status service is unavailable")
		}
		current, _, _ := state.get()
		for name := range current {
			if strings.HasPrefix(name, "*.") {
				continue
			}
			bundle, err := issuer.Certificate(crlCtx, name)
			if err != nil {
				return nil, nil, err
			}
			root, rest := pem.Decode(bundle.RootCAPEM)
			if root == nil || root.Type != "CERTIFICATE" || len(strings.TrimSpace(string(rest))) != 0 {
				return nil, nil, errors.New("invalid local certificate issuer")
			}
			return root.Bytes, bundle.RevocationListDER, nil
		}
		return nil, nil, errors.New("no active browser routes")
	}, AwaitRoute: func(ctx context.Context, hostname string) bool {
		return c.awaitBrowserRoute(ctx, state, hostname)
	}, DialContext: func(ctx context.Context, _, address string) (net.Conn, error) {
		_, addresses, _ := state.get()
		machineID, ok := addresses[address]
		if !ok {
			return nil, errors.New("unregistered browser service address")
		}
		_, rawPort, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		port, err := strconv.Atoi(rawPort)
		if err != nil {
			return nil, err
		}
		return c.cfg.DialMachine(ctx, machineID, port)
	}})
	if err == nil {
		err = proxy.StartListeners(&limitedListener{Listener: httpListener, permits: c.connections, ctx: context.WithoutCancel(ctx)}, &limitedListener{Listener: httpsListener, permits: c.connections, ctx: context.WithoutCancel(ctx)})
	}
	if err != nil {
		return guardedWebRoute{}, coordinatorOperationFailure("browser_proxy", err)
	}
	web = guardedWebRoute{proxy: proxy, base: base, browserRoutes: routes, state: state, ports: map[int]bool{}}
	c.addBrowserPortListeners(ctx, client, ip, routes, proxy, web.ports)
	c.webRoutes[base] = web
	return web, nil
}

// updateBrowserGatewayLocked applies changed routes to the running gateway: certificates for new names
// first, then the routes, aliases and per-port listeners, without stopping the :80/:443 listeners. A
// rebuild left a just-started service unreachable (TLS internal error, connection refused) until the
// new gateway was up (Lane H: private-https to a fresh port).
func (c *Coordinator) updateBrowserGatewayLocked(ctx context.Context, client guardedNameClient, old guardedWebRoute, routes map[string]splitdns.BrowserRoute, aliases map[string]string) (guardedWebRoute, error) {
	byAddress, denied, err := browserGatewayRoutes(routes, aliases)
	if err != nil {
		return guardedWebRoute{}, err
	}
	if err := client.ReplaceAliases(ctx, aliases); err != nil {
		return guardedWebRoute{}, coordinatorOperationFailure("browser_aliases", err)
	}
	if err := c.issueBrowserRouteCertificates(ctx, client, routes, old.browserRoutes); err != nil {
		return guardedWebRoute{}, err
	}
	if err := old.proxy.SetRoutes(routes, denied); err != nil {
		return guardedWebRoute{}, coordinatorOperationFailure("browser_proxy", err)
	}
	old.state.set(routes, byAddress, denied)
	wanted := browserListenerPorts(routes)
	for port := range old.ports {
		if !wanted[port] {
			_ = old.proxy.RemovePortListener(port)
			delete(old.ports, port)
		}
	}
	c.addBrowserPortListeners(ctx, client, netip.MustParseAddr(splitdns.BrowserGatewayIP), routes, old.proxy, old.ports)
	old.browserRoutes = routes
	c.webRoutes[old.base] = old
	return old, nil
}

// browserGatewayRoutes validates routes and returns the dial index and the denied alias hostnames.
func browserGatewayRoutes(routes map[string]splitdns.BrowserRoute, aliases map[string]string) (map[string]string, map[string]bool, error) {
	byAddress := make(map[string]string, len(routes))
	for _, route := range routes {
		if route.MachineID == "" {
			return nil, nil, errors.New("browser service has no authenticated machine identity")
		}
		address := net.JoinHostPort(route.Address.String(), strconv.Itoa(route.Port))
		if prior, exists := byAddress[address]; exists && prior != route.MachineID {
			return nil, nil, errors.New("ambiguous browser service address")
		}
		byAddress[address] = route.MachineID
	}
	denied := make(map[string]bool)
	for name, target := range aliases {
		if target == "" {
			denied[name] = true
		}
	}
	return byAddress, denied, nil
}

// issueBrowserRouteCertificates issues the certificate of every routed name not in previous. The
// local certificate service knows a name only after its aliases are replaced, so this runs after
// ReplaceAliases (issuing first left the gateway without listeners, round 22).
func (c *Coordinator) issueBrowserRouteCertificates(ctx context.Context, client guardedNameClient, routes, previous map[string]splitdns.BrowserRoute) error {
	for name, route := range routes {
		if strings.HasPrefix(name, "*.") {
			continue
		}
		if known, ok := previous[name]; ok && known == route {
			continue
		}
		if _, err := c.issueBrowserCertificate(ctx, client, name, route.MachineID, route.Port); err != nil {
			return err
		}
	}
	return nil
}

// browserCertificateFailure reports whether the browser gateway could not start only because a
// browser certificate or its trust could not be installed.
func browserCertificateFailure(err error) bool {
	var operation coordinatorOperationError
	return errors.As(err, &operation) && operation.operation == "browser_certificate"
}

// observeBrowserTrustFailure records the failure once until browser URLs work again.
func (c *Coordinator) observeBrowserTrustFailure(ctx context.Context, err error) {
	c.reportMu.Lock()
	reported := c.browserTrustReported
	c.browserTrustReported = true
	c.reportMu.Unlock()
	if !reported {
		c.observe(ctx, "browser_certificate", err)
	}
	diagnosticlog.TryInfo("browser URLs are unavailable: the local browser certificate could not be installed; machine access is unaffected")
}

// addBrowserPortListeners makes https://<machine>.<domain>:<port> work for every published port
// of a machine by serving the gateway address on that port. A port that cannot be served (in use,
// or an older machine guard) is reported and skipped; it never degrades the other browser URLs.
// browserListenerPorts are the machine ports that get a per-port listener: https://<machine>:<port>.
func browserListenerPorts(routes map[string]splitdns.BrowserRoute) map[int]bool {
	ports := make(map[int]bool)
	for name, route := range routes {
		if strings.HasPrefix(name, "*.") || route.Port == 80 || route.Port == 443 || route.Port < 1024 {
			continue
		}
		ports[route.Port] = true
	}
	return ports
}

// addBrowserPortListeners starts a per-port listener for every routed port not in served, and records it.
func (c *Coordinator) addBrowserPortListeners(ctx context.Context, client guardedNameClient, ip netip.Addr, routes map[string]splitdns.BrowserRoute, proxy *splitdns.Proxy, served map[int]bool) {
	privileged := 0
	for name, route := range routes {
		if !strings.HasPrefix(name, "*.") && route.Port != 80 && route.Port != 443 && route.Port < 1024 {
			privileged++
		}
	}
	if privileged > 0 {
		diagnosticlog.TryInfo("browser port URLs skip privileged machine ports; use the numeric browser URL or pb access machine", "count", privileged)
	}
	for _, port := range slices.Sorted(maps.Keys(browserListenerPorts(routes))) {
		if served[port] {
			continue
		}
		listener, err := client.Acquire(ctx, splitdns.BrowserGatewayHostname, ip, port)
		if err != nil {
			diagnosticlog.TryInfo("browser port URL unavailable for a machine port", "port", port, "port_in_use", errors.Is(err, machineguard.ErrPortInUse))
			continue
		}
		limited := &limitedListener{Listener: listener, permits: c.connections, ctx: context.WithoutCancel(ctx)}
		if err := proxy.AddPortListener(port, limited); err != nil {
			_ = limited.Close()
			diagnosticlog.TryInfo("browser port URL could not start for a machine port", "port", port)
			continue
		}
		served[port] = true
	}
}

// BrowserURLs reports only currently protected and published browser routes.
func (c *Coordinator) BrowserURLs() map[string]map[int32]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	result := make(map[string]map[int32]string)
	for _, web := range c.webRoutes {
		for host, route := range web.browserRoutes {
			_, label, err := splitdns.ParseBrowserHostname(host, c.localDomain())
			if err != nil || label != strconv.Itoa(route.Port) {
				continue
			}
			if result[route.MachineID] == nil {
				result[route.MachineID] = make(map[int32]string)
			}
			result[route.MachineID][int32(route.Port)] = "https://" + host
		}
	}
	return result
}

// browserRouteForHostDomain admits a base machine URL only while one exact port
// route for that machine is registered. The base page never selects a forwarding port.
func browserRouteForHostDomain(routes map[string]splitdns.BrowserRoute, host, domain string) (splitdns.BrowserRoute, bool) {
	if route, ok := splitdns.ResolveBrowserRoute(routes, host, domain); ok {
		return route, true
	}
	for name, route := range routes {
		if strings.HasPrefix(name, "*.") {
			continue
		}
		if _, base, found := strings.Cut(name, "."); found && base == host {
			return route, true
		}
	}
	return splitdns.BrowserRoute{}, false
}

func (c *Coordinator) issueBrowserCertificate(ctx context.Context, client guardedNameClient, hostname, machineID string, port int) (returned tls.Certificate, resultErr error) {
	defer func() { resultErr = coordinatorOperationFailure("browser_certificate", resultErr) }()
	if c.cfg.IssueCertificate != nil {
		return c.cfg.IssueCertificate(ctx, hostname, machineID, port)
	}
	issuer, ok := client.(interface {
		Certificate(context.Context, string) (machineguard.CertificateBundle, error)
	})
	if !ok {
		return tls.Certificate{}, errors.New("local browser certificate service is unavailable; reinstall Paperboat")
	}
	bundle, err := issuer.Certificate(ctx, hostname)
	if err != nil {
		return tls.Certificate{}, err
	}
	rootBlock, rest := pem.Decode(bundle.RootCAPEM)
	if rootBlock == nil || rootBlock.Type != "CERTIFICATE" || len(strings.TrimSpace(string(rest))) != 0 {
		return tls.Certificate{}, errors.New("invalid local certificate issuer")
	}
	root, err := x509.ParseCertificate(rootBlock.Bytes)
	if err != nil || !root.IsCA || !root.PermittedDNSDomainsCritical || len(root.PermittedDNSDomains) != 1 || root.PermittedDNSDomains[0] != "."+c.localDomain() {
		return tls.Certificate{}, errors.New("local certificate issuer must be namespace constrained")
	}
	cert, err := tls.X509KeyPair(bundle.CertificatePEM, bundle.PrivateKeyPEM)
	if err != nil {
		return tls.Certificate{}, err
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return tls.Certificate{}, err
	}
	roots := x509.NewCertPool()
	roots.AddCert(root)
	if _, err = leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: hostname}); err != nil {
		return tls.Certificate{}, err
	}
	if err = machineguard.InstallUserTrust(ctx, bundle.RootCAPEM); err != nil {
		return tls.Certificate{}, fmt.Errorf("install browser certificate trust: %w", err)
	}
	return cert, nil
}

package daemoncmd

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"

	pbSync "github.com/pinksaucepasta/paperboat/internal/controlsync/proto"
	"github.com/pinksaucepasta/paperboat/internal/diagnosticlog"
	"github.com/pinksaucepasta/paperboat/internal/machineguard"
	"github.com/pinksaucepasta/paperboat/internal/machineloopback"
	"github.com/pinksaucepasta/paperboat/internal/splitdns"
	"google.golang.org/protobuf/proto"
)

func mapPeerSnapshot(peers []*pbSync.PeerUpdate, cidr string) ([]*pbSync.PeerUpdate, error) {
	if cidr == "" {
		cidr = machineloopback.DefaultCIDR
	}
	mapped := make([]*pbSync.PeerUpdate, len(peers))
	for index, peer := range peers {
		if peer == nil {
			continue
		}
		copyPeer := proto.Clone(peer).(*pbSync.PeerUpdate)
		if strings.TrimSpace(peer.GetAssignedIp()) != "" {
			canonical, err := netip.ParseAddr(peer.GetAssignedIp())
			if err != nil {
				return nil, coordinatorFailure("machine snapshot contains an invalid assigned address", err)
			}
			local, err := machineloopback.MapCanonical(canonical, cidr)
			if err != nil {
				return nil, coordinatorFailure("machine snapshot address could not be mapped", err)
			}
			copyPeer.AssignedIp = local.String()
		}
		mapped[index] = copyPeer
	}
	return mapped, nil
}

// localDomain is the domain this computer's local access names live under.
func (c *Coordinator) localDomain() string {
	if domain := c.domain.Load(); domain != nil {
		return *domain
	}
	return ""
}

// resolveLocalDomainLocked reads the local access domain from the machine guard,
// the only owner of it, on every reconcile: a reinstall or a domain change is
// followed without restarting the daemon, and no stale copy can disagree with
// the guard's own name validation.
func (c *Coordinator) resolveLocalDomainLocked(ctx context.Context, nameClient guardedNameClient) error {
	status, err := nameClient.Status(ctx)
	if err != nil {
		return coordinatorOperationFailure("guard_status", err)
	}
	domain, err := splitdns.NormalizeBrowserDomain(status.BrowserDomain)
	if err != nil {
		return coordinatorFailure("This computer has no local access domain; run pb install again.", err)
	}
	c.domain.Store(&domain)
	return nil
}

func (c *Coordinator) replaceGuardedRoutes(ctx context.Context, peers []*pbSync.PeerUpdate) (err error) {
	c.mu.Lock()
	defer func() {
		err = coordinatorFailure("local machine access reconciliation failed", err, c.completedCleanupLocked())
		c.mu.Unlock()
	}()
	if c.ctx == nil {
		return errors.New("coordinator is not running")
	}
	nameClient := c.nameClient
	if nameClient == nil {
		if c.cfg.ConnectNameClient == nil {
			return errors.New("protected machine-name service is unavailable")
		}
		connected, err := c.cfg.ConnectNameClient(ctx)
		if err != nil {
			return coordinatorOperationFailure("guard_connect", err)
		}
		c.nameClient = connected
		nameClient = connected
	}
	if err := c.resolveLocalDomainLocked(ctx, nameClient); err != nil {
		// A guard that restarted (an update or self-heal replaced it) leaves this connection dead;
		// the next reconcile connects again instead of failing on it forever.
		c.resetNameGenerationLocked(ctx)
		return err
	}
	wanted := make(map[string]*guardedRoute)
	wantedWeb := make(map[string]guardedWebRoute)
	names := make(map[string]netip.Addr)
	aliases := make(map[string]string)
	browserRoutes := make(map[string]splitdns.BrowserRoute)
	conflictedPorts, firstConflictedPort := 0, 0
	for _, peer := range peers {
		if peer == nil || !peer.GetApproved() || !peer.GetOnline() || peer.GetAlias() == "" {
			continue
		}
		alias := peer.GetAlias()
		ip, err := netip.ParseAddr(peer.GetAssignedIp())
		if err != nil {
			continue
		}
		hostname := strings.ToLower(alias) + "." + c.localDomain()
		if prior, exists := names[hostname]; exists && prior != ip {
			c.resetNameGenerationLocked(ctx)
			return errors.New("control snapshot contains an ambiguous machine alias")
		}
		ports := peer.GetExportedPorts()
		if len(ports) == 0 {
			continue
		}
		_, localCertificates := nameClient.(interface {
			Certificate(context.Context, string) (machineguard.CertificateBundle, error)
		})
		browserReady := c.cfg.IssueCertificate != nil || localCertificates
		if browserReady {
			proxyPort := 80
			for _, machineProxy := range c.cfg.LocalAccess.MachineProxies {
				if machineProxy.MachineAlias == strings.ToLower(alias) {
					proxyPort = int(machineProxy.Port)
					break
				}
			}
			for _, raw := range ports {
				port := int(raw)
				name, err := splitdns.BrowserHostname(alias, port, c.localDomain())
				if err != nil {
					c.resetNameGenerationLocked(ctx)
					return err
				}
				if _, exists := browserRoutes[name]; exists {
					c.resetNameGenerationLocked(ctx)
					return errors.New("ambiguous browser hostname")
				}
				browserRoutes[name] = splitdns.BrowserRoute{Address: ip, Port: port, MachineID: peer.GetPeerId()}
				aliases[name] = splitdns.BrowserGatewayHostname
				if len(aliases) > machineguard.MaxBrowserAliases {
					c.resetNameGenerationLocked(ctx)
					return fmt.Errorf("local browser routes exceed the limit of %d; remove service aliases or exported ports", machineguard.MaxBrowserAliases)
				}
				if port == proxyPort {
					pattern, err := splitdns.BrowserWildcardPattern(strings.ToLower(alias), c.localDomain())
					if err != nil {
						c.resetNameGenerationLocked(ctx)
						return err
					}
					browserRoutes[pattern] = browserRoutes[name]
					aliases[pattern] = name
					if len(aliases) > machineguard.MaxBrowserAliases {
						c.resetNameGenerationLocked(ctx)
						return errors.New("local browser routes exceed the alias limit")
					}
				}
				for _, serviceAlias := range c.cfg.LocalAccess.ServiceAliases {
					if serviceAlias.MachineAlias != strings.ToLower(alias) || int(serviceAlias.Port) != port {
						continue
					}
					aliasName := serviceAlias.Name + "." + strings.ToLower(alias) + "." + c.localDomain()
					_, label, err := splitdns.ParseBrowserHostname(aliasName, c.localDomain())
					if err != nil || label == "" {
						c.resetNameGenerationLocked(ctx)
						return errors.New("invalid configured browser alias")
					}
					if _, exists := browserRoutes[aliasName]; exists {
						c.resetNameGenerationLocked(ctx)
						return errors.New("ambiguous browser service alias")
					}
					browserRoutes[aliasName] = browserRoutes[name]
					aliases[aliasName] = name
					if len(aliases) > machineguard.MaxBrowserAliases {
						c.resetNameGenerationLocked(ctx)
						return fmt.Errorf("local browser routes exceed the limit of %d; remove service aliases or exported ports", machineguard.MaxBrowserAliases)
					}
				}
			}

			for _, serviceAlias := range c.cfg.LocalAccess.ServiceAliases {
				if serviceAlias.MachineAlias != strings.ToLower(alias) {
					continue
				}
				aliasName := serviceAlias.Name + "." + strings.ToLower(alias) + "." + c.localDomain()
				if _, active := browserRoutes[aliasName]; active {
					continue
				}
				if _, proxy := aliases["*."+strings.ToLower(alias)+"."+c.localDomain()]; proxy {
					aliases[aliasName] = ""
					if len(aliases) > machineguard.MaxBrowserAliases {
						c.resetNameGenerationLocked(ctx)
						return errors.New("local browser routes exceed the alias limit")
					}
				}
			}
		}
		for _, raw := range ports {
			port := int(raw)
			if port < 1 || port > 65535 {
				continue
			}
			key := peer.GetPeerId() + "\x00" + hostname + "\x00" + ip.String() + "\x00" + strconv.Itoa(port)
			if route, ok := c.routes[key]; ok {
				wanted[key], names[hostname] = route, ip
				continue
			}
			listener, err := nameClient.Acquire(ctx, hostname, ip, port)
			if err != nil {
				if errors.Is(err, machineguard.ErrPortInUse) || errors.Is(err, machineguard.ErrListenerRefused) {
					// The guard's default deny remains in force without a lease. Another program's port or
					// another OS user's name or address refuses this one listener only: keep independent
					// routes and retry this port on reconciliation.
					conflictedPorts++
					if firstConflictedPort == 0 {
						firstConflictedPort = port
					}
					continue
				}
				c.resetNameGenerationLocked(ctx)
				return coordinatorOperationFailure("native_listener", err)
			}
			routeCtx, routeCancel := context.WithCancel(c.ctx)
			route := &guardedRoute{listener: listener, machineID: peer.GetPeerId(), port: port, ctx: routeCtx, cancel: routeCancel}
			wanted[key], names[hostname] = route, ip
			c.routes[key] = route
			c.wg.Add(1)
			go c.serveRoute(route)
		}
	}
	if conflictedPorts > 0 {
		diagnosticlog.TryInfo("protected machine ports remain blocked by local listener conflicts", "count", conflictedPorts, "first_port", firstConflictedPort)
	}
	if len(browserRoutes) > 0 {
		web, err := c.prepareBrowserGatewayLocked(ctx, nameClient, browserRoutes, aliases)
		switch {
		case errors.Is(err, machineguard.ErrPortInUse), errors.Is(err, machineguard.ErrListenerRefused):
			// Another program (a web server on :80/:443) owns a gateway port.
			// Machine names and native ports keep working; only browser URLs
			// are withdrawn, so a busy port must not degrade the whole daemon.
			diagnosticlog.TryInfo("browser gateway ports are in use by another program; browser URLs are unavailable")
			aliases = map[string]string{}
		case browserCertificateFailure(err):
			// Browser trust (for example NSS certutil on Linux) is a prerequisite of browser URLs
			// only: native machine access and names keep working without them.
			c.observeBrowserTrustFailure(ctx, err)
			aliases = map[string]string{}
		case err != nil:
			c.resetNameGenerationLocked(ctx)
			return err
		default:
			wantedWeb[splitdns.BrowserGatewayHostname] = web
		}
	}
	if err := nameClient.ReplaceAliases(ctx, aliases); err != nil {
		c.resetNameGenerationLocked(ctx)
		return coordinatorOperationFailure("browser_aliases", err)
	}
	if err := nameClient.ReplaceNames(ctx, names); err != nil {
		c.resetNameGenerationLocked(ctx)
		return coordinatorOperationFailure("machine_names", err)
	}
	var removedRoutes []*guardedRoute
	for key, route := range c.routes {
		if _, ok := wanted[key]; !ok {
			removedRoutes = append(removedRoutes, route)
		}
	}
	var removedWeb []guardedWebRoute
	for key, route := range c.webRoutes {
		if _, ok := wantedWeb[key]; !ok {
			removedWeb = append(removedWeb, route)
		}
	}
	cleanup := c.launchCleanupLocked(func() error {
		var causes []error
		for _, route := range removedRoutes {
			causes = append(causes, route.close())
		}
		for _, route := range removedWeb {
			causes = append(causes, lifecycleCleanup(route.proxy.Stop(ctx)))
		}
		return coordinatorFailure("removed local routes could not close", causes...)
	})
	select {
	case <-cleanup.done:
	case <-ctx.Done():
		c.resetNameGenerationLocked(ctx)
		return errors.Join(ctx.Err(), context.Cause(ctx))
	}

	c.routes = wanted
	c.webRoutes = wantedWeb
	return nil
}

func (c *Coordinator) withdrawGuardedLocked() {
	ctx := c.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	c.resetNameGenerationLocked(ctx)
}

func (c *Coordinator) resetNameGenerationLocked(ctx context.Context) {
	client := c.nameClient
	c.nameClient = nil
	routes := c.routes
	webRoutes := c.webRoutes
	c.routes = make(map[string]*guardedRoute)
	c.webRoutes = make(map[string]guardedWebRoute)
	for _, route := range routes {
		route.cancelNow()
	}
	cleanup := c.launchCleanupLocked(func() error {
		var causes []error
		// Close the old authority first; its releases cannot affect a replacement.
		if client != nil {
			causes = append(causes, lifecycleCleanup(client.Close()))
		}
		for _, route := range routes {
			causes = append(causes, route.close())
		}
		for _, route := range webRoutes {
			causes = append(causes, lifecycleCleanup(route.proxy.Stop(ctx)))
		}
		return coordinatorFailure("local access generation could not close", causes...)
	})
	select {
	case <-cleanup.done:
	case <-ctx.Done():
	}

}

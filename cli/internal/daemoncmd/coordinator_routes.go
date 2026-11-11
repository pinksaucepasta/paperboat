package daemoncmd

import (
	"context"
	"errors"
	"fmt"
	"github.com/pinksaucepasta/paperboat/internal/daemonrpc"
	"net/netip"
	"strconv"
	"strings"

	"github.com/pinksaucepasta/paperboat/internal/machineguard"
	"github.com/pinksaucepasta/paperboat/internal/machineloopback"
	"github.com/pinksaucepasta/paperboat/internal/splitdns"
	"github.com/pinksaucepasta/paperboat/shared/diagnosticlog"
)

// mapPeerSnapshot maps every peer's address into this computer's loopback range. A peer whose address is invalid or
// cannot be mapped is left out and reported in the returned error; the other peers keep their access (one bad entry
// used to withdraw every route).
func mapPeerSnapshot(peers []*daemonrpc.PeerUpdate, cidr string) ([]*daemonrpc.PeerUpdate, error) {
	if cidr == "" {
		cidr = machineloopback.DefaultCIDR
	}
	mapped := make([]*daemonrpc.PeerUpdate, 0, len(peers))
	var skipped []error
	for _, peer := range peers {
		if peer == nil {
			continue
		}
		copyPeer := peer.Clone()
		if strings.TrimSpace(peer.GetAssignedIp()) != "" {
			canonical, err := netip.ParseAddr(peer.GetAssignedIp())
			if err != nil {
				skipped = append(skipped, coordinatorFailure("machine snapshot contains an invalid assigned address", err))
				continue
			}
			local, err := machineloopback.MapCanonical(canonical, cidr)
			if err != nil {
				skipped = append(skipped, coordinatorFailure("machine snapshot address could not be mapped", err))
				continue
			}
			copyPeer.AssignedIp = local.String()
		}
		mapped = append(mapped, copyPeer)
	}
	return mapped, errors.Join(skipped...)
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

func (c *Coordinator) replaceGuardedRoutes(ctx context.Context, peers []*daemonrpc.PeerUpdate) (err error) {
	c.mu.Lock()
	defer func() {
		err = coordinatorFailure("local machine access reconciliation failed", err, c.completedCleanupLocked())
		c.mu.Unlock()
	}()
	if c.ctx == nil {
		return errors.New("coordinator is not running")
	}
	// Withdrawal first: forwards kept across a reset are cut as soon as a snapshot no longer authorizes their
	// machine and port, whether or not the guard can be reached to rebuild the listeners below.
	c.cutWithdrawnDrainingLocked(ctx, peers)
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
				wanted[key], names[hostname] = route, route.address
				continue
			}
			listener, assigned, err := nameClient.AcquireMachine(ctx, c.cfg.AccountID, peer.GetPeerId(), hostname, ip, port)
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
			route := &guardedRoute{listener: listener, address: assigned, machineID: peer.GetPeerId(), port: port, ctx: routeCtx, cancel: routeCancel}
			wanted[key], names[hostname] = route, assigned
			c.routes[key] = route
			c.wg.Add(1)
			go c.serveRoute(route)
		}
	}
	if conflictedPorts > 0 {
		diagnosticlog.TryInfo("protected machine ports remain blocked by local listener conflicts", "count", conflictedPorts, "first_port", firstConflictedPort)
	}
	c.browserPortsPending = false // set again below by every per-port listener that cannot start
	c.browserGatewayBusy, c.browserGatewayOwner = false, ""
	if len(browserRoutes) > 0 {
		web, err := c.prepareBrowserGatewayLocked(ctx, nameClient, browserRoutes, aliases)
		switch {
		case errors.Is(err, machineguard.ErrPortInUse), errors.Is(err, machineguard.ErrListenerRefused):
			// Another program (a web server on :80/:443) owns a gateway port.
			// Machine names and native ports keep working; only browser URLs
			// are withdrawn, so a busy port must not degrade the whole daemon.
			c.browserGatewayBusy, c.browserGatewayOwner = true, browserGatewayPortOwner()
			diagnosticlog.TryWarn("browser gateway ports are in use by another program; browser URLs are unavailable until it stops", "owner", c.browserGatewayOwner)
			aliases = map[string]string{}
		case browserCertificateFailure(err):
			// Browser trust (for example NSS certutil on Linux) is a prerequisite of browser URLs
			// only: native machine access and names keep working without them.
			c.observeBrowserTrustFailure(ctx, err)
			c.browserPending = true
			aliases = map[string]string{}
		case err != nil:
			c.resetNameGenerationLocked(ctx)
			return err
		default:
			c.browserPending = false
			wantedWeb[splitdns.BrowserGatewayHostname] = web
		}
	} else {
		c.browserPending = false
	}
	// One policy transaction for every listener acquired above (one WFP Apply for N peers on Windows; victus
	// with ~20 peers timed out applying one per acquire). Until it lands the new addresses stay denied.
	if err := nameClient.ApplyPolicy(ctx); err != nil {
		c.resetNameGenerationLocked(ctx)
		return coordinatorOperationFailure("native_listener", err)
	}
	if err := nameClient.ReplaceAliases(ctx, aliases); err != nil {
		c.resetNameGenerationLocked(ctx)
		return coordinatorOperationFailure("browser_aliases", err)
	}
	if err := nameClient.ReplaceNames(ctx, names); err != nil && !errors.Is(err, machineguard.ErrNamesRefused) {
		c.resetNameGenerationLocked(ctx)
		return coordinatorOperationFailure("machine_names", err)
	} else {
		// The guard published every other name: a refused name degrades only that item (multi-account hosts);
		// it is retried and pb doctor names the step meanwhile.
		c.namesRefused = err != nil
		if err != nil {
			diagnosticlog.TryWarn("some machine names were not published; the others work", "error", err)
		}
	}
	// Claims are released only against the last complete inventory read (every machine, online or not), never
	// against the routable peers: an offline machine keeps its address, and a withdrawal (expired services, a
	// rejected snapshot, an outage) releases nothing. The guard holds released claims 7 days and refuses an
	// empty set while claims exist.
	if machines := c.claimMachines; c.cfg.AccountID != "" && len(machines) > 0 {
		if err := nameClient.ReplaceClaims(ctx, c.cfg.AccountID, machines); err != nil {
			diagnosticlog.TryWarn("local machine address claims were not updated; the guard keeps them", "error", err)
		}
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
	// A reset (guard, alias or name error) stops accepting but keeps established forwards: the next
	// successful reconcile cuts those of machines that are gone, and Stop cuts all.
	for _, route := range routes {
		route.markDraining()
		c.draining = append(c.draining, route)
	}
	cleanup := c.launchCleanupLocked(func() error {
		var causes []error
		// Close the old authority first; its releases cannot affect a replacement.
		if client != nil {
			causes = append(causes, lifecycleCleanup(client.Close()))
		}
		for _, route := range routes {
			causes = append(causes, route.stopAccepting())
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

// cutWithdrawnDrainingLocked closes the draining routes the snapshot no longer authorizes (their machine and
// port, with the same rules that build the listeners) and those whose forwards have all ended.
func (c *Coordinator) cutWithdrawnDrainingLocked(ctx context.Context, peers []*daemonrpc.PeerUpdate) {
	if len(c.draining) == 0 {
		return
	}
	authorized := make(map[string]bool)
	for _, peer := range peers {
		if peer == nil || !peer.GetApproved() || !peer.GetOnline() || peer.GetAlias() == "" {
			continue
		}
		if _, err := netip.ParseAddr(peer.GetAssignedIp()); err != nil {
			continue
		}
		for _, port := range peer.GetExportedPorts() {
			authorized[peer.GetPeerId()+"\x00"+strconv.Itoa(int(port))] = true
		}
	}
	var cut []*guardedRoute
	kept := c.draining[:0]
	for _, route := range c.draining {
		if route.idle() || !authorized[route.machineID+"\x00"+strconv.Itoa(route.port)] {
			cut = append(cut, route)
		} else {
			kept = append(kept, route)
		}
	}
	c.draining = kept
	if len(cut) == 0 {
		return
	}
	cleanup := c.launchCleanupLocked(func() error {
		var causes []error
		for _, route := range cut {
			causes = append(causes, route.close())
		}
		return coordinatorFailure("withdrawn local forwards could not close", causes...)
	})
	select {
	case <-cleanup.done:
	case <-ctx.Done():
	}
}

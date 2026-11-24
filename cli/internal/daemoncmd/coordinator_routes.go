package daemoncmd

import (
	"context"
	"errors"
	"github.com/pinksaucepasta/paperboat/cli/internal/daemonrpc"
	"io"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"syscall"

	"github.com/pinksaucepasta/paperboat/cli/internal/machineguard"
	"github.com/pinksaucepasta/paperboat/cli/internal/machineloopback"
	"github.com/pinksaucepasta/paperboat/cli/internal/splitdns"
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

func (c *Coordinator) connectGuardLocked(ctx context.Context) (guardedNameClient, error) {
	if c.nameClient != nil {
		return c.nameClient, nil
	}
	if c.cfg.ConnectNameClient == nil {
		return nil, errors.New("protected machine-name service is unavailable")
	}
	connectCtx, cancel := context.WithTimeout(ctx, c.cfg.ReconcileTimeout)
	defer cancel()
	connected, err := c.cfg.ConnectNameClient(connectCtx)
	if err != nil {
		return nil, coordinatorOperationFailure("guard_connect", err)
	}
	c.nameClient = connected
	return connected, nil
}

// transientGuardFailure is a guard that stopped answering (timeout or closed connection), as when it restarts;
// a refusal or the reconcile's own cancellation is not retried. Prior art: netbird
// client/internal/routemanager/systemops/systemops_bsd.go retryFetchRIB retries only a named transient error.
func transientGuardFailure(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return false
	}
	for _, disconnect := range guardDisconnectErrors {
		if errors.Is(err, disconnect) {
			return true
		}
	}
	var netErr net.Error
	return errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, net.ErrClosed) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) ||
		(errors.As(err, &netErr) && netErr.Timeout())
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
	err = c.replaceGuardedRoutesLocked(ctx, peers)
	// A guard that restarted (an update or self-heal replaced it) or answered too slowly leaves this connection
	// dead (the failed step dropped it): on a timeout or closed connection, dial once more within this reconcile.
	var op coordinatorOperationError
	if err != nil && c.cfg.ConnectNameClient != nil && transientGuardFailure(ctx, err) && errors.As(err, &op) &&
		(op.LocalAccessStep() == "guard_status" || op.LocalAccessStep() == "native_listener") {
		diagnosticlog.TryWarn("machine guard did not answer; dialing it again", "error", err)
		// The failed attempt spent most of ctx's budget: the retry gets a full one of its own, ended by Stop.
		// The retry is ended by Stop, and by the caller's deadline only when there is one (tests).
		retryCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		defer cancel()
		defer context.AfterFunc(c.ctx, cancel)()
		c.resetNameGenerationLocked(retryCtx)
		err = c.replaceGuardedRoutesLocked(retryCtx, peers)
	}
	return err
}

// errSuperseded ends a reconcile between two guard calls because a different snapshot waits; what is held stays, and
// the next reconcile first withdraws what that snapshot does not authorize.
var errSuperseded = errors.New("newer machine list received during setup")

func routeKey(machineID, hostname string, ip netip.Addr, port int) string {
	return machineID + "\x00" + hostname + "\x00" + ip.String() + "\x00" + strconv.Itoa(port)
}

// withdrawUnauthorizedLocked closes every held listener peers does not authorize exactly, and the browser gateway if
// any of its URLs points at something peers does not authorize. The rest keeps its guard connection.
func (c *Coordinator) withdrawUnauthorizedLocked(peers []*daemonrpc.PeerUpdate) {
	listeners, targets, hosts := make(map[string]bool), make(map[string]bool), make(map[string]string)
	for _, peer := range peers {
		if peer == nil || !peer.GetApproved() || !peer.GetOnline() || peer.GetAlias() == "" {
			continue
		}
		ip, err := netip.ParseAddr(peer.GetAssignedIp())
		if err != nil {
			continue
		}
		hostname := strings.ToLower(peer.GetAlias()) + "." + c.localDomain()
		for _, raw := range peer.GetExportedPorts() {
			port, target := int(raw), routeKey(peer.GetPeerId(), "", ip, int(raw))
			listeners[routeKey(peer.GetPeerId(), hostname, ip, port)] = true
			targets[target] = true
			if host, err := splitdns.BrowserHostname(peer.GetAlias(), port, c.localDomain()); err == nil {
				hosts[host] = target
			}
		}
	}
	var routes []*guardedRoute
	for key, route := range c.routes {
		if !listeners[key] {
			routes = append(routes, route)
			delete(c.routes, key)
		}
	}
	var gateways []guardedWebRoute
	for base, web := range c.webRoutes {
		for host, route := range web.browserRoutes {
			target := routeKey(route.MachineID, "", route.Address, route.Port)
			_, label, err := splitdns.ParseBrowserHostname(strings.TrimPrefix(host, "*."), c.localDomain())
			numeric := err == nil && label == strconv.Itoa(route.Port)
			if !targets[target] || numeric && hosts[host] != target {
				gateways = append(gateways, web)
				delete(c.webRoutes, base)
				break
			}
		}
	}
	if len(routes)+len(gateways) == 0 {
		return
	}
	stopCtx, cancel := context.WithTimeout(c.ctx, c.cfg.ReconcileTimeout)
	cleanup := c.launchCleanupLocked(func() error {
		defer cancel()
		var causes []error
		for _, web := range gateways {
			causes = append(causes, lifecycleCleanup(web.proxy.Stop(stopCtx)))
		}
		for _, route := range routes {
			causes = append(causes, route.close())
		}
		return coordinatorFailure("withdrawn local routes could not close", causes...)
	})
	c.awaitCleanup(c.ctx, cleanup)
}

// awaitCleanup waits for cleanup, at most ReconcileTimeout: a reconcile itself has no deadline.
func (c *Coordinator) awaitCleanup(ctx context.Context, cleanup *generationCleanup) bool {
	ctx, cancel := context.WithTimeout(ctx, c.cfg.ReconcileTimeout)
	defer cancel()
	select {
	case <-cleanup.done:
		return true
	case <-ctx.Done():
		return false
	}
}

func (c *Coordinator) replaceGuardedRoutesLocked(ctx context.Context, peers []*daemonrpc.PeerUpdate) error {
	// Withdrawal first: forwards kept across a reset are cut as soon as a snapshot no longer authorizes their
	// machine and port, whether or not the guard can be reached to rebuild the listeners below.
	c.cutWithdrawnDrainingLocked(ctx, peers)
	client, err := c.connectGuardLocked(ctx)
	if err != nil {
		return err
	}
	nameClient := boundGuard(client, c.cfg.ReconcileTimeout)
	if err := c.resolveLocalDomainLocked(ctx, nameClient); err != nil {
		c.resetNameGenerationLocked(ctx)
		return err
	}
	if c.setupPartial {
		c.withdrawUnauthorizedLocked(peers)
	}
	c.setupPartial = true
	wanted := make(map[string]*guardedRoute)
	wantedWeb := make(map[string]guardedWebRoute)
	names := make(map[string]netip.Addr)
	aliasSet := browserAliasSet{names: make(map[string]string)}
	aliases, aliasesRefused := aliasSet.names, false
	browserRoutes := make(map[string]splitdns.BrowserRoute)
	conflictedPorts, firstConflictedPort := 0, 0
	peers, skippedMachines, skippedPorts := routableWithinLimits(peers)
	var topLevel [][2]string // top-level name, numeric hostname of its port
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
			// Names come from the server's service list, for listed ports only: a name grants no access.
			listed := c.portNames[peer.GetPeerId()]
			frontDesk := listed.FrontDesk.Port
			if frontDesk == 0 {
				frontDesk = 80
			}
			lower := strings.ToLower(alias)
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
				if !aliasSet.add(name, splitdns.BrowserGatewayHostname) {
					continue
				}
				browserRoutes[name] = splitdns.BrowserRoute{Address: ip, Port: port, MachineID: peer.GetPeerId(), HTTPS: port == 443}
				if port == frontDesk {
					pattern, err := splitdns.BrowserWildcardPattern(lower, c.localDomain())
					if err != nil {
						c.resetNameGenerationLocked(ctx)
						return err
					}
					if aliasSet.add(pattern, name) {
						desk := browserRoutes[name]
						desk.HTTPS = listed.FrontDesk.Scheme == "https" || listed.FrontDesk.Scheme == "" && port == 443
						browserRoutes[pattern] = desk
					}
				}
				for _, named := range listed.PortNames {
					if named.Port != port {
						continue
					}
					namedHost := named.Name + "." + lower + "." + c.localDomain()
					if _, label, err := splitdns.ParseBrowserHostname(namedHost, c.localDomain()); err != nil || label == "" || splitdns.NumericBrowserLabel(label) {
						continue // the server validates names; a name this computer cannot use is left out alone
					}
					if _, exists := browserRoutes[namedHost]; !exists && aliasSet.add(namedHost, name) {
						browserRoutes[namedHost] = browserRoutes[name]
					}
				}
				for _, named := range listed.TopLevelNames {
					if named.Port == port {
						topLevel = append(topLevel, [2]string{named.Name, name})
					}
				}
			}
		}
		for _, raw := range ports {
			port := int(raw)
			if port < 1 || port > 65535 {
				continue
			}
			key := routeKey(peer.GetPeerId(), hostname, ip, port)
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
			if c.superseded() {
				return coordinatorOperationFailure("native_listener", errSuperseded)
			}
		}
	}
	// <name>.<domain> opens the named port unless a machine has that name: the machine wins.
	for _, named := range topLevel {
		host := named[0] + "." + c.localDomain()
		if _, exists := browserRoutes[host]; exists || c.machineAliases[named[0]] || named[0] == "gateway" {
			continue
		}
		if aliasSet.add(host, named[1]) {
			browserRoutes[host] = browserRoutes[named[1]]
		}
	}
	if detail := overLimitDetail(skippedMachines, skippedPorts, aliasSet.skipped); detail != c.overLimit {
		c.overLimit = detail
		if detail != "" {
			diagnosticlog.TryWarn("this account is above this computer's local access limits; the rest is served", "not_served", detail)
		}
	}
	if conflictedPorts > 0 {
		diagnosticlog.TryInfo("protected machine ports remain blocked by local listener conflicts", "count", conflictedPorts, "first_port", firstConflictedPort)
	}
	c.browserPortsPending = false // set again below by every per-port listener that cannot start
	c.browserGatewayBusy, c.browserHTTPBusy, c.browserGatewayOwner = false, false, ""
	if len(browserRoutes) > 0 {
		web, err := c.prepareBrowserGatewayLocked(ctx, nameClient, browserRoutes, aliases)
		switch {
		case errors.Is(err, machineguard.ErrPortInUse), errors.Is(err, machineguard.ErrListenerRefused):
			// Another program (a web server on :443; :80 alone only sets browserHTTPBusy) owns a gateway port.
			// Machine names and native ports keep working; only browser URLs
			// are withdrawn, so a busy port must not degrade the whole daemon.
			c.browserGatewayBusy, c.browserGatewayOwner = true, browserGatewayPortOwner()
			diagnosticlog.TryWarn("browser gateway ports are in use by another program; browser URLs are unavailable until it stops", "owner", c.browserGatewayOwner)
			aliases = map[string]string{}
		case errors.Is(err, machineguard.ErrNamesRefused):
			// This OS user's browser names do not fit the guard: native access and machine names keep working.
			diagnosticlog.TryWarn("browser names were not published; machine access is unaffected", "error", err)
			aliases, aliasesRefused = map[string]string{}, true
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
	if c.superseded() {
		return coordinatorOperationFailure("native_listener", errSuperseded)
	}
	// One policy transaction for every listener acquired above (one WFP Apply for N peers on Windows; Windows test machine
	// with ~20 peers timed out applying one per acquire). Until it lands the new addresses stay denied.
	if err := nameClient.ApplyPolicy(ctx); err != nil {
		c.resetNameGenerationLocked(ctx)
		return coordinatorOperationFailure("native_listener", err)
	}
	if err := nameClient.ReplaceAliases(ctx, aliases); errors.Is(err, machineguard.ErrNamesRefused) {
		aliasesRefused = true
	} else if err != nil {
		c.resetNameGenerationLocked(ctx)
		return coordinatorOperationFailure("browser_aliases", err)
	}
	if err := nameClient.ReplaceNames(ctx, names); err != nil && !errors.Is(err, machineguard.ErrNamesRefused) {
		c.resetNameGenerationLocked(ctx)
		return coordinatorOperationFailure("machine_names", err)
	} else {
		// The guard published every other name: a refused name degrades only that item (multi-account hosts);
		// it is retried and pb doctor names the step meanwhile.
		c.namesRefused = err != nil || aliasesRefused
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
		// One policy change for all of them; each close below then finds its lease already gone. A change per
		// listener took 350 ms each at 1000 machines, so a few machines leaving outlasted the reconcile.
		if batch, ok := client.(interface {
			ReleaseMachines(context.Context, []netip.AddrPort) error
		}); ok && len(removedRoutes) > 1 {
			var listeners []netip.AddrPort
			for _, route := range removedRoutes {
				listeners = append(listeners, netip.AddrPortFrom(route.address, uint16(route.port)))
			}
			causes = append(causes, lifecycleCleanup(batch.ReleaseMachines(ctx, listeners)))
		}
		for _, route := range removedRoutes {
			causes = append(causes, route.close())
		}
		for _, route := range removedWeb {
			causes = append(causes, lifecycleCleanup(route.proxy.Stop(ctx)))
		}
		return coordinatorFailure("removed local routes could not close", causes...)
	})
	if !c.awaitCleanup(ctx, cleanup) {
		c.resetNameGenerationLocked(ctx)
		return errors.Join(context.DeadlineExceeded, context.Cause(ctx))
	}

	c.routes = wanted
	c.webRoutes = wantedWeb
	c.setupPartial = false
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
	c.awaitCleanup(ctx, cleanup)

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
	c.awaitCleanup(ctx, cleanup)
}

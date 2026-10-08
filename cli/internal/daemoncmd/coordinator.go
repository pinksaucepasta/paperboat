package daemoncmd

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/buildinfo"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/controlsync"
	pbSync "github.com/pinksaucepasta/paperboat/internal/controlsync/proto"
	"github.com/pinksaucepasta/paperboat/internal/daemonrpc"
	"github.com/pinksaucepasta/paperboat/internal/diagnosticlog"
	"github.com/pinksaucepasta/paperboat/internal/machineguard"
	"github.com/pinksaucepasta/paperboat/internal/machineloopback"
	"github.com/pinksaucepasta/paperboat/internal/splitdns"
	"google.golang.org/protobuf/proto"
)

type guardedNameClient interface {
	Acquire(context.Context, string, netip.Addr, int) (net.Listener, error)
	ReplaceNames(context.Context, map[string]netip.Addr) error
	ReplaceAliases(context.Context, map[string]string) error
	Close() error
}

type CoordinatorConfig struct {
	LocalAccess          config.LocalAccessConfig
	SocketAddress        string
	SyncAddress          string
	MachineID            string
	Token                func(context.Context) (string, error)
	TLSConfig            *tls.Config
	Insecure             bool
	NameClient           guardedNameClient
	ConnectNameClient    func(context.Context) (guardedNameClient, error)
	DialMachine          func(context.Context, string, int) (net.Conn, error)
	ApprovePeer          func(context.Context, string, bool) error
	ReconcileTimeout     time.Duration
	ReportReconcileError func(error)
	IssueCertificate     func(context.Context, string, string, int) (tls.Certificate, error)
}

type guardedRoute struct {
	listener  net.Listener
	machineID string
	port      int
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	closeOnce sync.Once
	mu        sync.Mutex
	closing   bool
}

func (r *guardedRoute) close() {
	r.closeOnce.Do(func() {
		r.cancelNow()
		_ = r.listener.Close()
		r.wg.Wait()
	})
}

func (r *guardedRoute) cancelNow() {
	r.mu.Lock()
	if !r.closing {
		r.closing = true
		r.cancel()
	}
	r.mu.Unlock()
}

func (r *guardedRoute) startForward() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closing {
		return false
	}
	r.wg.Add(1)
	return true
}

type guardedWebRoute struct {
	proxy         *splitdns.Proxy
	base          string
	machineID     string
	browserRoutes map[string]splitdns.BrowserRoute
}

type limitedListener struct {
	net.Listener
	permits chan struct{}
}

func (l *limitedListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		select {
		case l.permits <- struct{}{}:
			return &limitedConn{Conn: conn, release: func() { <-l.permits }}, nil
		default:
			_ = conn.Close()
		}
	}
}

type limitedConn struct {
	net.Conn
	release func()
	once    sync.Once
}

func (c *limitedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}

type Coordinator struct {
	cfg                CoordinatorConfig
	rpcServer          *daemonrpc.Server
	rpcListener        net.Listener
	rpcBackend         *daemonrpc.LiveDaemonBackend
	syncClient         *controlsync.Client
	errors             chan error
	nameClient         guardedNameClient
	ctx                context.Context
	cancel             context.CancelFunc
	mu                 sync.Mutex
	running            bool
	routes             map[string]*guardedRoute
	webRoutes          map[string]guardedWebRoute
	connections        chan struct{}
	wg                 sync.WaitGroup
	snapshotMu         sync.Mutex
	reconcileErr       error
	inventoryMu        sync.Mutex
	inventoryRevision  uint64
	inventoryTimer     *time.Timer
	stopInventoryTimer func() bool
}

func NewCoordinator(cfg CoordinatorConfig) (*Coordinator, error) {
	domain, err := splitdns.NormalizeBrowserDomain(cfg.LocalAccess.Domain)
	if err != nil {
		return nil, err
	}
	cfg.LocalAccess.Domain = domain
	if cfg.SocketAddress == "" {
		cfg.SocketAddress = daemonrpc.DefaultSocketAddress()
	}
	if cfg.SyncAddress != "" && (cfg.MachineID == "" || cfg.Token == nil) {
		return nil, errors.New("control sync requires machine identity and fresh authenticated credentials")
	}
	if cfg.NameClient != nil && cfg.ConnectNameClient != nil {
		return nil, errors.New("configure either an injected name client or a reconnect factory")
	}
	hasNames := cfg.NameClient != nil || cfg.ConnectNameClient != nil
	if hasNames != (cfg.DialMachine != nil) {
		return nil, errors.New("guarded names require an authenticated machine dialer")
	}
	if cfg.ReconcileTimeout <= 0 {
		cfg.ReconcileTimeout = controlsync.SnapshotTimeout
	}
	coordinator := &Coordinator{
		cfg:         cfg,
		nameClient:  cfg.NameClient,
		rpcBackend:  daemonrpc.NewLiveDaemonBackend(nil, buildinfo.Version),
		routes:      make(map[string]*guardedRoute),
		webRoutes:   make(map[string]guardedWebRoute),
		connections: make(chan struct{}, 256),
		errors:      make(chan error, 1),
	}
	coordinator.rpcBackend.SetApprovalHandler(cfg.ApprovePeer)
	return coordinator, nil
}

func (c *Coordinator) Start(parent context.Context) error {
	c.mu.Lock()
	if c.running {
		c.mu.Unlock()
		return nil
	}
	c.ctx, c.cancel = context.WithCancel(parent)
	c.mu.Unlock()
	if c.cfg.ConnectNameClient != nil {
		connectCtx, cancel := context.WithTimeout(parent, controlsync.ConnectTimeout)
		client, err := c.cfg.ConnectNameClient(connectCtx)
		cancel()
		if err != nil {
			c.stopStarted()
			return fmt.Errorf("connect protected machine-name service: %w", err)
		}
		c.mu.Lock()
		c.nameClient = client
		c.mu.Unlock()
	}
	if c.cfg.SyncAddress != "" {
		client := controlsync.NewClient(controlsync.ClientConfig{ServerAddr: c.cfg.SyncAddress, MachineID: c.cfg.MachineID, Token: c.cfg.Token, TLSConfig: c.cfg.TLSConfig, Insecure: c.cfg.Insecure, OnPeerUpdate: c.applySnapshot, OnError: func(err error) {
			select {
			case c.errors <- err:
			default:
			}
		}})
		c.rpcBackend.SetSyncClient(client)
		c.syncClient = client
		if err := client.Start(c.ctx); err != nil {
			c.stopStarted()
			return fmt.Errorf("start authenticated control sync: %w", err)
		}
	}
	listener, err := daemonrpc.Listen(c.cfg.SocketAddress)
	if err != nil {
		c.stopStarted()
		return fmt.Errorf("listen on daemon socket %s: %w", c.cfg.SocketAddress, err)
	}
	server := daemonrpc.NewServer(c.rpcBackend)
	c.mu.Lock()
	c.rpcListener, c.rpcServer, c.running = listener, server, true
	c.mu.Unlock()
	c.wg.Add(1)
	go func() { defer c.wg.Done(); _ = server.Serve(listener) }()
	return nil
}

// ApplyMachines projects authenticated, unexpired service discovery when the
// optional streaming topology endpoint is not configured.
func (c *Coordinator) ApplyMachines(ctx context.Context, machines []api.UserMachine, services []api.MachineServicesMachine) error {
	if c == nil {
		return errors.New("local access coordinator is unavailable")
	}
	if c.cfg.SyncAddress != "" {
		return c.ReconcileStatus(ctx)
	}
	c.inventoryMu.Lock()
	defer c.inventoryMu.Unlock()
	c.mu.Lock()
	parent := c.ctx
	c.mu.Unlock()
	if parent == nil {
		return errors.New("local access coordinator is not running")
	}
	if parent.Err() != nil {
		return parent.Err()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if c.inventoryTimer != nil {
		c.inventoryTimer.Stop()
	}
	if c.stopInventoryTimer != nil {
		c.stopInventoryTimer()
	}
	byID := make(map[string]api.MachineServicesMachine, len(services))
	var expiry time.Time
	for _, service := range services {
		if service.ExpiresAt.After(time.Now()) {
			byID[service.MachineID] = service
			if expiry.IsZero() || service.ExpiresAt.Before(expiry) {
				expiry = service.ExpiresAt
			}
		}
	}
	peers := make([]*pbSync.PeerUpdate, 0, len(machines))
	for _, m := range machines {
		if m.AssignedIP == "" || !m.NetworkApproved || !m.Online || m.State != "online" {
			continue
		}
		service, found := byID[m.ID]
		var ports []int32
		if found && service.InstallationGeneration == int64(m.InstallationGeneration) {
			for _, port := range service.Services {
				if port.Port > 0 && port.Port <= 65535 {
					ports = append(ports, int32(port.Port))
				}
			}
		}
		peers = append(peers, &pbSync.PeerUpdate{PeerId: m.ID, Alias: m.Alias,
			AssignedIp: m.AssignedIP, Approved: m.NetworkApproved, Online: m.Online, ExportedPorts: ports})
	}
	c.inventoryRevision++
	revision := c.inventoryRevision
	if err := c.reconcileSnapshot(ctx, peers, revision); err != nil {
		return err
	}
	if expiry.IsZero() {
		return nil
	}
	c.inventoryTimer = time.AfterFunc(time.Until(expiry), func() {
		c.inventoryMu.Lock()
		defer c.inventoryMu.Unlock()
		if c.inventoryRevision == revision && parent.Err() == nil {
			c.applySnapshot(nil, revision)
		}
	})
	timer := c.inventoryTimer
	c.stopInventoryTimer = context.AfterFunc(parent, func() { timer.Stop() })
	return nil
}

func (c *Coordinator) reportReconcileError(err error) {
	if c.cfg.ReportReconcileError != nil {
		c.cfg.ReportReconcileError(err)
		return
	}
	diagnosticlog.TryInfo("local machine access reconciliation failed", "error", err)
}

func (c *Coordinator) applySnapshot(peers []*pbSync.PeerUpdate, revision uint64) {
	c.mu.Lock()
	parent := c.ctx
	c.mu.Unlock()
	if parent == nil {
		parent = context.Background()
	}
	_ = c.reconcileSnapshot(parent, peers, revision)
}

func (c *Coordinator) reconcileSnapshot(ctx context.Context, peers []*pbSync.PeerUpdate, revision uint64) error {
	c.snapshotMu.Lock()
	defer c.snapshotMu.Unlock()
	err := c.applySnapshotLocked(ctx, peers, revision)
	c.reconcileErr = err
	return err
}

// ReconcileStatus reports the latest streaming topology reconciliation. The
// client's Start applies its first snapshot synchronously before readiness.
func (c *Coordinator) ReconcileStatus(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.snapshotMu.Lock()
	defer c.snapshotMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	return c.reconcileErr
}

func (c *Coordinator) applySnapshotLocked(parent context.Context, peers []*pbSync.PeerUpdate, revision uint64) error {
	localPeers, err := mapPeerSnapshot(peers, machineloopback.DefaultCIDR)
	if err != nil {
		c.mu.Lock()
		guarded := c.nameClient != nil || c.cfg.ConnectNameClient != nil
		c.mu.Unlock()
		if guarded && parent != nil {
			ctx, cancel := context.WithTimeout(parent, c.cfg.ReconcileTimeout)
			_ = c.replaceGuardedRoutes(ctx, nil)
			cancel()
		}
		c.reportReconcileError(fmt.Errorf("machine snapshot rejected: %w", err))
		c.rpcBackend.ApplyPeerUpdates(nil, revision, nil)
		return err
	}
	c.mu.Lock()
	namesEnabled := c.nameClient != nil || c.cfg.ConnectNameClient != nil
	c.mu.Unlock()
	if namesEnabled {
		if parent == nil {
			return errors.New("local access coordinator is not running")
		}
		ctx, cancel := context.WithTimeout(parent, c.cfg.ReconcileTimeout)
		err := c.replaceGuardedRoutes(ctx, localPeers)
		cancel()
		if err != nil {
			c.reportReconcileError(fmt.Errorf("local machine access withdrawn; retry after repairing the machine guard or browser trust: %w", err))
			c.rpcBackend.ApplyPeerUpdates(nil, revision, nil)
			return err
		}
		// RPC status must describe acquired native listeners, while browser URLs
		// can still offer an independently authorized route to an occupied port.
		c.mu.Lock()
		active := make(map[string]map[int]bool)
		for _, route := range c.routes {
			if active[route.machineID] == nil {
				active[route.machineID] = make(map[int]bool)
			}
			active[route.machineID][route.port] = true
		}
		for _, peer := range localPeers {
			if peer == nil {
				continue
			}
			forwarded := make([]int32, 0, len(peer.ExportedPorts))
			for _, port := range peer.ExportedPorts {
				if active[peer.PeerId][int(port)] {
					forwarded = append(forwarded, port)
				}
			}
			peer.ExportedPorts = forwarded
		}
		c.mu.Unlock()
	}
	c.rpcBackend.ApplyPeerUpdates(localPeers, revision, c.BrowserURLs())
	return nil
}

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
				return nil, fmt.Errorf("peer %q has an invalid assigned address", peer.GetPeerId())
			}
			local, err := machineloopback.MapCanonical(canonical, cidr)
			if err != nil {
				return nil, fmt.Errorf("peer %q assigned address: %w", peer.GetPeerId(), err)
			}
			copyPeer.AssignedIp = local.String()
		}
		mapped[index] = copyPeer
	}
	return mapped, nil
}

func (c *Coordinator) replaceGuardedRoutes(ctx context.Context, peers []*pbSync.PeerUpdate) error {
	c.mu.Lock()
	defer c.mu.Unlock()
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
			return fmt.Errorf("connect protected machine-name service: %w", err)
		}
		c.nameClient = connected
		nameClient = connected
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
		hostname := strings.ToLower(alias) + "." + splitdns.BrowserSuffix
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
				name, err := splitdns.BrowserHostname(alias, port, c.cfg.LocalAccess.Domain)
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
					pattern, err := splitdns.BrowserWildcardPattern(strings.ToLower(alias), c.cfg.LocalAccess.Domain)
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
					aliasName := serviceAlias.Name + "." + strings.ToLower(alias) + "." + c.cfg.LocalAccess.Domain
					_, label, err := splitdns.ParseBrowserHostname(aliasName, c.cfg.LocalAccess.Domain)
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
				aliasName := serviceAlias.Name + "." + strings.ToLower(alias) + "." + c.cfg.LocalAccess.Domain
				if _, active := browserRoutes[aliasName]; active {
					continue
				}
				if _, proxy := aliases["*."+strings.ToLower(alias)+"."+c.cfg.LocalAccess.Domain]; proxy {
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
				if errors.Is(err, machineguard.ErrPortInUse) {
					// The guard's default deny remains in force without a lease.
					// Keep independent routes and retry this port on reconciliation.
					conflictedPorts++
					if firstConflictedPort == 0 {
						firstConflictedPort = port
					}
					continue
				}
				c.resetNameGenerationLocked(ctx)
				return err
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
		if err != nil {
			c.resetNameGenerationLocked(ctx)
			return err
		}
		wantedWeb[splitdns.BrowserGatewayHostname] = web
	}
	if err := nameClient.ReplaceAliases(ctx, aliases); err != nil {
		c.resetNameGenerationLocked(ctx)
		return err
	}
	if err := nameClient.ReplaceNames(ctx, names); err != nil {
		c.resetNameGenerationLocked(ctx)
		return err
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
	cleanupDone := make(chan struct{})
	go func() {
		defer close(cleanupDone)
		for _, route := range removedRoutes {
			route.close()
		}
		for _, route := range removedWeb {
			_ = route.proxy.Stop(ctx)
		}
	}()
	select {
	case <-cleanupDone:
	case <-ctx.Done():
		c.resetNameGenerationLocked(ctx)
		return ctx.Err()
	}
	c.routes = wanted
	c.webRoutes = wantedWeb
	return nil
}

// prepareBrowserGatewayLocked binds the public wildcard address once. Every
// dial is bound to an exact route from the authenticated service snapshot.
func (c *Coordinator) prepareBrowserGatewayLocked(ctx context.Context, client guardedNameClient, routes map[string]splitdns.BrowserRoute, aliases map[string]string) (guardedWebRoute, error) {
	base := splitdns.BrowserGatewayHostname
	if old, ok := c.webRoutes[base]; ok {
		if maps.Equal(old.browserRoutes, routes) {
			return old, nil
		}
		if err := old.proxy.Stop(ctx); err != nil {
			return guardedWebRoute{}, err
		}
		delete(c.webRoutes, base)
	}
	ip := netip.MustParseAddr(splitdns.BrowserGatewayIP)
	httpListener, err := client.Acquire(ctx, base, ip, 80)
	if err != nil {
		return guardedWebRoute{}, err
	}
	httpsListener, err := client.Acquire(ctx, base, ip, 443)
	if err != nil {
		_ = httpListener.Close()
		return guardedWebRoute{}, err
	}
	closeListeners := func() { _ = httpListener.Close(); _ = httpsListener.Close() }
	if err := client.ReplaceAliases(ctx, aliases); err != nil {
		closeListeners()
		return guardedWebRoute{}, err
	}
	byAddress := make(map[string]string, len(routes))
	for name, route := range routes {
		if route.MachineID == "" {
			closeListeners()
			return guardedWebRoute{}, errors.New("browser service has no authenticated machine identity")
		}
		address := net.JoinHostPort(route.Address.String(), strconv.Itoa(route.Port))
		if prior, exists := byAddress[address]; exists && prior != route.MachineID {
			closeListeners()
			return guardedWebRoute{}, errors.New("ambiguous browser service address")
		}
		byAddress[address] = route.MachineID
		if strings.HasPrefix(name, "*.") {
			continue
		}
		if _, err := c.issueBrowserCertificate(ctx, client, name, route.MachineID, route.Port); err != nil {
			closeListeners()
			return guardedWebRoute{}, err
		}
	}
	deniedHosts := make(map[string]bool)
	for name, target := range aliases {
		if target == "" {
			deniedHosts[name] = true
		}
	}
	proxy, err := splitdns.NewProxy(splitdns.ProxyConfig{DeniedHosts: deniedHosts, Domain: c.cfg.LocalAccess.Domain, Routes: routes, IssueCertificate: func(certCtx context.Context, hostname string) (tls.Certificate, error) {
		if deniedHosts[hostname] {
			return tls.Certificate{}, errors.New("configured browser alias destination is unavailable")
		}
		route, ok := browserRouteForHostDomain(routes, hostname, c.cfg.LocalAccess.Domain)
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
		// This closure contains only the proxy's current registered routes.
		for name := range routes {
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
	}, DialContext: func(ctx context.Context, _, address string) (net.Conn, error) {
		machineID, ok := byAddress[address]
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
		err = proxy.StartListeners(&limitedListener{Listener: httpListener, permits: c.connections}, &limitedListener{Listener: httpsListener, permits: c.connections})
	}
	if err != nil {
		closeListeners()
		return guardedWebRoute{}, err
	}
	web := guardedWebRoute{proxy: proxy, base: base, browserRoutes: routes}
	c.webRoutes[base] = web
	return web, nil
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
	done := make(chan struct{})
	go func() {
		defer close(done)
		// Closing this generation first atomically withdraws its helper leases.
		// Later listener Close calls retain the old client and cannot affect a
		// replacement generation established by a future snapshot.
		if client != nil {
			_ = client.Close()
		}
		for _, route := range routes {
			route.close()
		}
		for _, route := range webRoutes {
			_ = route.proxy.Stop(ctx)
		}
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

func (c *Coordinator) serveRoute(route *guardedRoute) {
	defer c.wg.Done()
	for {
		local, err := route.listener.Accept()
		if err != nil {
			return
		}
		select {
		case c.connections <- struct{}{}:
		default:
			_ = local.Close()
			continue
		}
		if !route.startForward() {
			_ = local.Close()
			<-c.connections
			return
		}
		go func() {
			defer route.wg.Done()
			defer func() { <-c.connections }()
			c.forward(route.ctx, local, route.machineID, route.port)
		}()
	}
}

func (c *Coordinator) forward(ctx context.Context, local net.Conn, machineID string, port int) {
	defer local.Close()
	remote, err := c.cfg.DialMachine(ctx, machineID, port)
	if err != nil {
		return
	}
	defer remote.Close()
	done := make(chan error, 2)
	go func() {
		_, copyErr := io.Copy(remote, local)
		if half, ok := remote.(interface{ CloseWrite() error }); ok {
			_ = half.CloseWrite()
		}
		done <- copyErr
	}()
	go func() {
		_, copyErr := io.Copy(local, remote)
		if half, ok := local.(interface{ CloseWrite() error }); ok {
			_ = half.CloseWrite()
		}
		done <- copyErr
	}()
	canceled := ctx.Done()
	for remaining := 2; remaining > 0; {
		select {
		case copyErr := <-done:
			remaining--
			if copyErr != nil {
				_ = local.Close()
				_ = remote.Close()
			}
		case <-canceled:
			_ = local.Close()
			_ = remote.Close()
			canceled = nil
		}
	}
}

func (c *Coordinator) stopStarted() {
	c.mu.Lock()
	if c.cancel != nil {
		c.cancel()
	}
	c.withdrawGuardedLocked()
	c.ctx, c.cancel = nil, nil
	c.mu.Unlock()
	if c.syncClient != nil {
		_ = c.syncClient.Close()
		c.syncClient = nil
		c.rpcBackend.SetSyncClient(nil)
	}
}

func (c *Coordinator) Stop() error {
	c.mu.Lock()
	server, listener, cancel := c.rpcServer, c.rpcListener, c.cancel
	c.rpcServer, c.rpcListener, c.ctx, c.cancel, c.running = nil, nil, nil, nil, false
	routes := c.routes
	webRoutes := c.webRoutes
	nameClient := c.nameClient
	c.nameClient = nil
	c.routes = make(map[string]*guardedRoute)
	c.webRoutes = make(map[string]guardedWebRoute)
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if c.syncClient != nil {
		_ = c.syncClient.Close()
		c.syncClient = nil
		c.rpcBackend.SetSyncClient(nil)
	}
	// Closing the controller connection withdraws all helper leases in one
	// transaction and prevents per-listener RPC deadlines accumulating on stop.
	if nameClient != nil {
		_ = nameClient.Close()
	}
	for _, route := range routes {
		route.close()
	}
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), controlsync.ConnectTimeout)
	defer shutdownCancel()
	for _, route := range webRoutes {
		_ = route.proxy.Stop(shutdownCtx)
	}
	if server != nil {
		server.Stop()
	}
	if listener != nil {
		_ = listener.Close()
	}
	c.wg.Wait()
	return nil
}

// Errors reports a terminal sync failure after local routes have been withdrawn.
func (c *Coordinator) Errors() <-chan error { return c.errors }

// BrowserURLs reports only currently protected and published browser routes.
func (c *Coordinator) BrowserURLs() map[string]map[int32]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	result := make(map[string]map[int32]string)
	for _, web := range c.webRoutes {
		for host, route := range web.browserRoutes {
			_, label, err := splitdns.ParseBrowserHostname(host, c.cfg.LocalAccess.Domain)
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

// browserRouteForHost admits a base machine URL only while one exact port route
// for that machine is registered. The base page never selects a forwarding port.
func browserRouteForHost(routes map[string]splitdns.BrowserRoute, host string) (splitdns.BrowserRoute, bool) {
	return browserRouteForHostDomain(routes, host, splitdns.BrowserSuffix)
}

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

func (c *Coordinator) issueBrowserCertificate(ctx context.Context, client guardedNameClient, hostname, machineID string, port int) (tls.Certificate, error) {
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
	if err != nil || !root.IsCA || !root.PermittedDNSDomainsCritical || len(root.PermittedDNSDomains) != 1 || root.PermittedDNSDomains[0] != "."+c.cfg.LocalAccess.Domain {
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

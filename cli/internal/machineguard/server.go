//go:build linux || darwin || windows

package machineguard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/pinksaucepasta/paperboat/internal/buildinfo"
	"github.com/pinksaucepasta/paperboat/internal/machineloopback"
	"github.com/pinksaucepasta/paperboat/internal/splitdns"
	"maps"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type controlConn interface {
	Identity() (string, error)
	Receive() (request, error)
	Send(response, net.Listener) error
	Close() error
}
type controlListener interface {
	Accept() (controlConn, error)
	Close() error
}
type reservations struct {
	Names  map[string]string `json:"names"`
	Claims map[string]claim  `json:"claims"`
}
type guardedLease struct {
	hostname, ip string
	port         int
	uid          string
	owner        controlConn
	listener     net.Listener
}
type guardServer struct {
	mu              sync.Mutex
	cfg             Config
	reserved        reservations
	claims          *claimBook
	leases          map[string]*guardedLease
	gateways        map[int]*sharedGateway
	names           map[controlConn]map[string]string
	aliases         map[controlConn]map[string]string
	connections     map[controlConn]string
	wg              sync.WaitGroup
	closing         bool
	failures        chan error
	ca              *splitdns.CA
	trustReady      bool
	certificateGate chan struct{}
	certificates    map[controlConn]map[string]cachedCertificate
	previewPAC      map[controlConn]string
	// policyPending marks leases acquired since the last installed policy: an acquire only binds, and the
	// daemon's one "apply" per reconcile installs the policy of all of them in one transaction (one WFP Apply
	// for N peers, not N). Until then the new addresses stay denied (fail closed). One flag for the whole guard:
	// any connection's apply or release installs every pending lease, each already validated and bound.
	policyPending bool
}

// applyPolicy installs the whole access policy; tests replace it.
var applyPolicy = applyProtection

func Serve(ctx context.Context, cfg Config) (resultErr error) {
	if cfg.StateDir == "" {
		cfg.StateDir = DefaultStateDir
	}
	activeCIDR := cfg.LoopbackCIDR
	if strings.TrimSpace(activeCIDR) == "" {
		activeCIDR = machineloopback.DefaultCIDR
	}
	prefix, err := machineloopback.Prefix(activeCIDR)
	if err != nil {
		return fmt.Errorf("machine loopback CIDR: %w", err)
	}
	if prefix.String() != machineloopback.DefaultCIDR {
		return errors.New("machine guard uses the fixed 127.100.0.0/16 range")
	}
	cfg.LoopbackCIDR = prefix.String()
	if err := requirePrivilege(); err != nil {
		return err
	}
	if cfg.Socket == "" {
		cfg.Socket = DefaultSocket
	}
	if err := protectedDirectory(cfg.StateDir, 0700); err != nil {
		return err
	}
	lock, err := lockState(filepath.Join(cfg.StateDir, "lock"))
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, lock.Close()) }()
	g := &guardServer{cfg: cfg, reserved: reservations{Names: map[string]string{}}, leases: map[string]*guardedLease{}, names: map[controlConn]map[string]string{}, connections: map[controlConn]string{}, failures: make(chan error, 1)}
	if data, err := os.ReadFile(filepath.Join(cfg.StateDir, "reservations.json")); err == nil {
		if len(data) > 8<<20 || json.Unmarshal(data, &g.reserved) != nil || g.reserved.Names == nil {
			return errors.New("invalid machine guard reservation journal")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if g.reserved.Claims == nil {
		g.reserved.Claims = map[string]claim{}
	}
	g.claims = &claimBook{Claims: g.reserved.Claims}
	defer retirePlatform(cfg)
	defer g.startPreviewPAC(ctx)()
	if err = g.applyRules(ctx); err != nil {
		return err
	}
	trustCtx, trustCancel := context.WithTimeout(ctx, 2*time.Minute)
	err = g.prepareLocalCA(trustCtx)
	trustCancel()
	// Pending trust leaves the guard serving; RuntimeStatus reports TrustReady=false.
	if err != nil && !isTrustPending(err) {
		return fmt.Errorf("prepare trusted local browser CA: %w", err)
	}
	// Default-deny rules intentionally survive process exit and are restored before
	// user sessions at boot.
	listener, err := listenControl(cfg.Socket)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, listener.Close(), g.closeOwned()) }()
	acceptErrors := make(chan error, 1)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				acceptErrors <- err
				return
			}
			g.mu.Lock()
			if len(g.connections) >= 128 {
				g.mu.Unlock()
				conn.Close()
				continue
			}
			g.connections[conn] = ""
			g.wg.Add(1)
			g.mu.Unlock()
			go g.serveConnection(ctx, conn)
		}
	}()
	if err := notifyReady(); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		err = ctx.Err()
	case err = <-acceptErrors:
	case err = <-g.failures:
	}
	return err
}

func (g *guardServer) closeOwned() error {
	g.mu.Lock()
	g.closing = true
	var closeErr error
	for _, lease := range g.leases {
		closeErr = errors.Join(closeErr, shutdownListener(lease.listener))
	}
	g.leases = map[string]*guardedLease{}
	g.names = map[controlConn]map[string]string{}
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	closeErr = errors.Join(closeErr, g.applyRules(shutdownCtx))
	shutdownCancel()
	for conn := range g.connections {
		closeErr = errors.Join(closeErr, conn.Close())
	}
	g.mu.Unlock()
	g.wg.Wait()
	return closeErr
}
func (g *guardServer) serveConnection(ctx context.Context, conn controlConn) {
	defer g.wg.Done()
	defer conn.Close()
	uid, err := conn.Identity()
	if err != nil {
		return
	}
	defer func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		g.withdraw(ctx, conn)
		delete(g.connections, conn)
	}()

	g.mu.Lock()
	sameOwner := 0
	for other, owner := range g.connections {
		if other != conn && owner == uid {
			sameOwner++
		}
	}
	if sameOwner >= 4 {
		g.mu.Unlock()
		return
	}
	g.connections[conn] = uid
	g.mu.Unlock()
	for {
		in, err := conn.Receive()
		if err != nil {
			return
		}
		opCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		var file net.Listener
		var status *RuntimeStatus
		var certificate *CertificateBundle
		if in.Operation == "status" {
			status = g.runtimeStatus(uid)
		} else if in.Operation == "certificate" {
			certificate, err = g.certificate(opCtx, conn, uid, in.Hostname)
		} else {
			g.mu.Lock()
			in, err = g.assignClaim(uid, in)
			if err == nil {
				file, err = g.handle(opCtx, conn, uid, in)
			}
			g.pruneCertificates(conn, uid)
			g.mu.Unlock()
		}
		cancel()
		out := response{Status: status, Certificate: certificate}
		if in.Operation == "acquire" && err == nil {
			out.IP = in.IP
		}
		if err != nil {
			out.Error = err.Error()
			if errors.Is(err, ErrPortInUse) {
				out.ErrorCode = "port_in_use"
			} else if errors.Is(err, ErrListenerRefused) {
				out.ErrorCode = "listener_refused"
			} else if errors.Is(err, ErrNamesRefused) {
				out.ErrorCode = "names_refused"
			}
		}
		if err := conn.Send(out, file); err != nil {
			return
		}

	}
}

func (g *guardServer) runtimeStatus(owner ...string) *RuntimeStatus {
	g.mu.Lock()
	defer g.mu.Unlock()
	trustReady := g.trustReady
	renewal := g.ca != nil && !g.ca.ExpiresAt().After(time.Now().Add(localCARenewalWindow))
	domains, err := loadBrowserDomains(g.cfg.StateDir)
	if err != nil {
		trustReady = false
	}
	domain := domains.Default
	if len(owner) > 0 && err == nil {
		domain, err = domains.selected(owner[0])
		if err != nil {
			trustReady = false
		}
		if err == nil && domain != domains.Default {
			data, e := readRenewalFile(filepath.Join(g.cfg.StateDir, "certificates", domain, "rootCA.pem"), "rootCA.pem", validateOwnedRootStateFile)
			if e != nil {
				trustReady = false
			} else {
				cert, e := parseLocalTrustRoot(data)
				if e != nil {
					trustReady = false
				} else if !cert.NotAfter.After(time.Now().Add(localCARenewalWindow)) {
					renewal = true
				}
			}
		}
	}
	return &RuntimeStatus{BrowserDomain: domain, DefaultBrowserDomain: domains.Default, RootRenewalRequired: renewal, TrustReady: trustReady, Version: buildinfo.Version, LoopbackCIDR: g.cfg.LoopbackCIDR, Ready: true, ActiveLeases: len(g.leases), Features: guardFeatures(), GatewayRefusals: gatewayRefusalSnapshot()}
}

// validName admits exactly one machine label beneath the owner's local access
// domain. The label "gateway" is reserved for the browser gateway.
func validName(host, domain string) bool {
	if domain == "" || len(host) > 253 || host != strings.ToLower(host) {
		return false
	}
	label, ok := strings.CutSuffix(host, "."+domain)
	return ok && label != "gateway" && splitdns.ValidBrowserLabel(label)
}

// validSubdomainName admits <port>.<machine>.<domain> with a canonical port.
func validSubdomainName(host, domain string) bool {
	port, machine, ok := strings.Cut(host, ".")
	number, err := strconv.Atoi(port)
	return ok && err == nil && number > 0 && number <= 65535 && port == strconv.Itoa(number) && validName(machine, domain)
}

func validAddress(ip string) bool { return validAddressInCIDR(ip, machineloopback.DefaultCIDR) }

func validProtectedBindAddress(address, cidr string) bool {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	number, _ := strconv.Atoi(port)
	return validAddressInCIDR(host, cidr) || host == splitdns.BrowserGatewayIP && gatewayPortAllowed(number)
}

// gatewayPortAllowed is the set of ports the browser gateway address may serve: the web ports
// and unprivileged ports, where https://<machine>.<domain>:<port> reaches a machine service.
func gatewayPortAllowed(port int) bool {
	return port == 80 || port == 443 || port >= 1024 && port <= 65535
}

// protectedLoopbackCIDRs is the set of prefixes the guard denies by default and serves leases from: the one fixed
// machine range.
func protectedLoopbackCIDRs() []string { return []string{machineloopback.DefaultCIDR} }

func validAddressInCIDR(ip, cidr string) bool {
	if strings.TrimSpace(cidr) == "" {
		cidr = machineloopback.DefaultCIDR
	}
	if cidr != machineloopback.DefaultCIDR {
		return false
	}
	address, err := netip.ParseAddr(ip)
	prefix, prefixErr := machineloopback.Prefix(cidr)
	if err != nil || prefixErr != nil || !address.Is4() || !prefix.Contains(address) {
		return false
	}
	b := address.As4()
	return b[3] > 0 && b[3] < 255 && address != netip.MustParseAddr(splitdns.BrowserGatewayIP)
}

// validOwnedLoopbackAddress is used only to retire addresses recorded in the guard's protected alias journal, so
// cleanup can remove only recorded aliases inside the machine range.
func validOwnedLoopbackAddress(ip string) bool {
	address, err := netip.ParseAddr(ip)
	if err != nil || !address.Is4() {
		return false
	}
	octets := address.As4()
	if octets[3] == 0 || octets[3] == 255 {
		return false
	}
	for _, value := range protectedLoopbackCIDRs() {
		prefix, err := machineloopback.Prefix(value)
		if err == nil && prefix.Contains(address) {
			return true
		}
	}
	return false
}
func (g *guardServer) handle(ctx context.Context, conn controlConn, uid string, in request) (net.Listener, error) {
	key := net.JoinHostPort(in.IP, strconv.Itoa(in.Port))
	switch in.Operation {
	case "acquire":
		gateway := in.Hostname == splitdns.BrowserGatewayHostname && in.IP == splitdns.BrowserGatewayIP && gatewayPortAllowed(in.Port)
		if gateway && gatewayRouting {
			return g.acquireGatewayRoute(ctx, conn, uid, in)
		}
		domain, err := selectedBrowserDomain(g.cfg.StateDir, uid)
		if err != nil && !gateway {
			return nil, err
		}
		if !gateway && (!validName(in.Hostname, domain) || !validAddressInCIDR(in.IP, g.cfg.LoopbackCIDR) || in.Port < 1 || in.Port > 65535) {
			return nil, errors.New("invalid protected machine listener")
		}

		for other, aliases := range g.aliases {
			if _, exists := aliases[in.Hostname]; exists && g.sameNamespace(other, uid) {
				return nil, errors.New("machine name conflicts with an active browser alias")
			}
		}
		if _, exists := g.leases[key]; exists {
			return nil, listenerRefusal{reason: "protected listener is already owned by another connection"}
		}
		if err := g.reserveName(uid, in.Hostname); err != nil {
			return nil, err
		}
		if err := g.persist(); err != nil {
			return nil, err
		}
		listener, err := listenProtected(ctx, key, g.cfg.LoopbackCIDR)
		if err != nil {
			return nil, fmt.Errorf("create protected listener: %w", err)
		}
		g.leases[key] = &guardedLease{hostname: in.Hostname, ip: in.IP, port: in.Port, uid: uid, owner: conn, listener: listener}
		g.policyPending = true
		return listener, nil
	case "apply":
		if !g.policyPending {
			return nil, nil
		}
		return nil, g.applyRules(ctx)
	case "release":
		if in.IP == splitdns.BrowserGatewayIP && gatewayRouting {
			key = gatewayRouteKey(key, uid)
		}
		lease, ok := g.leases[key]
		if !ok {
			return nil, nil
		}
		if lease.owner != conn {
			return nil, errors.New("protected listener belongs to another connection")
		}

		delete(g.leases, key)
		g.removeUnservedNames(conn)
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		shutdownErr := shutdownListener(lease.listener)
		updateErr := g.applyRules(cleanupCtx)
		if err := errors.Join(shutdownErr, updateErr); err != nil {
			g.fail(err)
			return nil, err
		}
		return nil, nil
	case "claims":
		present := make(map[string]bool, len(in.Machines))
		for _, machine := range in.Machines {
			present[machine] = true
		}
		if err := g.claims.releaseAbsent(uid, in.Account, present, time.Now()); err != nil {
			return nil, err
		}
		return nil, g.persist()
	case "aliases":
		return nil, g.replaceAliases(ctx, conn, uid, in.Names)
	case "preview_pac":
		return nil, g.setPreviewPAC(conn, in.PAC)
	case "names":
		if len(in.Names) > 128 {
			return nil, errors.New("too many machine names")
		}
		domain, err := selectedBrowserDomain(g.cfg.StateDir, uid)
		if err != nil && len(in.Names) > 0 {
			return nil, err
		}
		accepted := make(map[string]string, len(in.Names))
		var refused []string
	names:
		for name, ip := range in.Names {
			if (!validName(name, domain) && !validSubdomainName(name, domain)) || !validAddressInCIDR(ip, g.cfg.LoopbackCIDR) {
				return nil, errors.New("invalid machine name")
			}
			found := false
			for _, lease := range g.leases {
				if lease.owner == conn && g.leaseServesName(conn, lease, name) && lease.ip == ip {
					found = true
					break
				}
			}
			if !found {
				refused = append(refused, name+" (no protected ready listener)")
				continue
			}
			// Publications are keyed by (uid, hostname): every OS user keeps the same short names and the routed
			// gateway sends each connection to its own user's daemon (multi-account hosts, PLAN 3b). Only another
			// connection of the same user (or any other connection where the gateway is not routed) clashes.
			for other, names := range g.names {
				if other == conn || (gatewayRouting && g.connections[other] != uid) {
					continue
				}
				if _, ok := names[name]; ok {
					refused = append(refused, name+" (already published by another connection of this user)")
					continue names
				}
			}
			accepted[name] = ip
		}
		if !maps.Equal(g.names[conn], accepted) {
			g.names[conn] = accepted
		}
		if len(refused) > 0 {
			slices.Sort(refused)
			return nil, namesRefusal{reason: "machine names not published: " + strings.Join(refused, ", ")}
		}
		return nil, nil
	default:
		return nil, errors.New("unknown machine guard operation")
	}
}
func has(m map[string]string, key string) bool { _, ok := m[key]; return ok }
func (g *guardServer) persist() error {
	data, err := json.Marshal(g.reserved)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(g.cfg.StateDir, ".reservations-")
	if err != nil {
		return err
	}
	name := file.Name()
	defer os.Remove(name)
	if err = file.Chmod(0600); err == nil {
		_, err = file.Write(data)
	}
	if err == nil {
		err = file.Sync()
	}
	err = errors.Join(err, file.Close())
	if err != nil {
		return err
	}
	if err = replaceStateFile(name, filepath.Join(g.cfg.StateDir, "reservations.json")); err != nil {
		return err
	}
	return syncDirectory(g.cfg.StateDir)
}
func (g *guardServer) withdraw(ctx context.Context, conn controlConn) {
	var retired []net.Listener
	for key, lease := range g.leases {
		if lease.owner == conn {
			delete(g.leases, key)
			if err := shutdownListener(lease.listener); err != nil {
				g.fail(err)
			}
			retired = append(retired, lease.listener)
		}
	}
	delete(g.names, conn)
	delete(g.aliases, conn)
	delete(g.certificates, conn)
	delete(g.previewPAC, conn)
	// If nft updates fail, retained kernel rules still demand the root-marked
	// listener; shutting down the shared listening socket disables every SCM_RIGHTS copy.
	// Existing unmarked replacement sockets still fail the retained SYN mark rule.
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if !g.closing && len(retired) > 0 {
		if err := g.applyRules(cleanupCtx); err != nil {
			g.fail(err)
		}
	}
}
func (g *guardServer) removeUnservedNames(conn controlConn) bool {
	changed := false
	for name, ip := range g.names[conn] {
		found := false
		for _, lease := range g.leases {
			if lease.owner == conn && g.leaseServesName(conn, lease, name) && lease.ip == ip {
				found = true
				break
			}
		}
		if !found {
			delete(g.names[conn], name)
			changed = true
		}
	}
	return changed
}
func (g *guardServer) fail(err error) {
	select {
	case g.failures <- err:
	default:
	}
}

func (g *guardServer) applyRules(ctx context.Context) error {
	leases := make([]*guardedLease, 0, len(g.leases))
	for _, lease := range g.leases {
		leases = append(leases, lease)
	}
	sort.Slice(leases, func(i, j int) bool {
		if leases[i].ip != leases[j].ip {
			return leases[i].ip < leases[j].ip
		}
		return leases[i].port < leases[j].port
	})
	if err := applyPolicy(ctx, g.cfg, leases); err != nil {
		return err
	}
	g.policyPending = false
	return nil
}

// assignClaim gives a machine listener request its address from the uid's claim for (account, machine): the
// preferred (server) address when it is free on this host, else another stable one. Registry lock held.
func (g *guardServer) assignClaim(uid string, in request) (request, error) {
	if in.Operation != "acquire" || in.Hostname == splitdns.BrowserGatewayHostname {
		return in, nil
	}
	preferred, err := netip.ParseAddr(in.IP)
	if err != nil {
		return in, errors.New("invalid preferred machine address")
	}
	if in.Account == "" || in.Machine == "" {
		return in, listenerRefusal{reason: "a machine address request needs an account and a machine"}
	}
	key := claimKey{UID: uid, Account: in.Account, Machine: in.Machine}
	ip, err := g.claims.assign(key, preferred, time.Now())
	if err != nil {
		return in, err
	}
	in.IP = ip
	return in, g.persist()
}

func guardFeatures() []string {
	if gatewayRouting {
		return []string{FeatureAddressClaims, FeatureGatewayRouting}
	}
	return []string{FeatureAddressClaims}
}

func gatewayRouteKey(key, uid string) string { return key + "#" + uid }

// acquireGatewayRoute registers uid's daemon on a gateway port shared by every OS user (F23-01). Called under g.mu.
func (g *guardServer) acquireGatewayRoute(ctx context.Context, conn controlConn, uid string, in request) (net.Listener, error) {
	if !in.Handoff {
		return nil, listenerRefusal{reason: "this machine guard routes the browser gateway by OS user; update pb for this user"}
	}
	key := gatewayRouteKey(net.JoinHostPort(in.IP, strconv.Itoa(in.Port)), uid)
	if _, exists := g.leases[key]; exists {
		return nil, listenerRefusal{reason: "protected listener is already owned by another connection"}
	}
	if len(g.leases) >= 8192 {
		return nil, listenerRefusal{reason: "machine guard reservation limit reached for this OS user"}
	}
	route, err := g.routeGateway(ctx, uid, in.Port)
	if err != nil {
		return nil, fmt.Errorf("create protected listener: %w", err)
	}
	g.leases[key] = &guardedLease{hostname: in.Hostname, ip: in.IP, port: in.Port, uid: uid, owner: conn, listener: route}
	g.policyPending = true
	return route, nil
}

// nameKey is the reservation key of a machine name or browser alias. Where the gateway is routed by uid (F23-01) a
// name belongs to one OS user's namespace only: two users may both have hp.<domain>, each reaching their own
// machine. Elsewhere a name is reserved for the whole host.
func nameKey(uid, hostname string) string {
	if gatewayRouting {
		return uid + ":" + hostname
	}
	return hostname
}

// sameNamespace reports whether names of conn's OS user can conflict with uid's names.
func (g *guardServer) sameNamespace(conn controlConn, uid string) bool {
	return !gatewayRouting || g.connections[conn] == uid
}

// reserveName reserves a machine name for uid, refusing a name another OS user holds in the same namespace and
// bounding how many names one user and the host keep.
func (g *guardServer) reserveName(uid, hostname string) error {
	key := nameKey(uid, hostname)
	atLimit := func() bool {
		ownedNames := 0
		for _, owner := range g.reserved.Names {
			if owner == uid {
				ownedNames++
			}
		}
		return !has(g.reserved.Names, key) && (len(g.reserved.Names) >= 4096 || ownedNames >= 512)
	}
	if len(g.leases) >= 8192 {
		return listenerRefusal{reason: "machine guard reservation limit reached for this OS user"}
	}
	if atLimit() {
		g.pruneNames(uid)
		if atLimit() {
			return listenerRefusal{reason: "machine guard reservation limit reached for this OS user"}
		}
	}
	if owner, exists := g.reserved.Names[key]; exists && owner != uid {
		return listenerRefusal{reason: "machine name or address belongs to another OS user; choose a nonconflicting installation"}
	}
	g.reserved.Names[key] = uid
	return nil
}

// pruneNames forgets uid's reservations of names nothing serves any more (a machine that left the account), so a
// long-lived machine with peer churn never fills its per-user cap with dead names. Called at the cap, under g.mu.
func (g *guardServer) pruneNames(uid string) {
	live := map[string]bool{}
	for _, lease := range g.leases {
		if lease.uid == uid {
			live[lease.hostname] = true
		}
	}
	for conn, names := range g.names {
		if g.connections[conn] == uid {
			for name := range names {
				live[name] = true
			}
		}
	}
	for conn, aliases := range g.aliases {
		if g.connections[conn] == uid {
			for name := range aliases {
				live[name] = true
			}
		}
	}
	for key, owner := range g.reserved.Names {
		if owner == uid && !live[strings.TrimPrefix(key, uid+":")] {
			delete(g.reserved.Names, key)
		}
	}
}

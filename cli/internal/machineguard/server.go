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
	IPs   map[string]string `json:"ips"`
	Names map[string]string `json:"names"`
}
type guardedLease struct {
	hostname, ip string
	port         int
	uid          string
	owner        controlConn
	listener     net.Listener
}
type guardServer struct {
	mu               sync.Mutex
	cfg              Config
	reserved         reservations
	leases           map[string]*guardedLease
	names            map[controlConn]map[string]string
	aliases          map[controlConn]map[string]string
	connections      map[controlConn]string
	wg               sync.WaitGroup
	closing          bool
	failures         chan error
	rangeChangeOwner controlConn
	ca               *splitdns.CA
	trustReady       bool
	certificateGate  chan struct{}
	certificates     map[controlConn]map[string]cachedCertificate
}

func Serve(ctx context.Context, cfg Config) (resultErr error) {
	if cfg.StateDir == "" {
		cfg.StateDir = DefaultStateDir
	}
	state, err := loadLoopbackState(cfg.StateDir)
	if err != nil {
		return fmt.Errorf("load protected loopback ranges: %w", err)
	}
	if state.Active != machineloopback.DefaultCIDR {
		return errors.New("machine guard must use the fixed 127.100.0.0/16 range; retry installation to migrate the previous range")
	}
	activeCIDR := cfg.LoopbackCIDR
	if strings.TrimSpace(activeCIDR) == "" {
		activeCIDR = state.Active
	}
	prefix, err := machineloopback.Prefix(activeCIDR)
	if err != nil {
		return fmt.Errorf("machine loopback CIDR: %w", err)
	}
	if prefix.String() != machineloopback.DefaultCIDR {
		return errors.New("machine guard uses the fixed 127.100.0.0/16 range; retry installation to migrate the previous range")
	}
	cfg.LoopbackCIDR = prefix.String()
	cfg.ProtectedLoopbackCIDRs = append(append([]string(nil), state.Protected...), cfg.ProtectedLoopbackCIDRs...)
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
	defer lock.Close()
	g := &guardServer{cfg: cfg, reserved: reservations{IPs: map[string]string{}, Names: map[string]string{}}, leases: map[string]*guardedLease{}, names: map[controlConn]map[string]string{}, connections: map[controlConn]string{}, failures: make(chan error, 1)}
	if data, err := os.ReadFile(filepath.Join(cfg.StateDir, "reservations.json")); err == nil {
		if len(data) > 8<<20 || json.Unmarshal(data, &g.reserved) != nil || g.reserved.IPs == nil || g.reserved.Names == nil {
			return errors.New("invalid machine guard reservation journal")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	defer retirePlatform(cfg)
	if err = g.applyRules(ctx); err != nil {
		return err
	}
	if err = cleanupHistoricalLocalNames(ctx, cfg); err != nil {
		return fmt.Errorf("remove historical local name integration: %w", err)
	}
	trustCtx, trustCancel := context.WithTimeout(ctx, 2*time.Minute)
	err = g.prepareLocalCA(trustCtx)
	trustCancel()
	if err != nil {
		return fmt.Errorf("prepare trusted local browser CA: %w", err)
	}
	// Default-deny rules intentionally survive process exit and are restored before
	// user sessions at boot.
	listener, err := listenControl(cfg.Socket)
	if err != nil {
		return err
	}
	defer listener.Close()
	stop := context.AfterFunc(ctx, func() { listener.Close() })
	defer stop()
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
	defer func() { listener.Close(); resultErr = errors.Join(resultErr, g.closeOwned()) }()
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

const loopbackCIDRFile = "loopback-cidr"
const loopbackStateFile = "loopback-ranges.json"

type loopbackState struct {
	Active    string   `json:"active"`
	Protected []string `json:"protected"`
}

func loadLoopbackState(stateDir string) (loopbackState, error) {
	if stateDir == "" {
		stateDir = DefaultStateDir
	}
	data, err := os.ReadFile(filepath.Join(stateDir, loopbackStateFile))
	var state loopbackState
	if err == nil {
		if len(data) > 8192 || json.Unmarshal(data, &state) != nil {
			return loopbackState{}, errors.New("invalid protected loopback range state")
		}
		if clean, cleanErr := machineloopback.NormalizeCIDR(state.Active); cleanErr == nil {
			state.Active = clean
			seen := map[string]bool{clean: true}
			valid := []string{clean}
			for _, value := range state.Protected {
				value, cleanErr = machineloopback.NormalizeCIDR(value)
				if cleanErr != nil {
					return loopbackState{}, errors.New("invalid protected loopback range state")
				}
				if seen[value] {
					continue
				}
				seen[value] = true
				valid = append(valid, value)
			}
			state.Protected = valid
			if len(valid) > 254 {
				return loopbackState{}, errors.New("protected loopback range limit exceeded")
			}
			return state, nil
		}
		return loopbackState{}, errors.New("invalid protected loopback range state")
	}
	if !os.IsNotExist(err) {
		return loopbackState{}, err
	}
	active, legacyErr := storedLoopbackCIDRLegacy(stateDir)
	if legacyErr != nil {
		return loopbackState{}, legacyErr
	}
	return loopbackState{Active: active, Protected: []string{active}}, nil
}

func storedLoopbackCIDRLegacy(stateDir string) (string, error) {
	if stateDir == "" {
		stateDir = DefaultStateDir
	}
	data, err := os.ReadFile(filepath.Join(stateDir, loopbackCIDRFile))
	if err == nil {
		if clean, cleanErr := machineloopback.NormalizeCIDR(strings.TrimSpace(string(data))); cleanErr == nil {
			return clean, nil
		}
		return "", errors.New("invalid legacy protected loopback range state")
	}
	if !os.IsNotExist(err) {
		return "", err
	}
	return machineloopback.DefaultCIDR, nil
}

func storeLoopbackCIDR(stateDir, cidr string) error {
	clean, err := machineloopback.NormalizeCIDR(cidr)
	if err != nil {
		return err
	}
	if clean != machineloopback.DefaultCIDR {
		return errors.New("machine loopback range is fixed at 127.100.0.0/16")
	}
	if stateDir == "" {
		stateDir = DefaultStateDir
	}
	if err := protectedDirectory(stateDir, 0700); err != nil {
		return err
	}
	old, err := loadLoopbackState(stateDir)
	if err != nil {
		return err
	}
	next, err := mergedLoopbackState(old, clean)
	if err != nil {
		return err
	}
	return writeLoopbackState(stateDir, next)
}

func mergedLoopbackState(old loopbackState, clean string) (loopbackState, error) {
	seen := make(map[string]bool)
	protected := []string{clean}
	seen[clean] = true
	for _, value := range append(old.Protected, old.Active) {
		normalized, err := machineloopback.NormalizeCIDR(value)
		if err != nil {
			return loopbackState{}, err
		}
		if seen[normalized] {
			continue
		}
		seen[normalized] = true
		protected = append(protected, normalized)
	}
	if len(protected) > 254 {
		return loopbackState{}, errors.New("machine loopback protected range limit reached")
	}
	return loopbackState{Active: clean, Protected: protected}, nil
}

func writeLoopbackState(stateDir string, state loopbackState) error {
	if stateDir == "" {
		stateDir = DefaultStateDir
	}
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(stateDir, ".loopback-ranges-")
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
	if err = replaceStateFile(name, filepath.Join(stateDir, loopbackStateFile)); err != nil {
		return err
	}
	return syncDirectory(stateDir)
}

func prepareLoopbackCIDRChange(ctx context.Context, cidr string) (string, *Client, error) {
	if strings.TrimSpace(cidr) == "" {
		state, err := loadLoopbackState(DefaultStateDir)
		if err != nil {
			return "", nil, err
		}
		cidr = state.Active
	}
	clean, err := machineloopback.NormalizeCIDR(cidr)
	if err != nil {
		return "", nil, err
	}
	if clean != machineloopback.DefaultCIDR {
		return "", nil, errors.New("machine loopback range is fixed at 127.100.0.0/16")
	}
	client, err := Connect(ctx, DefaultSocket)
	if err != nil {
		return clean, nil, nil
	}
	status, err := client.Status(ctx)
	if err != nil {
		client.Close()
		return "", nil, fmt.Errorf("inspect active machine guard before range change: %w", err)
	}
	if status.LoopbackCIDR != clean {
		if err = client.PrepareRangeChange(ctx, clean); err != nil {
			client.Close()
			return "", nil, err
		}
	}
	return clean, client, nil
}

// migrateLoopbackState makes the fixed range active while retaining every
// previously configured range in the persistent deny set. Installers call it
// before starting this version of the guard.
func migrateLoopbackState(ctx context.Context, stateDir string) (loopbackState, error) {
	state, err := loadLoopbackState(stateDir)
	if err != nil {
		return loopbackState{}, err
	}
	if state.Active == machineloopback.DefaultCIDR {
		return state, nil
	}
	_, fence, err := prepareLoopbackCIDRChange(ctx, machineloopback.DefaultCIDR)
	if err != nil {
		return loopbackState{}, err
	}
	if fence != nil {
		defer fence.Close()
	}
	next, err := mergedLoopbackState(state, machineloopback.DefaultCIDR)
	if err != nil {
		return loopbackState{}, err
	}
	if err := writeLoopbackState(stateDir, next); err != nil {
		return loopbackState{}, err
	}
	return next, nil
}
func (g *guardServer) closeOwned() error {
	g.mu.Lock()
	g.closing = true
	for _, lease := range g.leases {
		_ = shutdownListener(lease.listener)
		lease.listener.Close()
	}
	g.leases = map[string]*guardedLease{}
	g.names = map[controlConn]map[string]string{}
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	err := g.applyRules(shutdownCtx)
	shutdownCancel()
	for conn := range g.connections {
		conn.Close()
	}
	g.mu.Unlock()
	g.wg.Wait()
	return err
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
		if g.rangeChangeOwner == conn {
			g.rangeChangeOwner = nil
		}
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
			file, err = g.handle(opCtx, conn, uid, in)
			g.pruneCertificates(conn, uid)
			g.mu.Unlock()
		}
		cancel()
		out := response{Status: status, Certificate: certificate}
		if err != nil {
			out.Error = err.Error()
			if errors.Is(err, ErrPortInUse) {
				out.ErrorCode = "port_in_use"
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
	owners := make(map[string]struct{})
	for _, lease := range g.leases {
		owners[lease.uid] = struct{}{}
	}
	domain := splitdns.BrowserSuffix
	trustReady := g.trustReady
	renewal := g.ca != nil && !g.ca.ExpiresAt().After(time.Now().Add(localCARenewalWindow))
	if len(owner) > 0 {
		var err error
		domain, err = selectedBrowserDomain(g.cfg.StateDir, owner[0])
		if err != nil {
			trustReady = false
		}
		if err == nil && domain != splitdns.BrowserSuffix {
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
	return &RuntimeStatus{BrowserDomain: domain, RootRenewalRequired: renewal, TrustReady: trustReady, Version: buildinfo.Version, LoopbackCIDR: g.cfg.LoopbackCIDR, Ready: true, ActiveLeases: len(g.leases), ActiveOwners: len(owners), ProtectedLoopbackCIDRs: append([]string(nil), g.cfg.ProtectedLoopbackCIDRs...)}
}
func validName(host string) bool {
	if host == splitdns.BrowserGatewayHostname {
		return true
	}
	suffix := "." + splitdns.BrowserSuffix
	if len(host) > 253 || host != strings.ToLower(host) || !strings.HasSuffix(host, suffix) {
		return false
	}
	labels := strings.Split(strings.TrimSuffix(host, suffix), ".")
	if len(labels) != 1 || len(labels[0]) < 1 || len(labels[0]) > 63 || strings.HasPrefix(labels[0], "-") || strings.HasSuffix(labels[0], "-") {
		return false
	}
	for _, r := range labels[0] {
		if r != '-' && (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

func validSubdomainName(host string) bool {
	parts := strings.Split(host, ".")
	if len(parts) != 5 || host != strings.ToLower(host) || !validName(strings.Join(parts[1:], ".")) || strings.Join(parts[1:], ".") == splitdns.BrowserGatewayHostname {
		return false
	}
	port, err := strconv.Atoi(parts[0])
	return err == nil && port > 0 && port <= 65535 && parts[0] == strconv.Itoa(port)
}

func validAddress(ip string) bool { return validAddressInCIDR(ip, machineloopback.DefaultCIDR) }

func validProtectedBindAddress(address, cidr string) bool {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	return validAddressInCIDR(host, cidr) || host == splitdns.BrowserGatewayIP && (port == "80" || port == "443")
}

func protectedLoopbackCIDRs(cfg Config) []string {
	// Earlier releases let users select a projection range. Keep those historical
	// prefixes default-denied after migrating active forwarding to 127.100/16.
	values := append([]string{cfg.LoopbackCIDR, machineloopback.DefaultCIDR}, cfg.ProtectedLoopbackCIDRs...)
	seen := make(map[string]bool)
	out := make([]string, 0, len(values))
	for _, value := range values {
		if clean, err := machineloopback.NormalizeCIDR(value); err == nil && !seen[clean] {
			seen[clean] = true
			out = append(out, clean)
		}
	}
	return out
}

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

// validOwnedLoopbackAddress is used only to retire addresses recorded in the
// guard's protected alias journal. It accepts retained historical ranges and
// the old resolver's .1 address so cleanup can remove only recorded aliases.
func validOwnedLoopbackAddress(ip string, cfg Config) bool {
	address, err := netip.ParseAddr(ip)
	if err != nil || !address.Is4() {
		return false
	}
	octets := address.As4()
	if octets[3] == 0 || octets[3] == 255 {
		return false
	}
	for _, value := range protectedLoopbackCIDRs(cfg) {
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
	case "prepare_range":
		if !canReconfigureRange(conn, uid) {
			return nil, errors.New("machine loopback range changes require administrator authorization")
		}
		clean, err := machineloopback.NormalizeCIDR(in.LoopbackCIDR)
		if err != nil {
			return nil, err
		}
		if clean != machineloopback.DefaultCIDR {
			return nil, errors.New("machine loopback range is fixed at 127.100.0.0/16")
		}
		if clean == g.cfg.LoopbackCIDR {
			return nil, nil
		}
		if g.rangeChangeOwner != nil && g.rangeChangeOwner != conn {
			return nil, errors.New("another machine loopback range change is already pending")
		}
		if len(g.leases) > 0 || len(g.connections) > 1 {
			return nil, fmt.Errorf("cannot change machine loopback CIDR while %d protected listeners or other local daemon connections are active", len(g.leases))
		}
		g.rangeChangeOwner = conn
		return nil, nil
	case "acquire":
		if g.rangeChangeOwner != nil && g.rangeChangeOwner != conn {
			return nil, errors.New("machine loopback range change is pending")
		}
		gateway := in.Hostname == splitdns.BrowserGatewayHostname && in.IP == splitdns.BrowserGatewayIP && (in.Port == 80 || in.Port == 443)
		if !gateway && (!validName(in.Hostname) || in.Hostname == splitdns.BrowserGatewayHostname || !validAddressInCIDR(in.IP, g.cfg.LoopbackCIDR) || in.Port < 1 || in.Port > 65535) {
			return nil, errors.New("invalid protected machine listener")
		}

		for _, aliases := range g.aliases {
			if _, exists := aliases[in.Hostname]; exists {
				return nil, errors.New("machine name conflicts with an active browser alias")
			}
		}
		ownedIPs, ownedNames := 0, 0
		for _, owner := range g.reserved.IPs {
			if owner == uid {
				ownedIPs++
			}
		}
		for _, owner := range g.reserved.Names {
			if owner == uid {
				ownedNames++
			}
		}
		if len(g.leases) >= 8192 || !has(g.reserved.IPs, in.IP) && (len(g.reserved.IPs) >= 65533 || ownedIPs >= 512) || !has(g.reserved.Names, in.Hostname) && (len(g.reserved.Names) >= 4096 || ownedNames >= 512) {
			return nil, errors.New("machine guard reservation limit reached for this OS user")
		}

		for _, owner := range []struct {
			value  string
			exists bool
		}{{g.reserved.IPs[in.IP], has(g.reserved.IPs, in.IP)}, {g.reserved.Names[in.Hostname], has(g.reserved.Names, in.Hostname)}} {
			if owner.exists && owner.value != uid {
				return nil, errors.New("machine name or address belongs to another OS user; choose a nonconflicting installation")
			}
		}
		if _, exists := g.leases[key]; exists {
			return nil, errors.New("protected listener is already owned by another connection")
		}
		g.reserved.IPs[in.IP] = uid
		g.reserved.Names[in.Hostname] = uid
		if err := g.persist(); err != nil {
			return nil, err
		}
		listener, err := listenProtected(ctx, key, g.cfg.LoopbackCIDR)
		if err != nil {
			return nil, fmt.Errorf("create protected listener: %w", err)
		}
		g.leases[key] = &guardedLease{hostname: in.Hostname, ip: in.IP, port: in.Port, uid: uid, owner: conn, listener: listener}
		if err = g.applyRules(ctx); err != nil {
			delete(g.leases, key)
			listener.Close()
			return nil, err
		}
		return listener, nil
	case "release":
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
		_ = lease.listener.Close()
		if err := errors.Join(shutdownErr, updateErr); err != nil {
			g.fail(err)
			return nil, err
		}
		return nil, nil
	case "aliases":
		return nil, g.replaceAliases(ctx, conn, uid, in.Names)
	case "names":
		if len(in.Names) > 128 {
			return nil, errors.New("too many machine names")
		}
		for name, ip := range in.Names {
			if (!validName(name) && !validSubdomainName(name)) || !validAddressInCIDR(ip, g.cfg.LoopbackCIDR) {
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
				return nil, errors.New("machine name has no protected ready listener")
			}
			for other, names := range g.names {
				if other != conn {
					if _, ok := names[name]; ok {
						return nil, errors.New("machine hostname already published by another connection")
					}
				}
			}
		}
		previous := g.names[conn]
		if maps.Equal(previous, in.Names) {
			return nil, nil
		}
		g.names[conn] = in.Names
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
	for _, listener := range retired {
		listener.Close()
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
	return applyProtection(ctx, g.cfg, leases)
}

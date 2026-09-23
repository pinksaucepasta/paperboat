//go:build linux || darwin || windows

package deviceguard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/miekg/dns"
	"github.com/pinksaucepasta/paperboat/internal/deviceloopback"
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
	dnsView          map[string]string
	wg               sync.WaitGroup
	closing          bool
	failures         chan error
	certificates     map[controlConn]map[string]cachedCertificate
	certificateGate  chan struct{}
	rangeChangeOwner controlConn
}

func Serve(ctx context.Context, cfg Config) (resultErr error) {
	if strings.TrimSpace(cfg.LoopbackCIDR) == "" {
		state, err := loadLoopbackState(cfg.StateDir)
		if err != nil {
			return fmt.Errorf("load protected loopback ranges: %w", err)
		}
		cfg.LoopbackCIDR = state.Active
		cfg.ProtectedLoopbackCIDRs = state.Protected
	}
	prefix, err := deviceloopback.Prefix(cfg.LoopbackCIDR)
	if err != nil {
		return fmt.Errorf("device loopback CIDR: %w", err)
	}
	cfg.LoopbackCIDR = prefix.String()
	if err := requirePrivilege(); err != nil {
		return err
	}
	if cfg.Socket == "" {
		cfg.Socket = DefaultSocket
	}
	if cfg.StateDir == "" {
		cfg.StateDir = DefaultStateDir
	}
	if cfg.DNSAddress == "" {
		dnsAddress, _ := deviceloopback.DNSAddress(cfg.LoopbackCIDR)
		cfg.DNSAddress = net.JoinHostPort(dnsAddress.String(), defaultDNSPort)
	}
	if err := protectedDirectory(cfg.StateDir, 0700); err != nil {
		return err
	}
	lock, err := lockState(filepath.Join(cfg.StateDir, "lock"))
	if err != nil {
		return err
	}
	defer lock.Close()
	g := &guardServer{cfg: cfg, reserved: reservations{IPs: map[string]string{}, Names: map[string]string{}}, leases: map[string]*guardedLease{}, names: map[controlConn]map[string]string{}, connections: map[controlConn]string{}, dnsView: map[string]string{}, failures: make(chan error, 1), certificates: map[controlConn]map[string]cachedCertificate{}}
	if data, err := os.ReadFile(filepath.Join(cfg.StateDir, "reservations.json")); err == nil {
		if len(data) > 8<<20 || json.Unmarshal(data, &g.reserved) != nil || g.reserved.IPs == nil || g.reserved.Names == nil {
			return errors.New("invalid device guard reservation journal")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	defer retirePlatform(cfg)
	if err = g.applyRules(ctx); err != nil {
		return err
	}
	// Default-deny rules intentionally survive process exit and are restored before
	// user sessions at boot. Removing DNS does not invalidate application caches.
	listener, err := listenControl(cfg.Socket)
	if err != nil {
		return err
	}
	defer listener.Close()
	packet, err := net.ListenPacket("udp4", cfg.DNSAddress)
	if err != nil {
		return err
	}
	defer packet.Close()
	tcp, err := net.Listen("tcp4", cfg.DNSAddress)
	if err != nil {
		return err
	}
	defer tcp.Close()
	dnsUDP := &dns.Server{PacketConn: packet, Handler: dns.HandlerFunc(g.answerDNS), ReadTimeout: 2 * time.Second, WriteTimeout: 2 * time.Second}
	dnsTCP := &dns.Server{Listener: tcp, Handler: dns.HandlerFunc(g.answerDNS), ReadTimeout: 2 * time.Second, WriteTimeout: 2 * time.Second, MaxTCPQueries: 32}
	dnsErrors := make(chan error, 2)
	go func() { dnsErrors <- dnsUDP.ActivateAndServe() }()
	go func() { dnsErrors <- dnsTCP.ActivateAndServe() }()
	defer dnsUDP.Shutdown()
	defer dnsTCP.Shutdown()
	if cfg.ConfigureResolver {
		if err = setupResolver(ctx, cfg); err != nil {
			return err
		}
		defer retireResolver(cfg)
	}
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
	case err = <-dnsErrors:
		if err == nil {
			err = errors.New("device guard DNS stopped unexpectedly")
		}
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
		if clean, cleanErr := deviceloopback.NormalizeCIDR(state.Active); cleanErr == nil {
			state.Active = clean
			seen := map[string]bool{clean: true}
			valid := []string{clean}
			for _, value := range state.Protected {
				value, cleanErr = deviceloopback.NormalizeCIDR(value)
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
		if clean, cleanErr := deviceloopback.NormalizeCIDR(strings.TrimSpace(string(data))); cleanErr == nil {
			return clean, nil
		}
		return "", errors.New("invalid legacy protected loopback range state")
	}
	if !os.IsNotExist(err) {
		return "", err
	}
	return deviceloopback.DefaultCIDR, nil
}

func storeLoopbackCIDR(stateDir, cidr string) error {
	clean, err := deviceloopback.NormalizeCIDR(cidr)
	if err != nil {
		return err
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
		normalized, err := deviceloopback.NormalizeCIDR(value)
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
		return loopbackState{}, errors.New("device loopback protected range limit reached")
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
	clean, err := deviceloopback.NormalizeCIDR(cidr)
	if err != nil {
		return "", nil, err
	}
	client, err := Connect(ctx, DefaultSocket)
	if err != nil {
		return clean, nil, nil
	}
	status, err := client.Status(ctx)
	if err != nil {
		client.Close()
		return "", nil, fmt.Errorf("inspect active device guard before range change: %w", err)
	}
	if status.LoopbackCIDR != clean {
		if err = client.PrepareRangeChange(ctx, clean); err != nil {
			client.Close()
			return "", nil, err
		}
	}
	return clean, client, nil
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
	g.rebuildDNS()
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
		delete(g.certificates, conn)
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
		var certificate *CertificateBundle
		var status *RuntimeStatus
		if in.Operation == "status" {
			status = g.runtimeStatus()
		} else if in.Operation == "certificate" {
			certificate, err = g.certificate(opCtx, conn, uid, in.Hostname)
		} else {
			g.mu.Lock()
			file, err = g.handle(opCtx, conn, uid, in)
			g.mu.Unlock()
		}
		cancel()
		out := response{Certificate: certificate, Status: status}
		if err != nil {
			out.Error = err.Error()
		}
		if err := conn.Send(out, file); err != nil {
			return
		}

	}
}

func (g *guardServer) runtimeStatus() *RuntimeStatus {
	g.mu.Lock()
	defer g.mu.Unlock()
	owners := make(map[string]struct{})
	for _, lease := range g.leases {
		owners[lease.uid] = struct{}{}
	}
	return &RuntimeStatus{LoopbackCIDR: g.cfg.LoopbackCIDR, DNSAddress: g.cfg.DNSAddress, Ready: true, ActiveLeases: len(g.leases), ActiveOwners: len(owners), ProtectedLoopbackCIDRs: append([]string(nil), g.cfg.ProtectedLoopbackCIDRs...)}
}
func validName(host string) bool {
	if len(host) > 253 || host != strings.ToLower(host) {
		return false
	}
	labels := strings.Split(host, ".")
	if len(labels) != 2 || len(labels[0]) < 1 || len(labels[0]) > 63 || strings.HasPrefix(labels[0], "-") || strings.HasSuffix(labels[0], "-") {
		return false
	}
	for _, r := range labels[0] {
		if r != '-' && (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return false
		}
	}
	_, err := splitdns.ValidateSuffix(labels[1])
	return err == nil
}
func validAddress(ip string) bool { return validAddressInCIDR(ip, deviceloopback.DefaultCIDR) }

func protectedLoopbackCIDRs(cfg Config) []string {
	values := append([]string{cfg.LoopbackCIDR}, cfg.ProtectedLoopbackCIDRs...)
	seen := make(map[string]bool)
	out := make([]string, 0, len(values))
	for _, value := range values {
		if clean, err := deviceloopback.NormalizeCIDR(value); err == nil && !seen[clean] {
			seen[clean] = true
			out = append(out, clean)
		}
	}
	return out
}

func validAddressInCIDR(ip, cidr string) bool {
	if strings.TrimSpace(cidr) == "" {
		cidr = deviceloopback.DefaultCIDR
	}
	address, err := netip.ParseAddr(ip)
	prefix, prefixErr := deviceloopback.Prefix(cidr)
	if err != nil || prefixErr != nil || !address.Is4() || !prefix.Contains(address) {
		return false
	}
	b := address.As4()
	dnsAddress, _ := deviceloopback.DNSAddress(cidr)
	return b[3] > 0 && b[3] < 255 && address != dnsAddress
}
func (g *guardServer) handle(ctx context.Context, conn controlConn, uid string, in request) (net.Listener, error) {
	key := net.JoinHostPort(in.IP, strconv.Itoa(in.Port))
	switch in.Operation {
	case "prepare_range":
		if !canReconfigureRange(conn, uid) {
			return nil, errors.New("device loopback range changes require administrator authorization")
		}
		clean, err := deviceloopback.NormalizeCIDR(in.LoopbackCIDR)
		if err != nil {
			return nil, err
		}
		if clean == g.cfg.LoopbackCIDR {
			return nil, nil
		}
		if g.rangeChangeOwner != nil && g.rangeChangeOwner != conn {
			return nil, errors.New("another device loopback range change is already pending")
		}
		if len(g.leases) > 0 || len(g.connections) > 1 {
			return nil, fmt.Errorf("cannot change device loopback CIDR while %d protected listeners or other local daemon connections are active", len(g.leases))
		}
		g.rangeChangeOwner = conn
		return nil, nil
	case "acquire":
		if g.rangeChangeOwner != nil && g.rangeChangeOwner != conn {
			return nil, errors.New("device loopback range change is pending")
		}
		if !validName(in.Hostname) || !validAddressInCIDR(in.IP, g.cfg.LoopbackCIDR) || in.Port < 1 || in.Port > 65535 {
			return nil, errors.New("invalid protected device listener")
		}

		for _, aliases := range g.aliases {
			if _, exists := aliases[in.Hostname]; exists {
				return nil, errors.New("device name conflicts with an active browser alias")
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
			return nil, errors.New("device guard reservation limit reached for this OS user")
		}

		for _, owner := range []struct {
			value  string
			exists bool
		}{{g.reserved.IPs[in.IP], has(g.reserved.IPs, in.IP)}, {g.reserved.Names[in.Hostname], has(g.reserved.Names, in.Hostname)}} {
			if owner.exists && owner.value != uid {
				return nil, errors.New("device name or address belongs to another OS user; choose a nonconflicting installation")
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
			return nil, errors.New("protected port conflicts with an existing listener")
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
		namesChanged := g.removeUnservedNames(conn)
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		shutdownErr := shutdownListener(lease.listener)
		updateErr := g.applyRules(cleanupCtx)
		var resolverErr error
		if namesChanged && g.cfg.ConfigureResolver {
			resolverErr = g.configureDomains(cleanupCtx)
		}
		_ = lease.listener.Close()
		if err := errors.Join(shutdownErr, updateErr, resolverErr); err != nil {
			g.fail(err)
			return nil, err
		}
		return nil, nil
	case "aliases":
		return nil, g.replaceAliases(ctx, conn, uid, in.Names)
	case "names":
		if len(in.Names) > 128 {
			return nil, errors.New("too many device names")
		}
		for name, ip := range in.Names {
			if !validName(name) || !validAddressInCIDR(ip, g.cfg.LoopbackCIDR) {
				return nil, errors.New("invalid device name")
			}
			found := false
			for _, lease := range g.leases {
				if lease.owner == conn && g.leaseServesName(conn, lease, name) && lease.ip == ip {
					found = true
					break
				}
			}
			if !found {
				return nil, errors.New("device name has no protected ready listener")
			}
			for other, names := range g.names {
				if other != conn {
					if _, ok := names[name]; ok {
						return nil, errors.New("device hostname already published by another connection")
					}
				}
			}
		}
		previous := g.names[conn]
		if maps.Equal(previous, in.Names) {
			return nil, nil
		}
		g.names[conn] = in.Names
		if g.cfg.ConfigureResolver {
			if err := g.configureDomains(ctx); err != nil {
				g.names[conn] = previous
				return nil, err
			}
		}
		g.rebuildDNS()
		return nil, nil
	default:
		return nil, errors.New("unknown device guard operation")
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
	hadNames := len(g.names[conn]) > 0
	delete(g.names, conn)
	delete(g.aliases, conn)
	g.rebuildDNS()
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
	if hadNames && g.cfg.ConfigureResolver {
		if err := g.configureDomains(cleanupCtx); err != nil {
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
	g.rebuildDNS()
	return changed
}
func (g *guardServer) rebuildDNS() {
	g.dnsView = map[string]string{}
	for _, names := range g.names {
		for name, ip := range names {
			g.dnsView[name] = ip
		}
	}
}
func (g *guardServer) answerDNS(writer dns.ResponseWriter, in *dns.Msg) {
	out := new(dns.Msg)
	out.SetReply(in)
	out.Authoritative = true
	if len(in.Question) != 1 {
		out.Rcode = dns.RcodeFormatError
		_ = writer.WriteMsg(out)
		return
	}
	q := in.Question[0]
	name := strings.TrimSuffix(strings.ToLower(q.Name), ".")
	g.mu.Lock()
	ip := g.dnsView[name]
	g.mu.Unlock()
	if ip == "" {
		out.Rcode = dns.RcodeNameError
	} else if q.Qtype == dns.TypeA {
		out.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 1}, A: net.ParseIP(ip)}}
	}
	_ = writer.WriteMsg(out)
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
func (g *guardServer) configureDomains(ctx context.Context) error {
	seen := map[string]bool{}
	for _, names := range g.names {
		for name := range names {
			parts := strings.Split(name, ".")
			seen[parts[len(parts)-1]] = true
		}
	}
	domains := []string{}
	for domain := range seen {
		domains = append(domains, domain)
	}
	sort.Strings(domains)
	return configureDomains(ctx, g.cfg, domains)
}

package native_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/binary"
	"net"
	"net/netip"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/peernetwork"

	"github.com/pinksaucepasta/paperboat/internal/peertransport/mesh"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/native"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/peerquic"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
	"github.com/pinksaucepasta/paperboat/relay/derpquic"
	"github.com/pinksaucepasta/paperboat/relay/peerrelay"
	"tailscale.com/net/packet"
	"tailscale.com/types/key"
	"tailscale.com/wgengine/magicsock"
)

type directFilter struct {
	blockHandshakes atomic.Bool
	relayPort       uint16
	enabled         atomic.Bool
	writes          atomic.Uint64
	directData      atomic.Uint64
	relayData       atomic.Uint64
	relayHandshake  atomic.Uint64
	derpData        atomic.Uint64
	allowDERP       atomic.Bool
	dropped         atomic.Uint64
}

// Retain real authenticated discovery/control while making DERP data unavailable.
// Successful application traffic must therefore use the authorized UDP relay.
type controlOnlyCarrier struct {
	magicsock.DERPCarrier
	filter *directFilter
}

func (c controlOnlyCarrier) Send(k key.NodePublic, p []byte) error {
	if c.filter.blockHandshakes.Load() && len(p) >= 4 && binary.LittleEndian.Uint32(p) == 1 {
		return nil
	}
	if c.filter.allowDERP.Load() || mesh.IsMeowPacket(p) {
		var counter *atomic.Uint64
		if len(p) >= 4 && binary.LittleEndian.Uint32(p) == 4 {
			counter = &c.filter.derpData
		}
		_, err := countedWrite(counter, len(p), func() (int, error) { return len(p), c.DERPCarrier.Send(k, p) })
		return err
	}
	return nil
}
func (c controlOnlyCarrier) SendControl(k key.NodePublic, p []byte) error {
	return c.DERPCarrier.(magicsock.DERPControlSender).SendControl(k, p)
}
func (f *directFilter) wrapCarrier(c magicsock.DERPCarrier) magicsock.DERPCarrier {
	return controlOnlyCarrier{c, f}
}

func (f *directFilter) ListenPacket(ctx context.Context, network, address string) (net.PacketConn, error) {
	pc, err := (&net.ListenConfig{}).ListenPacket(ctx, network, address)
	if err != nil {
		return nil, err
	}
	udp, ok := pc.(*net.UDPConn)
	if !ok {
		_ = pc.Close()
		return nil, net.ErrClosed
	}
	return &filteredPacketConn{UDPConn: udp, filter: f}, nil
}

type filteredPacketConn struct {
	*net.UDPConn
	filter *directFilter
}

func (c *filteredPacketConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	port := 0
	if udp, ok := addr.(*net.UDPAddr); ok {
		port = udp.Port
	}
	return c.write(p, uint16(port), func() (int, error) { return c.UDPConn.WriteTo(p, addr) })
}

func (c *filteredPacketConn) WriteToUDPAddrPort(p []byte, addr netip.AddrPort) (int, error) {
	return c.write(p, addr.Port(), func() (int, error) { return c.UDPConn.WriteToUDPAddrPort(p, addr) })
}

func (c *filteredPacketConn) write(p []byte, port uint16, write func() (int, error)) (int, error) {
	if port == c.filter.relayPort {
		return countedWrite(c.filter.relayCounter(p), len(p), write)
	}
	if !c.filter.enabled.Load() {
		c.filter.dropped.Add(1)
		return len(p), nil
	}
	c.filter.writes.Add(1)
	var counter *atomic.Uint64
	if len(p) >= 4 && binary.LittleEndian.Uint32(p) == 4 {
		counter = &c.filter.directData
	}
	return countedWrite(counter, len(p), write)
}

// countedWrite counts p before it leaves. Counting after the syscall races the
// test: the peer can answer and the check run before the sending goroutine is
// scheduled again (seen: echo received 60us before the server's count). A
// failed or short write takes its count back.
func countedWrite(counter *atomic.Uint64, want int, write func() (int, error)) (int, error) {
	if counter == nil {
		return write()
	}
	counter.Add(1)
	n, err := write()
	if err != nil || n != want {
		counter.Add(^uint64(0))
	}
	return n, err
}

// Discovery probes also traverse the relay; only WireGuard transport data
// demonstrates that an endpoint selected it for application traffic.
func (f *directFilter) relayCounter(p []byte) *atomic.Uint64 {
	var header packet.GeneveHeader
	if len(p) < packet.GeneveFixedHeaderLength+32 || header.Decode(p) != nil || header.Control || header.Protocol != packet.GeneveProtocolWireGuard {
		return nil
	}
	switch binary.LittleEndian.Uint32(p[packet.GeneveFixedHeaderLength:]) {
	case 1, 2, 3:
		return &f.relayHandshake
	case 4:
		return &f.relayData
	}
	return nil
}

func TestAutomaticPeerRelayThenDirectRecovery(t *testing.T) { testAutomaticPeerRelay(t) }

// Under CPU contention the peer relay path is found only after a fresh peer's
// first WireGuard initiation, which wireguard-go repeats after 5s. Retiring the
// client after every one-second QUIC attempt discarded that path, so a peer
// reachable only through a peer relay was never reached (CI, 2026-11-04).
func TestPeerRelayOnlyDialUnderCPUStarvation(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
		}
	}()
	testAutomaticPeerRelay(t)
}

func testAutomaticPeerRelay(t *testing.T) {
	if testing.Short() {
		t.Skip("real UDP peer relay integration")
	}
	t.Setenv("IN_TS_TEST", "true")
	// Setup plus the fault phases' own bounds (20+35+8+8+35s); each phase reports its own failure.
	ctx, cancel := context.WithTimeout(t.Context(), 150*time.Second)
	defer cancel()

	signerPublic, signerPrivate, _ := ed25519.GenerateKey(rand.Reader)
	clientTLS, clientFingerprint := testTLS(t, "peerrelay-cli")
	serverTLS, serverFingerprint := testTLS(t, "peerrelay-machine")
	clientBinding := tailnet.NetworkBinding{AccountID: "account_slice", EndpointID: "cli_slice", Role: "cli", EndpointGeneration: 1, QUICCertificateFingerprint: clientFingerprint, VirtualAddress: "fd7a:115c:a1e0::11"}
	serverBinding := tailnet.NetworkBinding{AccountID: "account_slice", EndpointID: "machine_slice", Role: "machine", MachineID: "machine_slice", EndpointGeneration: 1, MachineGeneration: 1, QUICCertificateFingerprint: serverFingerprint, VirtualAddress: "fd7a:115c:a1e0::12"}

	portProbe, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	relayPort := uint16(portProbe.LocalAddr().(*net.UDPAddr).Port)
	_ = portProbe.Close()
	clientFilter, serverFilter := &directFilter{relayPort: relayPort}, &directFilter{relayPort: relayPort}
	clientAuthority := testAuthority(t, clientBinding, testKeys{"native_test": signerPublic}, clientFilter)
	serverAuthority := testAuthority(t, serverBinding, testKeys{"native_test": signerPublic}, serverFilter)
	clientBinding.WireGuardPublicKey, clientBinding.KeyGeneration = prepareBinding(t, clientAuthority)
	serverBinding.WireGuardPublicKey, serverBinding.KeyGeneration = prepareBinding(t, serverAuthority)
	clientDisco, err := clientAuthority.DiscoveryPublicKey()
	if err != nil {
		t.Fatal(err)
	}
	serverDisco, err := serverAuthority.DiscoveryPublicKey()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	clientConfig := testConfiguration(now.Unix(), 1, clientBinding, serverBinding, "dial")
	serverConfig := testConfiguration(now.Unix(), 1, serverBinding, clientBinding, "accept")
	applyTestConfiguration(t, clientAuthority, signerPrivate, clientConfig)
	applyTestConfiguration(t, serverAuthority, signerPrivate, serverConfig)

	serviceDisco := key.NewDisco()
	serviceNode := key.NewNode()
	service := peernetwork.RelayDescriptor{WireGuardPublicKey: derpquic.KeyString(serviceNode.Public()), DiscoPublicKey: derpquic.DiscoKeyString(serviceDisco.Public()), VirtualAddress: "fd7a:115c:a1e0::30"}
	peerRelayConfig := peerrelay.Config{Service: service, DiscoPrivate: serviceDisco, Port: relayPort, Addresses: []netip.AddrPort{netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), relayPort)}}
	udp, err := peerrelay.New(peerRelayConfig)
	if err != nil {
		t.Fatal(err)
	}
	var udpMu sync.RWMutex
	defer func() {
		udpMu.RLock()
		current := udp
		udpMu.RUnlock()
		_ = current.Close()
	}()

	relayTLS, _ := testTLS(t, "localhost")
	relayCertificate, _ := x509.ParseCertificate(relayTLS.Certificates[0].Certificate[0])
	roots := x509.NewCertPool()
	roots.AddCert(relayCertificate)
	relaySocket, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer relaySocket.Close()
	const nodeID, epoch = "peerrelay_test", "epoch_test"
	relay, err := derpquic.NewServer(derpquic.Verifier{Issuer: "https://api.example.test", NodeID: nodeID, NodeGeneration: 1, ProcessEpoch: epoch, Keys: map[string]ed25519.PublicKey{"native_test": signerPublic}})
	if err != nil {
		t.Fatal(err)
	}
	var controlCalls, controlErrors atomic.Uint64
	if err = relay.SetControlService(service, func(ctx context.Context, request derpquic.ControlRequest) ([]byte, error) {
		controlCalls.Add(1)
		udpMu.RLock()
		current := udp
		udpMu.RUnlock()
		response, handleErr := current.Handle(ctx, request)
		if handleErr != nil {
			controlErrors.Add(1)
		}
		return response, handleErr
	}); err != nil {
		t.Fatal(err)
	}
	relayCtx, stopRelay := context.WithCancel(ctx)
	relayDone := make(chan error, 1)
	go func() { relayDone <- relay.Serve(relayCtx, relaySocket, relayTLS) }()
	var relayShutdown sync.Once
	shutdownRelay := func() {
		relayShutdown.Do(func() {
			stopRelay()
			_ = relay.Close()
			<-relayDone
			relay.Wait()
		})
	}
	defer shutdownRelay()
	node := tailnet.RegionalNode{NodeID: nodeID, NodeGeneration: 1, ProcessEpoch: epoch, Region: "test", FailureDomain: "test-a", Roles: []string{"relay", "peer_relay"}, Transports: []string{"derp_quic", "peer_relay_udp"}, EndpointHost: "127.0.0.1", EndpointQUICPort: uint16(relaySocket.LocalAddr().(*net.UDPAddr).Port), State: "ready", ObservedAt: now.Unix(), ExpiresAt: now.Add(time.Minute).Unix(), CapacityLimit: 100, CapacityUsed: 1, CapacityObservedAt: now.Unix()}
	grant := func(selfDisco, peerDisco string) func(*peernetwork.RelayGrant) {
		return func(g *peernetwork.RelayGrant) {
			g.DiscoPublicKey = selfDisco
			g.Peers[0].DiscoPublicKey = peerDisco
			g.PeerRelay = &service
		}
	}
	applyRelayAuthority(t, clientAuthority, signerPrivate, clientConfig, clientTLS, node, grant(clientDisco, serverDisco))
	clientRegion := applyConfiguredRelay(t, clientAuthority, clientTLS, roots, node)
	applyRelayAuthority(t, serverAuthority, signerPrivate, serverConfig, serverTLS, node, grant(serverDisco, clientDisco))
	serverRegion := applyConfiguredRelay(t, serverAuthority, serverTLS, roots, node)
	_ = clientRegion

	clientOwner, err := native.NewOwner(native.Config{Authority: clientAuthority, TLS: clientTLS})
	if err != nil {
		t.Fatal(err)
	}
	defer clientOwner.Close()
	serverOwner, err := native.NewOwner(native.Config{Authority: serverAuthority, TLS: serverTLS})
	if err != nil {
		t.Fatal(err)
	}
	defer serverOwner.Close()
	// The signed relay authority lasts 60s, but a starved setup alone takes about that long (CI 41967595134:
	// the relay stopped forwarding 60s in; CI 42943969723: the warm dial outlived the grant, so every later
	// AuthorizePair was denied, Denied:43). Renewing only after a dial or phase succeeds deadlocks on the
	// expiry it must prevent, so renew on the wall clock as the production refresh loop (Authority.Run) does;
	// expiration must not masquerade as a transport recovery failure.
	var renewMu sync.Mutex
	renewRelayAuthority := func() {
		renewMu.Lock()
		defer renewMu.Unlock()
		refreshed := time.Now().UTC().Truncate(time.Second)
		// The control plane advances the network generation on every refresh.
		// Renewing timestamps at the old generation is a conflicting signed grant
		// and correctly causes the relay to reject its next authentication frame.
		clientConfig.Generation++
		serverConfig.Generation++
		applyTestConfiguration(t, clientAuthority, signerPrivate, clientConfig)
		applyTestConfiguration(t, serverAuthority, signerPrivate, serverConfig)
		node.ObservedAt, node.CapacityObservedAt, node.ExpiresAt = refreshed.Unix(), refreshed.Unix(), refreshed.Add(time.Minute).Unix()
		applyRelayAuthority(t, clientAuthority, signerPrivate, clientConfig, clientTLS, node, grant(clientDisco, serverDisco))
		applyRelayAuthority(t, serverAuthority, signerPrivate, serverConfig, serverTLS, node, grant(serverDisco, clientDisco))
	}
	renewDone := make(chan struct{})
	renewCtx, stopRenew := context.WithCancel(ctx)
	go func() {
		defer close(renewDone)
		for ticker := time.NewTicker(15 * time.Second); ; {
			select {
			case <-renewCtx.Done():
				ticker.Stop()
				return
			case <-ticker.C:
				renewRelayAuthority()
			}
		}
	}()
	defer func() { stopRenew(); <-renewDone }()
	var previousClient, previousServer uint64
	repeatTransfer := runRealTerminalAndFileProtocols(t, clientOwner, serverOwner, serverAuthority, serverRegion, func(address mesh.Addr) {
		readyCtx, cancelReady := context.WithTimeout(ctx, 8*time.Second)
		if readyErr := serverAuthority.PrepareRegional(readyCtx, ""); readyErr != nil {
			cancelReady()
			t.Fatalf("machine relay readiness: %v", readyErr)
		}
		cancelReady()
		deadline := time.Now().Add(8 * time.Second)
		for clientFilter.relayData.Load() == 0 || serverFilter.relayData.Load() == 0 {
			if time.Now().After(deadline) {
				t.Fatalf("automatic peer relay data path was not selected: peerrelay=%+v derp=%+v control=%d/%d data=%d/%d", udp.Snapshot(), relay.Snapshot(), controlCalls.Load(), controlErrors.Load(), clientFilter.relayData.Load(), serverFilter.relayData.Load())
			}
			warm, dialErr := clientOwner.Dial(ctx, address, "machine_slice", peerquic.ClassInteractive)
			if dialErr != nil {
				t.Fatalf("warm dial: %v; UDP=%+v DERP=%+v control=%d/%d data=%d/%d handshake=%d/%d dropped=%d/%d", dialErr, udp.Snapshot(), relay.Snapshot(), controlCalls.Load(), controlErrors.Load(), clientFilter.relayData.Load(), serverFilter.relayData.Load(), clientFilter.relayHandshake.Load(), serverFilter.relayHandshake.Load(), clientFilter.dropped.Load(), serverFilter.dropped.Load())
			}
			_ = warm.Close()
			time.Sleep(20 * time.Millisecond)
		}
		previousClient, previousServer = clientFilter.relayData.Load(), serverFilter.relayData.Load()
	}, func(consumer string) {
		sent, received := clientFilter.relayData.Load(), serverFilter.relayData.Load()
		if sent <= previousClient || received <= previousServer {
			t.Fatalf("%s did not traverse peer relay in both directions: before=%d/%d after=%d/%d", consumer, previousClient, previousServer, sent, received)
		}
		previousClient, previousServer = sent, received
	})

	clientFilter.allowDERP.Store(true)
	serverFilter.allowDERP.Store(true)
	oldUDP := udp
	if err := oldUDP.Close(); err != nil {
		t.Fatal(err)
	}
	// magicsock keeps sending only to a relay path it trusts for 6.5s after its
	// last pong (tailscale wgengine/magicsock/magicsock.go trustUDPAddrDuration,
	// endpoint.go addrForSendLocked); QUIC then backs its probes off over that
	// silence, so DERP data can first appear about 8s after the outage.
	derpDeadline := time.Now().Add(20 * time.Second)
	beforeClientDERP, beforeServerDERP := clientFilter.derpData.Load(), serverFilter.derpData.Load()
	var derpErr error
	for time.Now().Before(derpDeadline) {
		attemptCtx, attemptCancel := context.WithTimeout(ctx, time.Second)
		derpErr = repeatTransfer(attemptCtx)
		attemptCancel()
		if derpErr == nil && clientFilter.derpData.Load() > beforeClientDERP && serverFilter.derpData.Load() > beforeServerDERP {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if clientFilter.derpData.Load() <= beforeClientDERP || serverFilter.derpData.Load() <= beforeServerDERP {
		t.Fatalf("peer relay outage did not carry application data over DERP: before=%d/%d after=%d/%d err=%v", beforeClientDERP, beforeServerDERP, clientFilter.derpData.Load(), serverFilter.derpData.Load(), derpErr)
	}
	restartedUDP, err := peerrelay.New(peerRelayConfig)
	if err != nil {
		t.Fatal(err)
	}
	udpMu.Lock()
	udp = restartedUDP
	udpMu.Unlock()
	// Upstream retains a bound relay endpoint for up to 30 seconds before
	// allocating against a restarted service with a new server disco key.
	restartDeadline := time.Now().Add(35 * time.Second)
	var restartErr error
	var recovered bool
	for time.Now().Before(restartDeadline) {
		attemptCtx, attemptCancel := context.WithTimeout(ctx, transferAttemptTimeout)
		beforeClient, beforeServer := clientFilter.relayData.Load(), serverFilter.relayData.Load()
		restartErr = repeatTransfer(attemptCtx)
		attemptCancel()
		if restartErr == nil && restartedUDP.Snapshot().AuthorizedPackets > 0 && clientFilter.relayData.Load() > beforeClient && serverFilter.relayData.Load() > beforeServer {
			recovered = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !recovered {
		t.Fatalf("same-session file transfer did not recover after peer relay restart: stats=%+v err=%v", restartedUDP.Snapshot(), restartErr)
	}

	clientFilter.allowDERP.Store(false)
	serverFilter.allowDERP.Store(false)
	if err := repeatTransfer(ctx); err != nil {
		t.Fatalf("recovered peer relay transfer with DERP data disabled: %v", err)
	}

	clientFilter.enabled.Store(true)
	serverFilter.enabled.Store(true)
	deadline := time.Now().Add(8 * time.Second)
	for clientFilter.writes.Load()+serverFilter.writes.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if clientFilter.writes.Load()+serverFilter.writes.Load() == 0 {
		t.Fatal("direct WireGuard UDP did not recover")
	}
	// Discovery writes are not proof of a direct application path. Require a
	// successful same-session transfer and direct WireGuard data in both directions.
	directDeadline := time.Now().Add(8 * time.Second)
	beforeClientDirect, beforeServerDirect := clientFilter.directData.Load(), serverFilter.directData.Load()
	var directErr error
	var directRecovered bool
	for time.Now().Before(directDeadline) {
		attemptCtx, attemptCancel := context.WithTimeout(ctx, time.Second)
		directErr = repeatTransfer(attemptCtx)
		attemptCancel()
		if directErr == nil && clientFilter.directData.Load() > beforeClientDirect && serverFilter.directData.Load() > beforeServerDirect {
			directRecovered = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !directRecovered {
		t.Fatalf("direct application path did not recover: data=%d/%d control=%d/%d relay=%+v err=%v", clientFilter.directData.Load(), serverFilter.directData.Load(), controlCalls.Load(), controlErrors.Load(), restartedUDP.Snapshot(), directErr)
	}
	// Direct loss rediscovers the relay while the reused session's timers are backed off; the product keeps DERP
	// data on for that tail, and the loop below still requires relay data in both directions.
	clientFilter.allowDERP.Store(true)
	serverFilter.allowDERP.Store(true)
	clientFilter.enabled.Store(false)
	serverFilter.enabled.Store(false)
	beforeClient, beforeServer := clientFilter.relayData.Load(), serverFilter.relayData.Load()
	peerDeadline := time.Now().Add(35 * time.Second)
	var peerErr error
	for time.Now().Before(peerDeadline) {
		attemptCtx, attemptCancel := context.WithTimeout(ctx, transferAttemptTimeout)
		peerErr = repeatTransfer(attemptCtx)
		attemptCancel()
		if peerErr == nil && clientFilter.relayData.Load() > beforeClient && serverFilter.relayData.Load() > beforeServer {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if clientFilter.relayData.Load() <= beforeClient || serverFilter.relayData.Load() <= beforeServer {
		t.Fatalf("direct loss did not recover same-session traffic through peer relay: before=%d/%d after=%d/%d control=%d/%d handshakes=%d/%d dropped=%d/%d relay=%+v derp=%+v err=%v", beforeClient, beforeServer, clientFilter.relayData.Load(), serverFilter.relayData.Load(), controlCalls.Load(), controlErrors.Load(), clientFilter.relayHandshake.Load(), serverFilter.relayHandshake.Load(), clientFilter.dropped.Load(), serverFilter.dropped.Load(), restartedUDP.Snapshot(), relay.Snapshot(), peerErr)
	}
}

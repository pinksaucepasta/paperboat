package native_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/binary"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-relay/derpquic"
	"github.com/pinksaucepasta/paperboat-relay/peerrelay"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/native"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/peerquic"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
	"github.com/tailscale/tailcat"
	"tailscale.com/net/packet"
	"tailscale.com/types/key"
	"tailscale.com/wgengine/magicsock"
)

type directFilter struct {
	relayPort      uint16
	enabled        atomic.Bool
	writes         atomic.Uint64
	relayData      atomic.Uint64
	relayHandshake atomic.Uint64
	derpData       atomic.Uint64
	allowDERP      atomic.Bool
	dropped        atomic.Uint64
}

// Retain real authenticated discovery/control while making DERP data unavailable.
// Successful application traffic must therefore use the authorized UDP relay.
type controlOnlyCarrier struct {
	magicsock.DERPCarrier
	filter *directFilter
}

func (c controlOnlyCarrier) Send(k key.NodePublic, p []byte) error {
	if c.filter.allowDERP.Load() || tailcat.IsMeowPacket(p) {
		err := c.DERPCarrier.Send(k, p)
		if err == nil && len(p) >= 4 && binary.LittleEndian.Uint32(p) == 4 {
			c.filter.derpData.Add(1)
		}
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
	if uint16(port) == c.filter.relayPort {
		n, err := c.UDPConn.WriteTo(p, addr)
		c.countRelayData(p, n, err)
		return n, err
	}
	if !c.filter.enabled.Load() {
		c.filter.dropped.Add(1)
		return len(p), nil
	}
	c.filter.writes.Add(1)
	return c.UDPConn.WriteTo(p, addr)
}

func (c *filteredPacketConn) WriteToUDPAddrPort(p []byte, addr netip.AddrPort) (int, error) {
	if addr.Port() == c.filter.relayPort {
		n, err := c.UDPConn.WriteToUDPAddrPort(p, addr)
		c.countRelayData(p, n, err)
		return n, err
	}
	if !c.filter.enabled.Load() {
		c.filter.dropped.Add(1)
		return len(p), nil
	}
	c.filter.writes.Add(1)
	return c.UDPConn.WriteToUDPAddrPort(p, addr)
}

// Discovery probes also traverse the relay; only WireGuard transport data
// demonstrates that an endpoint selected it for application traffic.
func (c *filteredPacketConn) countRelayData(p []byte, n int, err error) {
	var header packet.GeneveHeader
	if err == nil && n == len(p) && len(p) >= packet.GeneveFixedHeaderLength+32 && header.Decode(p) == nil && !header.Control && header.Protocol == packet.GeneveProtocolWireGuard {
		switch binary.LittleEndian.Uint32(p[packet.GeneveFixedHeaderLength:]) {
		case 1, 2, 3:
			c.filter.relayHandshake.Add(1)
		case 4:
			c.filter.relayData.Add(1)
		}
	}
}

func TestAutomaticPeerRelayThenDirectRecovery(t *testing.T) {
	if testing.Short() {
		t.Skip("real UDP peer relay integration")
	}
	t.Setenv("IN_TS_TEST", "true")
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
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
	service := derpquic.ServiceDescriptor{WireGuardPublicKey: derpquic.KeyString(serviceNode.Public()), DiscoPublicKey: derpquic.DiscoKeyString(serviceDisco.Public()), VirtualAddress: "fd7a:115c:a1e0::30"}
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
	grant := func(selfDisco, peerDisco string) func(*derpquic.Grant) {
		return func(g *derpquic.Grant) {
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
	var previousClient, previousServer uint64
	repeatTransfer := runRealTerminalAndFileProtocols(t, clientOwner, serverOwner, serverAuthority, serverRegion, func(address tailcat.Addr) {
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
	derpDeadline := time.Now().Add(8 * time.Second)
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
		attemptCtx, attemptCancel := context.WithTimeout(ctx, 750*time.Millisecond)
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
	// A direct probe write precedes endpoint selection. Keep application traffic
	// flowing while magicsock completes the direct path handshake.
	directDeadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(directDeadline) {
		attemptCtx, attemptCancel := context.WithTimeout(ctx, time.Second)
		_ = repeatTransfer(attemptCtx)
		attemptCancel()
		time.Sleep(50 * time.Millisecond)
	}
	clientFilter.enabled.Store(false)
	serverFilter.enabled.Store(false)
	beforeClient, beforeServer := clientFilter.relayData.Load(), serverFilter.relayData.Load()
	peerDeadline := time.Now().Add(35 * time.Second)
	var peerErr error
	for time.Now().Before(peerDeadline) {
		attemptCtx, attemptCancel := context.WithTimeout(ctx, time.Second)
		peerErr = repeatTransfer(attemptCtx)
		attemptCancel()
		if peerErr == nil && clientFilter.relayData.Load() > beforeClient && serverFilter.relayData.Load() > beforeServer {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if clientFilter.relayData.Load() <= beforeClient || serverFilter.relayData.Load() <= beforeServer {
		t.Fatalf("direct loss did not recover same-session traffic through peer relay: before=%d/%d after=%d/%d err=%v", beforeClient, beforeServer, clientFilter.relayData.Load(), serverFilter.relayData.Load(), peerErr)
	}
}

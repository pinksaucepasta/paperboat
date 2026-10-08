package native_test

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-relay/derpquic"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/native"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/peerquic"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/streamauth"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
	"go.uber.org/goleak"
)

type droppingPacketConn struct {
	net.PacketConn
	drop       atomic.Bool
	mu         sync.Mutex
	sources    map[string]bool
	dropSource string
}

func (c *droppingPacketConn) ReadFrom(p []byte) (int, net.Addr, error) {
	for {
		n, addr, err := c.PacketConn.ReadFrom(p)
		if err != nil {
			return n, addr, err
		}
		c.mu.Lock()
		c.sources[addr.String()] = true
		drop := c.drop.Load() || c.dropSource == addr.String()
		c.mu.Unlock()
		if !drop {
			return n, addr, nil
		}
	}
}

func (c *droppingPacketConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	c.mu.Lock()
	drop := c.drop.Load() || c.dropSource == addr.String()
	c.mu.Unlock()
	if drop {
		return len(p), nil
	}
	return c.PacketConn.WriteTo(p, addr)
}

func (c *droppingPacketConn) dropOne() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for source := range c.sources {
		c.dropSource = source
		return true
	}
	return false
}

func (c *droppingPacketConn) clearOne() {
	c.mu.Lock()
	c.dropSource = ""
	c.mu.Unlock()
}

func TestNativeSessionRecoversQUICToWSSAndBack(t *testing.T) {
	if testing.Short() {
		t.Skip("real DERP QUIC/WSS transition integration")
	}
	leaks := goleak.IgnoreCurrent()
	t.Cleanup(func() { goleak.VerifyNone(t, leaks) })
	t.Setenv("IN_TS_TEST", "true")
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()

	pub, signer, _ := ed25519.GenerateKey(nil)
	clientTLS, clientFP := testTLS(t, "wss-client")
	serverTLS, serverFP := testTLS(t, "wss-server")
	clientBinding := tailnet.NetworkBinding{AccountID: "wss-recovery", EndpointID: "wss-client", Role: "cli", EndpointGeneration: 1, QUICCertificateFingerprint: clientFP, VirtualAddress: "fd7a:115c:a1e0::51"}
	serverBinding := tailnet.NetworkBinding{AccountID: "wss-recovery", EndpointID: "wss-server", Role: "machine", MachineID: "wss-server", EndpointGeneration: 1, MachineGeneration: 1, QUICCertificateFingerprint: serverFP, VirtualAddress: "fd7a:115c:a1e0::52"}
	clientAuthority := testAuthority(t, clientBinding, testKeys{"native_test": pub}, &regionalDirectBlock{})
	serverAuthority := testAuthority(t, serverBinding, testKeys{"native_test": pub}, &regionalDirectBlock{})
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
	applyTestConfiguration(t, clientAuthority, signer, clientConfig)
	applyTestConfiguration(t, serverAuthority, signer, serverConfig)

	relayTLS, _ := testTLS(t, "localhost")
	cert, _ := x509.ParseCertificate(relayTLS.Certificates[0].Certificate[0])
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	udp, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dropper := &droppingPacketConn{PacketConn: udp, sources: make(map[string]bool)}
	const nodeID, epoch = "wss-recovery", "epoch-wss-recovery"
	relay, err := derpquic.NewServer(derpquic.Verifier{Issuer: "https://api.example.test", NodeID: nodeID, NodeGeneration: 1, ProcessEpoch: epoch, Keys: map[string]ed25519.PublicKey{"native_test": pub}})
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewUnstartedServer(relay.WSSHandler())
	httpServer.TLS = &tls.Config{Certificates: relayTLS.Certificates, ClientAuth: tls.RequestClientCert, MinVersion: tls.VersionTLS13}
	httpServer.StartTLS()
	defer httpServer.Close()
	relayCtx, stopRelay := context.WithCancel(ctx)
	relayDone := make(chan error, 1)
	go func() { relayDone <- relay.Serve(relayCtx, dropper, relayTLS) }()
	defer func() {
		stopRelay()
		_ = relay.Close()
		_ = udp.Close()
		select {
		case <-relayDone:
		case <-time.After(2 * time.Second):
			t.Error("relay did not stop")
		}
		relay.Wait()
	}()
	_, tcpPortText, _ := net.SplitHostPort(httpServer.Listener.Addr().String())
	tcpPort, _ := strconv.Atoi(tcpPortText)
	node := tailnet.RegionalNode{NodeID: nodeID, NodeGeneration: 1, ProcessEpoch: epoch, Region: "test", FailureDomain: "rack-a", Roles: []string{"relay"}, Transports: []string{"derp_quic", "derp_wss"}, EndpointHost: "127.0.0.1", EndpointQUICPort: uint16(udp.LocalAddr().(*net.UDPAddr).Port), EndpointTCPPort: uint16(tcpPort), State: "ready", ObservedAt: now.Unix(), ExpiresAt: now.Add(2 * time.Minute).Unix(), CapacityLimit: 100, CapacityUsed: 1, CapacityObservedAt: now.Unix()}
	nodes := []tailnet.RegionalNode{node}
	applyRegionalAuthority(t, clientAuthority, signer, clientConfig, clientTLS, clientDisco, serverDisco, nodes)
	applyRegionalAuthority(t, serverAuthority, signer, serverConfig, serverTLS, serverDisco, clientDisco, nodes)
	clientRegions, err := clientAuthority.ConfigureRegionalRelays(regionalTLS(clientTLS, roots))
	if err != nil {
		t.Fatal(err)
	}
	serverRegions, err := serverAuthority.ConfigureRegionalRelays(regionalTLS(serverTLS, roots))
	if err != nil {
		t.Fatal(err)
	}
	if len(clientRegions) != 1 || len(serverRegions) != 1 {
		t.Fatalf("regional shortlist=%d/%d", len(clientRegions), len(serverRegions))
	}

	clientOwner, _ := native.NewOwner(native.Config{Authority: clientAuthority, TLS: clientTLS})
	defer clientOwner.Close()
	serverOwner, _ := native.NewOwner(native.Config{Authority: serverAuthority, TLS: serverTLS})
	defer serverOwner.Close()
	descriptor := startRelayEchoServer(t, serverOwner, serverAuthority, serverRegions[0])
	session, err := clientOwner.Dial(ctx, descriptor, serverBinding.EndpointID, peerquic.ClassInteractive)
	if err != nil {
		t.Fatal(err)
	}
	assertWSSRecoveryBytes(t, session, "over-quic")
	waitLeg := func(want string, limit time.Duration) {
		deadline, nextRefresh := time.Now().Add(limit), time.Now().Add(5*time.Second)
		for time.Now().Before(deadline) {
			if time.Now().After(nextRefresh) {
				tick := time.Now()
				clientConfig.Generation++
				serverConfig.Generation++
				clientConfig.IssuedAt = tick.Unix()
				serverConfig.IssuedAt = tick.Unix()
				applyTestConfiguration(t, clientAuthority, signer, clientConfig)
				applyTestConfiguration(t, serverAuthority, signer, serverConfig)
				nodes[0].ObservedAt, nodes[0].CapacityObservedAt, nodes[0].ExpiresAt = tick.Unix(), tick.Unix(), tick.Add(60*time.Second).Unix()
				applyRegionalAuthority(t, clientAuthority, signer, clientConfig, clientTLS, clientDisco, serverDisco, nodes)
				applyRegionalAuthority(t, serverAuthority, signer, serverConfig, serverTLS, serverDisco, clientDisco, nodes)
				nextRefresh = time.Now().Add(5 * time.Second)
			}
			s := clientAuthority.RegionalStatus()
			if s.LocalTransport == want && s.PeerTransport == want {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("regional transport did not become %s: client=%+v server=%+v", want, clientAuthority.RegionalStatus(), serverAuthority.RegionalStatus())
	}
	waitLeg("derp_quic", 15*time.Second)
	for deadline := time.Now().Add(2 * time.Second); !dropper.dropOne(); {
		if time.Now().After(deadline) {
			t.Fatal("relay observed no UDP source to drop")
		}
		time.Sleep(10 * time.Millisecond)
	}
	waitMixed := func(limit time.Duration) {
		deadline := time.Now().Add(limit)
		for time.Now().Before(deadline) {
			status := clientAuthority.RegionalStatus()
			if status.LocalTransport != "" && status.PeerTransport != "" && status.LocalTransport != status.PeerTransport {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("regional transport did not become mixed: client=%+v server=%+v", clientAuthority.RegionalStatus(), serverAuthority.RegionalStatus())
	}
	waitMixed(15 * time.Second)
	assertWSSRecoveryBytes(t, session, "mixed-quic-wss")
	dropper.drop.Store(true)
	dropper.clearOne()
	waitLeg("derp_wss", 55*time.Second)
	assertWSSRecoveryBytes(t, session, "over-wss")
	dropper.drop.Store(false)
	waitLeg("derp_quic", 25*time.Second)
	assertWSSRecoveryBytes(t, session, "quic-restored")
	dropper.drop.Store(true)
	waitLeg("derp_wss", 25*time.Second)
	assertWSSRecoveryBytes(t, session, "wss-reused")
}

func assertWSSRecoveryBytes(t *testing.T, session *native.Session, value string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	h, _ := streamauth.New("wss-recovery-op", "terminal", "wss-recovery-stream", "credential_terminal", time.Now().Add(time.Minute), 1<<20)
	s, err := session.OpenAuthorized(ctx, h, "grant_test", "terminal")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_ = s.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err = s.Write([]byte(value)); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(value))
	if _, err = io.ReadFull(s, got); err != nil || string(got) != value {
		t.Fatalf("transition bytes=%q err=%v", got, err)
	}
}

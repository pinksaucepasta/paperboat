package native_test

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/native"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/peerquic"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
	"github.com/pinksaucepasta/paperboat/relay/derpquic"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=fault-injection-test
	"tailscale.com/wgengine/magicsock"
)

// pbmac 2026-11-05 (dev-751282416): a daemon whose peer had signed direct endpoints that did not answer looped
// disco pings for 7 minutes and never used the relay. Two relays are granted; the first one is unreachable from
// this machine. The dial must reach the peer through the live relay within a few seconds.
func TestNativeDialFallsBackToTheLiveRelayWhenDirectAndFirstRelayAreDead(t *testing.T) {
	for _, dead := range []string{"a-relay", "z-relay"} {
		for _, silentPeer := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/silent=%t", dead, silentPeer), func(t *testing.T) { testDeadRelayFallback(t, dead, silentPeer) })
		}
	}
}

func testDeadRelayFallback(t *testing.T, deadID string, silentPeer bool) {
	if testing.Short() {
		t.Skip("real DERP QUIC integration")
	}
	t.Setenv("IN_TS_TEST", "true")
	t.Setenv("TS_DEBUG_ALWAYS_USE_DERP", "true") // the signed direct endpoints never answer
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Second)
	defer cancel()

	pub, signer, _ := ed25519.GenerateKey(nil)
	clientTLS, clientFP := testTLS(t, "dead-client")
	serverTLS, serverFP := testTLS(t, "dead-server")
	clientBinding := tailnet.NetworkBinding{AccountID: "dead-pair", EndpointID: "dead-client", Role: "cli", EndpointGeneration: 1, QUICCertificateFingerprint: clientFP, VirtualAddress: "fd7a:115c:a1e0::71"}
	serverBinding := tailnet.NetworkBinding{AccountID: "dead-pair", EndpointID: "dead-server", Role: "machine", MachineID: "dead-server", EndpointGeneration: 1, MachineGeneration: 1, QUICCertificateFingerprint: serverFP, VirtualAddress: "fd7a:115c:a1e0::72"}
	var err error
	clientAuthority := testAuthority(t, clientBinding, testKeys{"native_test": pub})
	serverAuthority := testAuthority(t, serverBinding, testKeys{"native_test": pub})
	if silentPeer {
		// The peer does not answer regional probes, as while its configuration lags this machine's reinstall.
		serverSelf := serverBinding
		if public, ok := testQUICKeys.Load(serverSelf.QUICCertificateFingerprint); ok {
			serverSelf.QUICPublicKey = public.(string)
		}
		serverAuthority, err = tailnet.NewAuthority(tailnet.AuthorityOptions{
			Store: config.ProfileStore{Path: t.TempDir(), Secrets: testSecrets{}}, Issuer: "https://api.example.test", Self: serverSelf,
			Keys: testKeys{"native_test": pub}, TestOnlyDERPCarrier: func(c magicsock.DERPCarrier) magicsock.DERPCarrier { return silentRegionalCarrier{c} },
		})
		if err != nil {
			t.Fatal(err)
		}
	}
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
	// TEST-NET-1: signed candidates that never answer, like a reinstalled peer's stale or unroutable ones.
	clientConfig.Peers[0].Endpoints = []string{"192.0.2.10:41641"}
	serverConfig.Peers[0].Endpoints = []string{"192.0.2.11:41641"}
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
	liveID := "z-relay"
	if deadID == liveID {
		liveID = "a-relay"
	}
	relay, err := derpquic.NewServer(derpquic.Verifier{Issuer: "https://api.example.test", NodeID: liveID, NodeGeneration: 1, ProcessEpoch: "epoch", Keys: map[string]ed25519.PublicKey{"native_test": pub}})
	if err != nil {
		t.Fatal(err)
	}
	relayCtx, stopRelay := context.WithCancel(ctx)
	relayDone := make(chan error, 1)
	go func() { relayDone <- relay.Serve(relayCtx, udp, relayTLS) }()
	defer func() {
		stopRelay()
		_ = relay.Close()
		_ = udp.Close()
		<-relayDone
		relay.Wait()
	}()
	// A bound socket that never answers: QUIC to it times out, like a relay this machine cannot reach.
	silent, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer silent.Close()
	node := func(id string, port int) tailnet.RegionalNode {
		return tailnet.RegionalNode{NodeID: id, NodeGeneration: 1, ProcessEpoch: "epoch", Region: "test", FailureDomain: id, Roles: []string{"relay"}, Transports: []string{"derp_quic"}, EndpointHost: "127.0.0.1", EndpointQUICPort: uint16(port), State: "ready", ObservedAt: now.Unix(), ExpiresAt: now.Add(2 * time.Minute).Unix(), CapacityLimit: 100, CapacityUsed: 1, CapacityObservedAt: now.Unix()}
	}
	nodes := []tailnet.RegionalNode{node(liveID, udp.LocalAddr().(*net.UDPAddr).Port), node(deadID, silent.LocalAddr().(*net.UDPAddr).Port)}
	if nodes[0].NodeID > nodes[1].NodeID {
		nodes[0], nodes[1] = nodes[1], nodes[0]
	}
	applyRegionalAuthority(t, clientAuthority, signer, clientConfig, clientTLS, clientDisco, serverDisco, nodes)
	applyRegionalAuthority(t, serverAuthority, signer, serverConfig, serverTLS, serverDisco, clientDisco, nodes)
	if _, err := clientAuthority.ConfigureRegionalRelays(regionalTLS(clientTLS, roots)); err != nil {
		t.Fatal(err)
	}
	serverRegions, err := serverAuthority.ConfigureRegionalRelays(regionalTLS(serverTLS, roots))
	if err != nil {
		t.Fatal(err)
	}
	clientOwner, _ := native.NewOwner(native.Config{Authority: clientAuthority, TLS: clientTLS})
	defer clientOwner.Close()
	serverOwner, _ := native.NewOwner(native.Config{Authority: serverAuthority, TLS: serverTLS})
	defer serverOwner.Close()
	descriptor := startRelayEchoServer(t, serverOwner, serverAuthority, serverRegions[0])
	started := time.Now()
	// One unreachable relay costs its 5 s probe once; a second round (10.5 s) is a regression.
	dial, stopDial := context.WithTimeout(ctx, 10*time.Second)
	defer stopDial()
	session, err := clientOwner.Dial(dial, descriptor, serverBinding.EndpointID, peerquic.ClassInteractive)
	if err != nil {
		t.Fatalf("no path to the peer after %v although a relay is live: %v", time.Since(started), err)
	}
	defer session.Close()
	assertWSSRecoveryBytes(t, session, "dead-home")
	t.Logf("first echo after %v", time.Since(started))
}

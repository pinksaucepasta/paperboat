package native_test

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"net"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-relay/derpquic"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/native"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/peerquic"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
)

// F9-02/F9-05 device finding: the provisional rendezvous used only the first
// granted relay, so one relay answering "relay authorization rejected" failed
// every native dial (pbvm2 self-access) while another granted relay was healthy.
func TestNativeDialTriesTheNextRegionalRelayWhenTheFirstRefusesAuthorization(t *testing.T) {
	if testing.Short() {
		t.Skip("real DERP/QUIC integration")
	}
	t.Setenv("IN_TS_TEST", "true")
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	pub, signer, _ := ed25519.GenerateKey(nil)
	otherPub, _, _ := ed25519.GenerateKey(nil)
	clientTLS, clientFP := testTLS(t, "refused-client")
	serverTLS, serverFP := testTLS(t, "refused-server")
	clientBinding := tailnet.NetworkBinding{AccountID: "refused", EndpointID: "refused-client", Role: "cli", EndpointGeneration: 1, QUICCertificateFingerprint: clientFP, VirtualAddress: "fd7a:115c:a1e0::51"}
	serverBinding := tailnet.NetworkBinding{AccountID: "refused", EndpointID: "refused-server", Role: "machine", MachineID: "refused-server", EndpointGeneration: 1, MachineGeneration: 1, QUICCertificateFingerprint: serverFP, VirtualAddress: "fd7a:115c:a1e0::52"}
	clientAuthority := testAuthority(t, clientBinding, testKeys{"native_test": pub})
	serverAuthority := testAuthority(t, serverBinding, testKeys{"native_test": pub})
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

	// The first granted relay verifies grants against a different key, so it
	// refuses every one; the second is healthy.
	relayTLS, _ := testTLS(t, "localhost")
	cert, _ := x509.ParseCertificate(relayTLS.Certificates[0].Certificate[0])
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	start := func(id string, key ed25519.PublicKey) tailnet.RegionalNode {
		packetConn, listenErr := net.ListenPacket("udp4", "127.0.0.1:0")
		if listenErr != nil {
			t.Fatal(listenErr)
		}
		epoch := "epoch-" + id
		relay, newErr := derpquic.NewServer(derpquic.Verifier{Issuer: "https://api.example.test", NodeID: id, NodeGeneration: 1, ProcessEpoch: epoch, Keys: map[string]ed25519.PublicKey{"native_test": key}})
		if newErr != nil {
			t.Fatal(newErr)
		}
		relayCtx, stop := context.WithCancel(ctx)
		served := make(chan error, 1)
		go func() { served <- relay.Serve(relayCtx, packetConn, relayTLS) }()
		t.Cleanup(func() { stop(); _ = relay.Close(); _ = packetConn.Close(); <-served; relay.Wait() })
		port := packetConn.LocalAddr().(*net.UDPAddr).Port
		return tailnet.RegionalNode{NodeID: id, NodeGeneration: 1, ProcessEpoch: epoch, Region: "test", FailureDomain: "rack-" + id, Roles: []string{"relay"}, Transports: []string{"derp_quic"}, EndpointHost: "127.0.0.1", EndpointQUICPort: uint16(port), State: "ready", ObservedAt: now.Unix(), ExpiresAt: now.Add(90 * time.Second).Unix(), CapacityLimit: 100, CapacityUsed: 1, CapacityObservedAt: now.Unix()}
	}
	refusing, healthy := start("a-refusing", otherPub), start("b-healthy", pub)
	applyRegionalAuthority(t, clientAuthority, signer, clientConfig, clientTLS, clientDisco, serverDisco, []tailnet.RegionalNode{refusing, healthy})
	applyRegionalAuthority(t, serverAuthority, signer, serverConfig, serverTLS, serverDisco, clientDisco, []tailnet.RegionalNode{healthy})
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
	dialCtx, stopDial := context.WithTimeout(ctx, 20*time.Second)
	defer stopDial()
	session, err := clientOwner.Dial(dialCtx, descriptor, serverBinding.EndpointID, peerquic.ClassInteractive)
	if err != nil {
		t.Fatalf("dial failed although a healthy relay was granted: %v", err)
	}
	assertRegionalBytes(t, ctx, session, "second-relay")
}

package native_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"net"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-relay/derpquic"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/native"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/peerquic"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=fault-injection-test
	"tailscale.com/types/key"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=fault-injection-test
	"tailscale.com/wgengine/magicsock"
)

// silentRegionalCarrier drops this endpoint's regional probe replies, as a host does while its
// signed configuration does not list a just-paired peer yet. Admission and data still pass.
type silentRegionalCarrier struct{ magicsock.DERPCarrier }

func (c silentRegionalCarrier) Send(k key.NodePublic, p []byte) error {
	if bytes.HasPrefix(p, []byte("pb-region\x01")) {
		return nil
	}
	return c.DERPCarrier.Send(k, p)
}

func (c silentRegionalCarrier) SendControl(k key.NodePublic, p []byte) error {
	if bytes.HasPrefix(p, []byte("pb-region\x01")) {
		return nil
	}
	return c.DERPCarrier.(magicsock.DERPControlSender).SendControl(k, p)
}

// A just-paired peer took ~5 s to connect: the client waited the whole regional probe timeout
// for an acknowledged common relay before it prepared one provisionally.
func TestNativeDialToPeerThatHasNotAnsweredRegionalProbesIsFast(t *testing.T) {
	if testing.Short() {
		t.Skip("real DERP QUIC integration")
	}
	t.Setenv("IN_TS_TEST", "true")
	t.Setenv("TS_DEBUG_ALWAYS_USE_DERP", "true")
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	pub, signer, _ := ed25519.GenerateKey(nil)
	clientTLS, clientFP := testTLS(t, "fresh-client")
	serverTLS, serverFP := testTLS(t, "fresh-server")
	clientBinding := tailnet.NetworkBinding{AccountID: "fresh-pair", EndpointID: "fresh-client", Role: "cli", EndpointGeneration: 1, QUICCertificateFingerprint: clientFP, VirtualAddress: "fd7a:115c:a1e0::61"}
	serverBinding := tailnet.NetworkBinding{AccountID: "fresh-pair", EndpointID: "fresh-server", Role: "machine", MachineID: "fresh-server", EndpointGeneration: 1, MachineGeneration: 1, QUICCertificateFingerprint: serverFP, VirtualAddress: "fd7a:115c:a1e0::62"}
	clientAuthority := testAuthority(t, clientBinding, testKeys{"native_test": pub})
	serverSelf := serverBinding
	if public, ok := testQUICKeys.Load(serverSelf.QUICCertificateFingerprint); ok {
		serverSelf.QUICPublicKey = public.(string)
	}
	serverAuthority, err := tailnet.NewAuthority(tailnet.AuthorityOptions{
		Store: config.ProfileStore{Path: t.TempDir(), Secrets: testSecrets{}}, Issuer: "https://api.example.test", Self: serverSelf,
		Keys: testKeys{"native_test": pub}, TestOnlyDERPCarrier: func(c magicsock.DERPCarrier) magicsock.DERPCarrier { return silentRegionalCarrier{c} },
	})
	if err != nil {
		t.Fatal(err)
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
	const nodeID, epoch = "fresh-relay", "epoch-fresh"
	relay, err := derpquic.NewServer(derpquic.Verifier{Issuer: "https://api.example.test", NodeID: nodeID, NodeGeneration: 1, ProcessEpoch: epoch, Keys: map[string]ed25519.PublicKey{"native_test": pub}})
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
	node := tailnet.RegionalNode{NodeID: nodeID, NodeGeneration: 1, ProcessEpoch: epoch, Region: "test", FailureDomain: "rack-a", Roles: []string{"relay"}, Transports: []string{"derp_quic"}, EndpointHost: "127.0.0.1", EndpointQUICPort: uint16(udp.LocalAddr().(*net.UDPAddr).Port), State: "ready", ObservedAt: now.Unix(), ExpiresAt: now.Add(2 * time.Minute).Unix(), CapacityLimit: 100, CapacityUsed: 1, CapacityObservedAt: now.Unix()}
	nodes := []tailnet.RegionalNode{node}
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
	session, err := clientOwner.Dial(ctx, descriptor, serverBinding.EndpointID, peerquic.ClassInteractive)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	assertWSSRecoveryBytes(t, session, "fresh-peer")
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("first connection to a peer that has not answered regional probes took %v, want under 1s", elapsed)
	}
}

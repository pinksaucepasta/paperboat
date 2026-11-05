package native_test

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/nativesession"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/native"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/peerquic"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/streamauth"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
	"github.com/pinksaucepasta/paperboat/relay/derpquic"
)

// A client that connects after the machine moved to a generation (or was not connected during the move)
// reaches the generation from its signed configuration alone: no old connection, no candidate exchange,
// and the old primary already gone.
func TestFreshClientDialsTheGenerationItsConfigurationOffers(t *testing.T) {
	if testing.Short() {
		t.Skip("real two-node DERP/QUIC integration")
	}
	t.Setenv("IN_TS_TEST", "true")
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	pub, signer, _ := ed25519.GenerateKey(nil)
	clientTLS, clientFP := testTLS(t, "fresh-client")
	serverTLS, serverFP := testTLS(t, "fresh-server")
	client := tailnet.NetworkBinding{AccountID: "fresh", EndpointID: "fresh-client", Role: "cli", EndpointGeneration: 1, QUICCertificateFingerprint: clientFP, VirtualAddress: "fd7a:115c:a1e0::51"}
	server := tailnet.NetworkBinding{AccountID: "fresh", EndpointID: "fresh-server", Role: "machine", MachineID: "fresh-server", EndpointGeneration: 1, MachineGeneration: 1, QUICCertificateFingerprint: serverFP, VirtualAddress: "fd7a:115c:a1e0::52"}
	raw, _ := testQUICKeys.Load(serverFP)
	server.QUICPublicKey = raw.(string)
	clientAuthority := testAuthority(t, client, testKeys{"native_test": pub}, &regionalDirectBlock{})
	serverStore := config.ProfileStore{Path: t.TempDir(), Secrets: testSecrets{}}
	serverAuthority, err := tailnet.NewAuthority(tailnet.AuthorityOptions{Store: serverStore, Issuer: "https://api.example.test", Self: server, Keys: testKeys{"native_test": pub}, TestOnlyPacketListener: &regionalDirectBlock{}})
	if err != nil {
		t.Fatal(err)
	}
	client.WireGuardPublicKey, client.KeyGeneration = prepareBinding(t, clientAuthority)
	server.WireGuardPublicKey, server.KeyGeneration = prepareBinding(t, serverAuthority)
	clientDisco, _ := clientAuthority.DiscoveryPublicKey()
	serverDisco, _ := serverAuthority.DiscoveryPublicKey()
	now := time.Now().UTC().Truncate(time.Second)
	clientConfig := testConfiguration(now.Unix(), 1, client, server, "dial")
	serverConfig := testConfiguration(now.Unix(), 1, server, client, "accept")
	applyTestConfiguration(t, clientAuthority, signer, clientConfig)
	applyTestConfiguration(t, serverAuthority, signer, serverConfig)

	relayTLS, _ := testTLS(t, "localhost")
	cert, _ := x509.ParseCertificate(relayTLS.Certificates[0].Certificate[0])
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	relay := startTestRegionalRelay(t, ctx, pub, relayTLS, "fresh-relay", now)
	nodes := []tailnet.RegionalNode{relay.node}
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
	primary, _ := native.NewOwner(native.Config{Authority: serverAuthority, TLS: serverTLS})
	descriptor := startRelayEchoServer(t, primary, serverAuthority, serverRegions[0])

	// The staged process: its own authority over the same custody, and a generation serving everything.
	nextAuthority, err := tailnet.NewAuthority(tailnet.AuthorityOptions{Store: serverStore, Issuer: "https://api.example.test", Self: server, Keys: testKeys{"native_test": pub}, TestOnlyPacketListener: &regionalDirectBlock{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := nextAuthority.Restore(ctx); err != nil {
		t.Fatal(err)
	}
	applyRegionalAuthority(t, nextAuthority, signer, serverConfig, serverTLS, serverDisco, clientDisco, nodes)
	if _, err := nextAuthority.ConfigureRegionalRelays(regionalTLS(serverTLS, roots)); err != nil {
		t.Fatal(err)
	}
	nextOwner, err := native.NewOwner(native.Config{Authority: nextAuthority, TLS: serverTLS})
	if err != nil {
		t.Fatal(err)
	}
	defer nextOwner.Close()
	identity, err := nextOwner.PrepareCarrierIdentity()
	if err != nil {
		t.Fatal(err)
	}
	identity.Offer.Claims.AllApplications = true
	apps, err := nativesession.New(nativesession.Config{Authorize: func(context.Context, streamauth.Header) (string, error) { return "grant_test", nil }, ServeStream: func(_ context.Context, _ streamauth.Header, c net.Conn) error {
		if _, err := c.Write([]byte("G")); err != nil {
			return err
		}
		_, err := io.Copy(c, c)
		return err
	}, ServeTransfer: func(context.Context, net.Conn) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	genCtx, stopGen := context.WithCancel(ctx)
	generation, err := nextOwner.StartCarrierNetwork(genCtx, identity, "", apps.Serve)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { stopGen(); generation.Close(); apps.Wait() }()
	primary.Close() // the update restarts the old primary: nothing may depend on it

	offer, err := identity.Offer.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	clientConfig.Generation++
	clientConfig.IssuedAt = time.Now().Unix()
	clientConfig.Peers[0].CarrierOffer = offer
	applyTestConfiguration(t, clientAuthority, signer, clientConfig)
	for _, consumer := range []string{"exec", "ssh"} {
		started := time.Now()
		fresh, err := clientOwner.Dial(ctx, descriptor, server.EndpointID, peerquic.ClassInteractive)
		if err != nil {
			t.Fatal("fresh dial with an offered generation failed", err)
		}
		stream := openConsumer(t, ctx, fresh, consumer)
		_ = stream.SetDeadline(time.Now().Add(5 * time.Second))
		var mark [1]byte
		if _, err := io.ReadFull(stream, mark[:]); err != nil || mark[0] != 'G' {
			t.Fatal("a fresh connection did not open on the generation", consumer, err)
		}
		carrierEcho(t, stream)
		t.Logf("%s on the generation after %v", consumer, time.Since(started))
		stream.Close()
		fresh.Close()
	}
}

// startTestRegionalRelay is regional_integration_test.go's relay, stopped at cleanup.
func startTestRegionalRelay(t *testing.T, ctx context.Context, pub ed25519.PublicKey, relayTLS *tls.Config, id string, now time.Time) *regionalRelay {
	t.Helper()
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	epoch := "epoch-" + id
	r, err := derpquic.NewServer(derpquic.Verifier{Issuer: "https://api.example.test", NodeID: id, NodeGeneration: 1, ProcessEpoch: epoch, Keys: map[string]ed25519.PublicKey{"native_test": pub}})
	if err != nil {
		t.Fatal(err)
	}
	rctx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- r.Serve(rctx, pc, relayTLS) }()
	shutdown := sync.OnceFunc(func() {
		stop()
		_ = r.Close()
		_ = pc.Close()
		<-done
		r.Wait()
	})
	t.Cleanup(shutdown)
	port := pc.LocalAddr().(*net.UDPAddr).Port
	return &regionalRelay{addr: fmt.Sprintf("127.0.0.1:%d", port), stop: shutdown, node: tailnet.RegionalNode{NodeID: id, NodeGeneration: 1, ProcessEpoch: epoch, Region: "test", FailureDomain: "rack-a", Roles: []string{"relay"}, Transports: []string{"derp_quic"}, EndpointHost: "127.0.0.1", EndpointQUICPort: uint16(port), State: "ready", ObservedAt: now.Unix(), ExpiresAt: now.Add(90 * time.Second).Unix(), CapacityLimit: 100, CapacityUsed: 1, CapacityObservedAt: now.Unix()}}
}

func carrierEcho(t *testing.T, stream net.Conn) {
	t.Helper()
	stream.SetDeadline(time.Now().Add(time.Second))
	if _, err := stream.Write([]byte("unchanged")); err != nil {
		t.Fatal(err)
	}
	var b [9]byte
	if _, err := io.ReadFull(stream, b[:]); err != nil || string(b[:]) != "unchanged" {
		t.Fatal("carrier altered its live stream", err)
	}
}

func openConsumer(t *testing.T, ctx context.Context, s *native.Session, consumer string) net.Conn {
	t.Helper()
	h, err := streamauth.New(consumer, consumer, consumer, "credential_"+consumer, time.Now().Add(time.Minute), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	capability := map[string]string{"exec": "exec", "ssh": "managed_ssh"}[consumer]
	stream, err := s.OpenAuthorized(ctx, h, "grant_test", capability)
	if err != nil {
		t.Fatal(err)
	}
	return stream
}

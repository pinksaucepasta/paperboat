package native_test

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"io"
	"net"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/nativesession"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/native"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/peerquic"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/streamauth"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
)

// pb update: a client reaches the new carrier generation from its signed configuration over the only relay the
// generation knows. That relay then restarts on a new port (same node and process epoch) and the next signed
// refresh arrives in a real refresh's order: configuration, then candidates, then grants. The generation is
// passive (nothing to send), so only the redefined pinned region can bring its relay link back; data on the
// held connection must answer again within a few hundred ms, not after wireguard-go's 5 s rekey
// (c2a657ee4, ad0b80e74: 51-542 ms fixed, 3-5 s without).
func TestDelegatedGenerationNegotiatesAndRenewsItsRegionalRoute(t *testing.T) {
	if testing.Short() {
		t.Skip("real two-node DERP/QUIC integration")
	}
	t.Setenv("IN_TS_TEST", "true")
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	pub, signer, _ := ed25519.GenerateKey(nil)
	clientTLS, clientFP := testTLS(t, "renew-client")
	serverTLS, serverFP := testTLS(t, "renew-server")
	client := tailnet.NetworkBinding{AccountID: "renew", EndpointID: "renew-client", Role: "cli", EndpointGeneration: 1, QUICCertificateFingerprint: clientFP, VirtualAddress: "fd7a:115c:a1e0::61"}
	server := tailnet.NetworkBinding{AccountID: "renew", EndpointID: "renew-server", Role: "machine", MachineID: "renew-server", EndpointGeneration: 1, MachineGeneration: 1, QUICCertificateFingerprint: serverFP, VirtualAddress: "fd7a:115c:a1e0::62"}
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
	a := startTestRegionalRelay(t, ctx, pub, relayTLS, "renew-a", now)
	b := startTestRegionalRelay(t, ctx, pub, relayTLS, "renew-b", now)
	b.node.FailureDomain = "rack-b"
	// The client knows both relays; the machine and its generation only b, so the route is negotiated onto b.
	applyRegionalAuthority(t, clientAuthority, signer, clientConfig, clientTLS, clientDisco, serverDisco, []tailnet.RegionalNode{a.node, b.node})
	applyRegionalAuthority(t, serverAuthority, signer, serverConfig, serverTLS, serverDisco, clientDisco, []tailnet.RegionalNode{b.node})
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
	defer primary.Close()
	descriptor := startRelayEchoServer(t, primary, serverAuthority, serverRegions[0])

	// The update's new generation: its own authority over the same custody, serving every application.
	nextAuthority, err := tailnet.NewAuthority(tailnet.AuthorityOptions{Store: serverStore, Issuer: "https://api.example.test", Self: server, Keys: testKeys{"native_test": pub}, TestOnlyPacketListener: &regionalDirectBlock{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := nextAuthority.Restore(ctx); err != nil {
		t.Fatal(err)
	}
	applyRegionalAuthority(t, nextAuthority, signer, serverConfig, serverTLS, serverDisco, clientDisco, []tailnet.RegionalNode{b.node})
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

	offer, err := identity.Offer.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	clientConfig.Generation++
	clientConfig.IssuedAt = time.Now().Unix()
	clientConfig.Peers[0].CarrierOffer = offer
	applyTestConfiguration(t, clientAuthority, signer, clientConfig)
	session, err := clientOwner.Dial(ctx, descriptor, server.EndpointID, peerquic.ClassInteractive)
	if err != nil {
		t.Fatal("a client did not reach the generation its configuration offers", err)
	}
	defer session.Close()
	stream := openConsumer(t, ctx, session, "exec")
	defer stream.Close()
	_ = stream.SetDeadline(time.Now().Add(5 * time.Second))
	var mark [1]byte
	if _, err := io.ReadFull(stream, mark[:]); err != nil || mark[0] != 'G' {
		t.Fatal("the connection did not open on the generation", err)
	}
	carrierEcho(t, stream)

	// Relay b restarts on a new port; the signed refresh rebinds it, then the old instance stops.
	old := b
	b = startTestRegionalRelay(t, ctx, pub, relayTLS, "renew-b", now)
	b.node.FailureDomain = "rack-b"
	if b.node.EndpointQUICPort == old.node.EndpointQUICPort || b.node.ProcessEpoch != old.node.ProcessEpoch {
		t.Fatal("the relay did not move to a new port under the same process epoch")
	}
	issued := time.Now()
	for _, n := range []*tailnet.RegionalNode{&a.node, &b.node} {
		n.ObservedAt, n.CapacityObservedAt, n.ExpiresAt = issued.Unix(), issued.Unix(), issued.Add(60*time.Second).Unix()
	}
	clientConfig.Generation++
	serverConfig.Generation++
	clientConfig.IssuedAt, serverConfig.IssuedAt = issued.Unix(), issued.Unix()
	applyTestConfiguration(t, clientAuthority, signer, clientConfig)
	applyRegionalAuthority(t, clientAuthority, signer, clientConfig, clientTLS, clientDisco, serverDisco, []tailnet.RegionalNode{a.node, b.node})
	applyTestConfiguration(t, nextAuthority, signer, serverConfig)
	applyRegionalAuthority(t, nextAuthority, signer, serverConfig, serverTLS, serverDisco, clientDisco, []tailnet.RegionalNode{b.node})
	old.stop()
	started := time.Now()
	if err := echoWithin(stream, 2*time.Second); err != nil {
		t.Fatalf("held connection on the generation did not answer within 2 s of the relay rebind: %v", err)
	}
	t.Logf("data again %v after the relay rebind", time.Since(started))
}

// echoWithin writes one probe and waits up to limit for its echo; a lost packet is retransmitted by QUIC.
func echoWithin(stream net.Conn, limit time.Duration) error {
	_ = stream.SetDeadline(time.Now().Add(limit))
	defer stream.SetDeadline(time.Time{})
	if _, err := stream.Write([]byte("renewed")); err != nil {
		return err
	}
	got := make([]byte, len("renewed"))
	if _, err := io.ReadFull(stream, got); err != nil {
		return err
	}
	if string(got) != "renewed" {
		return io.ErrUnexpectedEOF
	}
	return nil
}

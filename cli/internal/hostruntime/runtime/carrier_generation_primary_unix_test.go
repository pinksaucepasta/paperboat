//go:build linux || darwin

package runtime

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"encoding/base64"
	"net"
	"net/netip"
	"path/filepath"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/session"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workloadbridge"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/native"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/peerquic"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
)

// pb update, then the old primary exits: the delegated generation takes the
// machine's primary network in place. A stream open on its delegated network
// keeps working, it reports the machine's endpoints, and a client that holds
// no offer reaches it by the machine identity alone.
func TestDelegatedCarrierGenerationTakesThePrimaryNetwork(t *testing.T) {
	if !targetCarrierRoot(t) {
		return
	}
	f := newTargetCarrierFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	if _, err := f.client.TerminalCarrier(ctx, workloadbridge.CarrierRequest{Operation: "start", Slot: 1}); err != nil {
		t.Fatal(err)
	}
	fresh := f.freshClient(t)
	f.ensureNativePrimary(t, ctx)
	old, err := f.nativeClient.Dial(ctx, f.primaryAddress, "machine_target", peerquic.ClassInteractive)
	if err != nil {
		t.Fatal(err)
	}
	ports := &udpPortRecorder{bound: make(chan uint16, 16)}
	s, g := f.startDelegatedGeneration(t, ctx, ports)
	offer, err := s.generationOffer()
	if err != nil {
		t.Fatal(err)
	}
	if err := f.primaryOwner.OfferCarrier(ctx, offer); err != nil {
		t.Fatal(err)
	}
	moved, err := f.nativeClient.Dial(ctx, f.primaryAddress, "machine_target", peerquic.ClassInteractive)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := session.Snapshot{ID: "ses_target", Generation: 1}
	kept := f.admit(t, ctx, moved, snapshot)

	// The old primary exits.
	old.Close()
	if err := f.primaryOwner.Close(); err != nil {
		t.Fatal(err)
	}
	for len(ports.bound) != 0 {
		<-ports.bound
	}
	f.mu.Lock()
	f.primaryTaken = true
	f.mu.Unlock()
	if err := s.promoteGeneration(ctx, g); err != nil {
		t.Fatal("the generation did not take the primary network", err)
	}
	var port uint16
	select {
	case port = <-ports.bound:
	case <-ctx.Done():
		t.Fatal("the promoted generation bound no primary socket")
	}
	assertOpen(t, kept)

	connection := fresh.dial(t, ctx, f, targetPrimaryAddress(t, port))
	f.admit(t, ctx, connection, snapshot)
	assertOpen(t, kept)
	assertOpen(t, f.admit(t, ctx, moved, snapshot))
	// The first primary renewal learns the server takes reports; the report rides on the next one, within
	// the process's renewal budget (one per 12 s), as at any primary's start.
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		f.mu.Lock()
		reported := f.endpointsReported
		f.mu.Unlock()
		if reported {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the promoted generation never reported the machine's endpoints")
		}
	}
}

type udpPortRecorder struct{ bound chan uint16 }

func (l *udpPortRecorder) ListenPacket(ctx context.Context, network, address string) (net.PacketConn, error) {
	socket, err := (&net.ListenConfig{}).ListenPacket(ctx, network, address)
	if err == nil && network == "udp4" {
		select {
		case l.bound <- socket.LocalAddr().(*net.UDPAddr).AddrPort().Port():
		default:
		}
	}
	return socket, err
}

// freshTargetClient is a second CLI peer of the machine that never held a
// carrier offer.
type freshTargetClient struct {
	authority *tailnet.Authority
	binding   tailnet.NetworkBinding
	tls       tls.Certificate
}

// freshClient adds a second CLI peer to the machine's signed network, with the
// first client's grant.
func (f *targetCarrierFixture) freshClient(t *testing.T) *freshTargetClient {
	t.Helper()
	certificate, _ := newBrowserTerminalClientCertificate(t, time.Now())
	public := certificate.PrivateKey.(ed25519.PrivateKey).Public().(ed25519.PublicKey)
	f.mu.Lock()
	first := f.network.Peers[0]
	f.mu.Unlock()
	binding := tailnet.NetworkBinding{AccountID: first.Identity.AccountID, EndpointID: "cli_fresh", Role: "cli", EndpointGeneration: 1, QUICCertificateFingerprint: "0000000000000000000000000000000000000000000000000000000000000002", QUICPublicKey: base64.RawURLEncoding.EncodeToString(public), VirtualAddress: "fd7a:115c:a1e0::3"}
	store := config.ProfileStore{Path: filepath.Join(f.root, "fresh-network"), Secrets: config.FileSecretStore{Dir: filepath.Join(f.root, "fresh-network", "secrets")}}
	authority, err := tailnet.NewAuthority(tailnet.AuthorityOptions{Store: store, Issuer: f.issuer, Self: binding, Keys: targetCarrierKeys{f.signer.Public().(ed25519.PublicKey)}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { authority.Close() })
	binding.WireGuardPublicKey, binding.KeyGeneration, err = authority.PrepareKey(false)
	binding.KeyGeneration++
	if err != nil {
		t.Fatal(err)
	}
	if binding.DiscoPublicKey, err = authority.DiscoveryPublicKey(); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.network.Generation++
	f.network.Peers = append(f.network.Peers, tailnet.NetworkPeer{Identity: binding, Scopes: first.Scopes})
	f.mu.Unlock()
	return &freshTargetClient{authority: authority, binding: binding, tls: certificate}
}

// dial reaches the machine at its primary address with no carrier offer.
func (c *freshTargetClient) dial(t *testing.T, ctx context.Context, f *targetCarrierFixture, address netip.AddrPort) *native.Session {
	t.Helper()
	f.mu.Lock()
	configuration := f.network
	f.mu.Unlock()
	configuration.Generation++
	configuration.Self = c.binding
	scope := configuration.Peers[0].Scopes[0]
	scope.Direction = "dial"
	configuration.Peers = []tailnet.NetworkPeer{{Identity: f.network.Self, Endpoints: []string{address.String()}, Scopes: []tailnet.NetworkScope{scope}}}
	if err := c.authority.Apply(ctx, f.configurationToken(t, configuration)); err != nil {
		t.Fatal(err)
	}
	owner, err := native.NewOwner(native.Config{Authority: c.authority, TLS: &tls.Config{MinVersion: tls.VersionTLS13, InsecureSkipVerify: true, NextProtos: []string{peerquic.ALPN}, Certificates: []tls.Certificate{c.tls}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { owner.Close() })
	descriptor, err := c.authority.Descriptor("machine_target")
	if err != nil {
		t.Fatal(err)
	}
	connection, err := owner.Dial(ctx, descriptor, "machine_target", peerquic.ClassInteractive)
	if err != nil {
		t.Fatal("a client without an offer did not reach the machine's primary network", err)
	}
	t.Cleanup(func() { connection.Close() })
	return connection
}

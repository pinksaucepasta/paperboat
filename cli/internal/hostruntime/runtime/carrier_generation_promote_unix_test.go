//go:build linux || darwin

package runtime

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"encoding/base64"
	"net/netip"
	"path/filepath"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/native"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/peerquic"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
)

// pb update under drain, real machine state and real native connections:
//   - a stream held on the old primary keeps working while the new delegated generation serves every new
//     connection (the client learns it from its signed configuration) and after the old primary stops
//     admitting new ones; the old primary exits only once that stream closed;
//   - the generation then takes the primary role in place: it binds the machine's primary network and reports
//     the machine's endpoints, a stream already open on its delegated network keeps working, and a client
//     that never held an offer reaches it by the machine identity alone.
func TestDelegatedGenerationTakesThePrimaryRoleAfterTheOldPrimaryDrains(t *testing.T) {
	f := newPromoteFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	primary := f.startPrimary(t, ctx)
	old, err := f.client.Dial(ctx, f.primaryAddress, "machine_target", peerquic.ClassInteractive)
	if err != nil {
		t.Fatal(err)
	}
	held, mark := openMarked(t, ctx, old)
	if mark != 'P' {
		t.Fatalf("the first stream was served by %q, not the old primary", mark)
	}
	echo(t, held, "held-before-update")

	ports := &promotionListener{primary: newPinnedPacketListener(t)}
	s, g := f.startGeneration(t, ctx, ports)
	offer, err := g.carrier.Current().MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	f.applyClient(t, ctx, func(c *tailnet.NetworkConfiguration) { c.Peers[0].CarrierOffer = offer })
	moved, err := f.client.Dial(ctx, f.primaryAddress, "machine_target", peerquic.ClassInteractive)
	if err != nil {
		t.Fatal("a new connection did not reach the generation its configuration offers", err)
	}
	defer moved.Close()
	kept, mark := openMarked(t, ctx, moved)
	if mark != 'G' {
		t.Fatalf("a new connection was served by %q, not the new generation", mark)
	}
	echo(t, held, "held-during-drain")
	echo(t, kept, "kept-on-generation")
	held.Close()
	old.Close()
	if err := primary.Close(); err != nil {
		t.Fatal("the drained primary did not exit cleanly", err)
	}

	// The generation takes the primary role; its refresh at promotion admits a second client.
	fresh := f.freshClient(t, ctx)
	ports.armed.Store(true)
	f.mu.Lock()
	f.primaryTaken = true
	f.mu.Unlock()
	if err := s.promoteGeneration(ctx, g); err != nil {
		t.Fatal("the generation did not take the primary network", err)
	}
	var port uint16
	select {
	case port = <-ports.primary.bound:
	case <-ctx.Done():
		t.Fatal("the promoted generation bound no primary socket")
	}
	echo(t, kept, "kept-across-promotion")
	stream, mark := openMarked(t, ctx, fresh.dial(t, ctx, f, primaryAddress(t, port)))
	if mark != 'G' {
		t.Fatalf("a client without an offer was served by %q, not the promoted generation", mark)
	}
	echo(t, stream, "fresh-on-primary")
	echo(t, kept, "kept-after-fresh")
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

// freshTarget is a second CLI peer, with the first client's grant, that never held a carrier offer.
type freshTarget struct {
	authority *tailnet.Authority
	binding   tailnet.NetworkBinding
	tls       tls.Certificate
}

// freshClient adds the second CLI peer to the machine's signed network; the machine learns it from its next
// refresh, as from the server.
func (f *promoteFixture) freshClient(t *testing.T, ctx context.Context) *freshTarget {
	t.Helper()
	certificate, _ := newBrowserTerminalClientCertificate(t, time.Now())
	fingerprint := certificateFingerprint(certificate)
	public := certificate.PrivateKey.(ed25519.PrivateKey).Public().(ed25519.PublicKey)
	f.mu.Lock()
	first := f.network.Peers[0]
	f.mu.Unlock()
	binding := tailnet.NetworkBinding{AccountID: first.Identity.AccountID, EndpointID: "cli_fresh", Role: "cli", EndpointGeneration: 1, QUICCertificateFingerprint: fingerprint, QUICPublicKey: base64.RawURLEncoding.EncodeToString(public), VirtualAddress: "fd7a:115c:a1e0::3"}
	store := config.ProfileStore{Path: filepath.Join(f.root, "fresh-network"), Secrets: config.FileSecretStore{Dir: filepath.Join(f.root, "fresh-network", "secrets")}}
	authority, err := tailnet.NewAuthority(tailnet.AuthorityOptions{Store: store, Issuer: f.issuer, Self: binding, Keys: promoteKeys{f.signer.Public().(ed25519.PublicKey)}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { authority.Close() })
	binding.WireGuardPublicKey, binding.KeyGeneration, err = authority.PrepareKey(false)
	if err != nil {
		t.Fatal(err)
	}
	binding.KeyGeneration++
	if binding.DiscoPublicKey, err = authority.DiscoveryPublicKey(); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.network.Generation++
	f.network.IssuedAt = time.Now().Unix()
	f.network.Peers = append(f.network.Peers, tailnet.NetworkPeer{Identity: binding, Scopes: first.Scopes})
	f.mu.Unlock()
	return &freshTarget{authority: authority, binding: binding, tls: certificate}
}

// dial reaches the machine at address by its identity alone.
func (c *freshTarget) dial(t *testing.T, ctx context.Context, f *promoteFixture, address netip.AddrPort) *native.Session {
	t.Helper()
	f.mu.Lock()
	configuration := f.network
	f.mu.Unlock()
	scope := configuration.Peers[0].Scopes[0]
	scope.Direction = "dial"
	machine := configuration.Self
	configuration.Generation++
	configuration.Self = c.binding
	configuration.Peers = []tailnet.NetworkPeer{{Identity: machine, Endpoints: []string{address.String()}, Scopes: []tailnet.NetworkScope{scope}}}
	if err := c.authority.Apply(ctx, f.token(t, configuration)); err != nil {
		t.Fatal(err)
	}
	owner, err := native.NewOwner(native.Config{Authority: c.authority, TLS: &tls.Config{MinVersion: tls.VersionTLS13, InsecureSkipVerify: true, NextProtos: []string{peerquic.ALPN}, Certificates: []tls.Certificate{c.tls}}}) //nolint:gosec // peer identity is pinned by the signed network
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

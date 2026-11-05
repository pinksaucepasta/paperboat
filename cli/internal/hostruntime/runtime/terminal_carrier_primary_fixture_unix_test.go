//go:build linux || darwin

package runtime

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"encoding/json"
	"net"
	"net/netip"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workloadbridge"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/native"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/peerquic"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
)

// The production candidate client always starts from an authenticated serving
// connection. The fixture captures only the primary's real bind port, then
// supplies its signed endpoint through the same network configuration contract.
func (f *targetCarrierFixture) ensureNativePrimary(t *testing.T, ctx context.Context) {
	t.Helper()
	if f.primaryOwner != nil {
		return
	}
	tlsConfig, _, self, err := loadNativePeerTLS(f.root, "machine_target", 1)
	if err != nil {
		t.Fatal(err)
	}
	listener := newTargetPrimaryPacketListener(t)
	authority, err := tailnet.NewAuthority(tailnet.AuthorityOptions{Store: config.ProfileStore{Path: filepath.Join(f.root, "peer-network"), Secrets: config.FileSecretStore{Dir: filepath.Join(f.root, "peer-network", "secrets")}}, Issuer: f.issuer, Self: self, Keys: targetCarrierKeys{f.signer.Public().(ed25519.PublicKey)}, TestOnlyPacketListener: listener})
	if err != nil {
		t.Fatal(err)
	}
	if err := authority.Restore(ctx); err != nil {
		t.Fatal(err)
	}
	f.primaryOwner, err = native.NewOwner(native.Config{Authority: authority, TLS: tlsConfig, PrepareCarrier: f.prepareGeneration})
	if err != nil {
		t.Fatal(err)
	}
	_, err = authority.Listen(nil)
	if err != nil {
		t.Fatal(err)
	}
	var port uint16
	select {
	case port = <-listener.bound:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	address := targetPrimaryAddress(t, port)
	clientConfig := f.network
	clientConfig.Generation++
	clientConfig.Self = f.network.Peers[0].Identity
	scope := f.network.Peers[0].Scopes[0]
	scope.Direction = "dial"
	peer := f.network.Self
	clientConfig.Peers = []tailnet.NetworkPeer{{Identity: peer, Endpoints: []string{address.String()}, Scopes: []tailnet.NetworkScope{scope}}}
	if err := f.clientAuthority.Apply(ctx, f.configurationToken(t, clientConfig)); err != nil {
		t.Fatal(err)
	}
	f.nativeClient, err = native.NewOwner(native.Config{Authority: f.clientAuthority, TLS: &tls.Config{MinVersion: tls.VersionTLS13, InsecureSkipVerify: true, NextProtos: []string{peerquic.ALPN}, Certificates: []tls.Certificate{f.clientTLS}}})
	if err != nil {
		t.Fatal(err)
	}
	f.primaryAddress, err = f.clientAuthority.Descriptor("machine_target")
	if err != nil {
		t.Fatal(err)
	}
	run, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- f.primaryOwner.Listen(run, nil, f.owner.carrier.apps.Serve) }()
	t.Cleanup(func() { cancel(); f.nativeClient.Close(); f.primaryOwner.Close(); <-done })
}

func (f *targetCarrierFixture) prepareGeneration(ctx context.Context, peer string, request native.CarrierExchange) (native.CarrierOffer, error) {
	f.mu.Lock()
	extra := f.extraTarget
	f.mu.Unlock()
	if extra != nil && bytes.Equal(extra.offer.Claims.Certificate, request.Offer.Claims.Certificate) {
		if err := extra.network.AddPeer(peer, request.DiscoPublicKey, request.Endpoints); err != nil {
			return native.CarrierOffer{}, err
		}
		if err := extra.network.PreparePeerRelay(ctx, peer, request.RelayNodeID); err != nil {
			return native.CarrierOffer{}, err
		}
		socket, err := extra.network.Dial(ctx, peer)
		if err != nil {
			return native.CarrierOffer{}, err
		}
		socket.Close()
		return extra.offer, nil
	}
	if f.unknownRelay {
		request.RelayNodeID = "relay_refused"
	}
	body, err := json.Marshal(request)
	if err != nil {
		return native.CarrierOffer{}, err
	}
	result, err := f.client.TerminalCarrier(ctx, workloadbridge.CarrierRequest{Operation: "prepare-peer", PeerID: peer, Exchange: body})
	if err != nil {
		return native.CarrierOffer{}, err
	}
	return native.ParseCarrierOffer(result.Offer)
}

// targetPrimaryPacketListener keeps the primary's IPv4 socket on one port. The
// client is given that endpoint once; magicsock rebinds try its configured port
// 41642 first, and on CI another test's engine freed it 6 ms after the primary
// started on a random port, so the primary moved and the client pinged a dead
// port until its deadline (main CI job 127099348562).
type targetPrimaryPacketListener struct {
	port  uint16
	bound chan uint16 // each IPv4 bind, dropped when full; one engine only
}

func newTargetPrimaryPacketListener(t *testing.T) *targetPrimaryPacketListener {
	t.Helper()
	probe, err := net.ListenPacket("udp4", ":0")
	if err != nil {
		t.Fatal(err)
	}
	port := uint16(probe.LocalAddr().(*net.UDPAddr).Port)
	if err := probe.Close(); err != nil {
		t.Fatal(err)
	}
	return &targetPrimaryPacketListener{port: port, bound: make(chan uint16, 16)}
}

func (l *targetPrimaryPacketListener) ListenPacket(ctx context.Context, network, address string) (net.PacketConn, error) {
	if network == "udp4" {
		address = net.JoinHostPort("", strconv.Itoa(int(l.port)))
	}
	socket, err := (&net.ListenConfig{}).ListenPacket(ctx, network, address)
	if err == nil && network == "udp4" {
		select {
		case l.bound <- l.port:
		default:
		}
	}
	return socket, err
}
func targetPrimaryAddress(t *testing.T, port uint16) netip.AddrPort {
	t.Helper()
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range addresses {
		prefix, err := netip.ParsePrefix(raw.String())
		if err == nil && prefix.Addr().Is4() && prefix.Addr().IsGlobalUnicast() && !prefix.Addr().IsLoopback() {
			return netip.AddrPortFrom(prefix.Addr(), port)
		}
	}
	t.Fatal("real primary fixture requires a non-loopback interface")
	return netip.AddrPort{}
}

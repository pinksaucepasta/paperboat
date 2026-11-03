package native_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/nativesession"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/native"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/peerquic"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/streamauth"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
	"tailscale.com/tstest/integration"
	"tailscale.com/types/nettype"
)

func TestDelegatedCarrierInheritsMachineAuthorityAndRevocation(t *testing.T) {
	testDelegatedCarrierGeneration(t, false, false)
}
func TestDelegatedCarrierPunchesAnUnsolicitedInboundFilter(t *testing.T) {
	testDelegatedCarrierGeneration(t, true, false)
}

func TestDelegatedCarrierRetainsPreparationAcrossLostInitials(t *testing.T) {
	testDelegatedCarrierGeneration(t, true, true)
}

func testDelegatedCarrierGeneration(t *testing.T, behindNAT, lostInitials bool) {
	t.Setenv("IN_TS_TEST", "true")
	regions := integration.RunDERPAndSTUN(t, func(string, ...any) {}, "127.0.0.1")
	public, signer, _ := ed25519.GenerateKey(rand.Reader)
	clientTLS, clientFP := testTLS(t, "cli")
	machineTLS, machineFP := testTLS(t, "machine")
	client := tailnet.NetworkBinding{AccountID: "account_test", EndpointID: "cli_test", Role: "cli", EndpointGeneration: 1, QUICCertificateFingerprint: clientFP, VirtualAddress: "fd7a:115c:a1e0::1"}
	machine := tailnet.NetworkBinding{AccountID: "account_test", EndpointID: "machine_test", Role: "machine", MachineID: "machine_test", EndpointGeneration: 1, MachineGeneration: 1, QUICCertificateFingerprint: machineFP, VirtualAddress: "fd7a:115c:a1e0::2"}
	var listener nettype.PacketListener
	var filtered *punchPacketListener
	if behindNAT {
		filtered = &punchPacketListener{}
		listener = filtered
	}
	ca, ma := testAuthority(t, client, testKeys{"native_test": public}), testAuthority(t, machine, testKeys{"native_test": public}, listener)
	client.WireGuardPublicKey, client.KeyGeneration = prepareBinding(t, ca)
	machine.WireGuardPublicKey, machine.KeyGeneration = prepareBinding(t, ma)
	now := time.Now().Unix()
	cc := testConfiguration(now, 1, client, machine, "dial")
	mc := testConfiguration(now, 1, machine, client, "accept")
	applyTestConfiguration(t, ca, signer, cc)
	applyTestConfiguration(t, ma, signer, mc)
	co, err := native.NewOwner(native.Config{Authority: ca, TLS: clientTLS})
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()
	var generation *tailnet.CarrierNetwork
	var identity *native.CarrierIdentity
	mo, err := native.NewOwner(native.Config{Authority: ma, TLS: machineTLS, PrepareCarrier: func(ctx context.Context, peer string, request native.CarrierExchange) (native.CarrierOffer, error) {
		if identity == nil || !bytes.Equal(request.Offer.Claims.Certificate, identity.Offer.Claims.Certificate) {
			return native.CarrierOffer{}, native.ErrInvalid
		}
		if err := generation.AddPeer(peer, request.DiscoPublicKey, request.Endpoints); err != nil {
			return native.CarrierOffer{}, err
		}
		socket, err := generation.Dial(ctx, peer)
		if err != nil {
			return native.CarrierOffer{}, err
		}
		socket.Close()
		return identity.Offer, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer mo.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	address := startTestServer(t, mo, ma, regions.Regions[1])
	old, err := co.Dial(ctx, address, "machine_test", peerquic.ClassInteractive)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	before, err := ca.Self()
	if err != nil {
		t.Fatal(err)
	}
	oldStream := openCarrierTerminal(t, ctx, old, "old")
	defer oldStream.Close()
	carrierEcho(t, oldStream)
	if lostInitials {
		filtered.stallNewSockets.Store(true)
	}
	identity, err = mo.PrepareCarrierIdentity()
	if err != nil {
		t.Fatal(err)
	}
	apps, err := nativesession.New(nativesession.Config{Authorize: func(context.Context, streamauth.Header) (string, error) { return "grant_test", nil }, ServeStream: func(_ context.Context, _ streamauth.Header, c net.Conn) error { _, err := io.Copy(c, c); return err }, ServeTransfer: func(context.Context, net.Conn) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	directCtx, stopDirect := context.WithCancel(ctx)
	generation, err = mo.StartCarrierNetwork(directCtx, identity, "", apps.Serve)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { stopDirect(); generation.Close(); apps.Wait() }()
	offer := identity.Offer
	// The wire proof is machine-signed and uses only the old authenticated peer.
	// No NetworkAPI is supplied to either owner; a server round trip cannot occur.
	moved := make(chan *native.Session, 1)
	if err := old.OnCarrierMove(func(moveCtx context.Context, offered native.CarrierOffer) error {
		next, err := old.DialCarrier(moveCtx, offered)
		if err != nil {
			t.Logf("candidate move failed: %v", err)
		}
		if err == nil {
			moved <- next
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// Crash before a new listener can accept: a real UDP dial fails while the
	// old application continues. No server refresh or terminal bytes are used.
	unreachable, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	failedIdentity, err := mo.PrepareCarrierIdentity()
	if err == nil {
		err = mo.SignCarrierCandidates(failedIdentity, "", generation.DiscoPublic(), []netip.AddrPort{unreachable.LocalAddr().(*net.UDPAddr).AddrPort()})
	}
	unreachable.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err := mo.OfferCarrier(ctx, failedIdentity.Offer); err == nil {
		t.Fatal("unreachable generation was acknowledged as moved")
	}
	carrierEcho(t, oldStream)
	// The old server sends only a bounded, signed address offer on its existing
	// authenticated QUIC connection, and waits for the client's move ACK.
	// Its application byte stream remains usable throughout.
	// A real second QUIC connection has no terminal move handler. It must
	// remain outside terminal preparation (SSH and other consumers drain later).
	other, err := co.Dial(ctx, address, "machine_test", peerquic.ClassInteractive)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	header, err := streamauth.New("other", "exec", "other", "credential_exec", time.Now().Add(time.Minute), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	otherStream, err := other.OpenAuthorized(ctx, header, "grant_test", "exec")
	if err != nil {
		t.Fatal(err)
	}
	defer otherStream.Close()
	carrierEcho(t, otherStream)
	if err := mo.OfferCarrier(ctx, offer); err != nil {
		t.Fatal(err)
	}
	carrierEcho(t, otherStream)
	next := <-moved
	if next == nil {
		t.Fatal("carrier offer was acknowledged without a new connection")
	}
	defer next.Close()
	nextStream := openCarrierTerminal(t, ctx, next, "new")
	defer nextStream.Close()
	carrierEcho(t, nextStream)
	if lostInitials && filtered.stalled.Load() == 0 {
		t.Fatal("lost-Initial capture did not drop new-generation packets")
	}
	carrierEcho(t, oldStream)
	for _, consumer := range []string{"exec", "ssh"} {
		h, err := streamauth.New("denied", consumer, "denied", "credential_terminal", time.Now().Add(time.Minute), 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		if c, err := next.OpenAuthorized(ctx, h, "grant_test", "terminal"); err == nil {
			c.Close()
			t.Fatal("terminal delegation admitted another consumer", consumer)
		}
	}

	after, err := ca.Self()
	if err != nil || before != after {
		t.Fatal("delegation changed registered endpoint identity", err)
	}
	bad := offer
	bad.Claims.MachineID = "another_machine"
	body, _ := json.Marshal(bad.Claims)
	bad.Signature = ed25519.Sign(machineTLS.Certificates[0].PrivateKey.(ed25519.PrivateKey), body)
	if s, err := old.DialCarrier(ctx, bad); err == nil {
		s.Close()
		t.Fatal("machine delegation escaped its parent resource")
	}
	carrierEcho(t, oldStream)
	// One signed machine withdrawal fences both the old transport and delegation.
	cc.Generation++
	cc.Peers = nil
	applyTestConfiguration(t, ca, signer, cc)
	for _, stream := range []net.Conn{oldStream, nextStream} {
		stream.SetReadDeadline(time.Now().Add(time.Second))
		var b [1]byte
		_, err := stream.Read(b[:])
		var timeout net.Error
		if err == nil || errors.As(err, &timeout) && timeout.Timeout() {
			t.Fatal("machine revocation left a generation alive", err)
		}
	}
	if s, err := old.DialCarrier(ctx, offer); err == nil {
		s.Close()
		t.Fatal("revoked parent admitted a delegated generation")
	}
}

func openCarrierTerminal(t *testing.T, ctx context.Context, s *native.Session, id string) net.Conn {
	t.Helper()
	h, err := streamauth.New(id, "terminal", id, "credential_terminal", time.Now().Add(time.Minute), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := s.OpenAuthorized(ctx, h, "grant_test", "terminal")
	if err != nil {
		t.Fatal(err)
	}
	return stream
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

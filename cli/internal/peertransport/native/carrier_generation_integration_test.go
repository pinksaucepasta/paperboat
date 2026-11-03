package native_test

import (
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
)

func TestDelegatedCarrierInheritsMachineAuthorityAndRevocation(t *testing.T) {
	t.Setenv("IN_TS_TEST", "true")
	regions := integration.RunDERPAndSTUN(t, func(string, ...any) {}, "127.0.0.1")
	public, signer, _ := ed25519.GenerateKey(rand.Reader)
	clientTLS, clientFP := testTLS(t, "cli")
	machineTLS, machineFP := testTLS(t, "machine")
	client := tailnet.NetworkBinding{AccountID: "account_test", EndpointID: "cli_test", Role: "cli", EndpointGeneration: 1, QUICCertificateFingerprint: clientFP, VirtualAddress: "fd7a:115c:a1e0::1"}
	machine := tailnet.NetworkBinding{AccountID: "account_test", EndpointID: "machine_test", Role: "machine", MachineID: "machine_test", EndpointGeneration: 1, MachineGeneration: 1, QUICCertificateFingerprint: machineFP, VirtualAddress: "fd7a:115c:a1e0::2"}
	ca, ma := testAuthority(t, client, testKeys{"native_test": public}), testAuthority(t, machine, testKeys{"native_test": public})
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
	mo, err := native.NewOwner(native.Config{Authority: ma, TLS: machineTLS})
	if err != nil {
		t.Fatal(err)
	}
	defer mo.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	old, err := co.Dial(ctx, startTestServer(t, mo, ma, regions.Regions[1]), "machine_test", peerquic.ClassInteractive)
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
	socket, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	identity, err := mo.NewCarrierIdentity([]netip.AddrPort{socket.LocalAddr().(*net.UDPAddr).AddrPort()})
	if err != nil {
		socket.Close()
		t.Fatal(err)
	}
	encoded, err := identity.Offer.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	offer, err := native.ParseCarrierOffer(encoded)
	if err != nil {
		t.Fatal(err)
	}
	apps, err := nativesession.New(nativesession.Config{Authorize: func(context.Context, streamauth.Header) (string, error) { return "grant_test", nil }, ServeStream: func(_ context.Context, _ streamauth.Header, c net.Conn) error { _, err := io.Copy(c, c); return err }, ServeTransfer: func(context.Context, net.Conn) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	directCtx, stopDirect := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- mo.ListenCarrier(directCtx, socket, identity, apps.Serve) }()
	defer func() { stopDirect(); socket.Close(); <-done; apps.Wait() }()
	// The wire proof is machine-signed and uses only the old authenticated peer.
	// No NetworkAPI is supplied to either owner; a server round trip cannot occur.
	next, err := old.DialCarrier(ctx, offer)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	nextStream := openCarrierTerminal(t, ctx, next, "new")
	defer nextStream.Close()
	carrierEcho(t, nextStream)
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

package native_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"net"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/nativesession"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/native"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/peerquic"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/streamauth"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
	"tailscale.com/tstest/integration"
)

// pbvm4 feat6: during an update exec, SSH, transfers and private streams stayed
// on the old primary carrier and paused (5 s) or were refused (~40 s) while it
// restarted. Once a generation that serves every application is offered, a
// client without a terminal moves too: its open streams keep running on the old
// connection, and its new connections open on the generation.
func TestCarrierGenerationTakesEveryNewConnection(t *testing.T) {
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
	applyTestConfiguration(t, ca, signer, testConfiguration(now, 1, client, machine, "dial"))
	applyTestConfiguration(t, ma, signer, testConfiguration(now, 1, machine, client, "accept"))
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
		if err := generation.PreparePeerRelay(ctx, peer, request.RelayNodeID); err != nil {
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
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	address := startTestServer(t, mo, ma, regions.Regions[1])
	old, err := co.Dial(ctx, address, "machine_test", peerquic.ClassInteractive)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	oldStream := openConsumer(t, ctx, old, "exec")
	defer oldStream.Close()
	carrierEcho(t, oldStream)

	identity, err = mo.PrepareCarrierIdentity()
	if err != nil {
		t.Fatal(err)
	}
	identity.Offer.Claims.AllApplications = true
	// The generation marks what it serves, so the test sees which carrier answered.
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
	generation, err = mo.StartCarrierNetwork(genCtx, identity, "", apps.Serve)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { stopGen(); generation.Close(); apps.Wait() }()
	if err := mo.OfferCarrier(ctx, identity.Offer); err != nil {
		t.Fatal("a connection without a terminal declined the generation", err)
	}
	carrierEcho(t, oldStream)
	for _, consumer := range []string{"exec", "ssh", "exec"} {
		fresh, err := co.Dial(ctx, address, "machine_test", peerquic.ClassInteractive)
		if err != nil {
			t.Fatal(err)
		}
		stream := openConsumer(t, ctx, fresh, consumer)
		stream.SetDeadline(time.Now().Add(2 * time.Second))
		var mark [1]byte
		if _, err := io.ReadFull(stream, mark[:]); err != nil || mark[0] != 'G' {
			t.Fatal("a new connection did not open on the generation", consumer, err)
		}
		carrierEcho(t, stream)
		stream.Close()
		fresh.Close()
	}
	carrierEcho(t, oldStream)
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

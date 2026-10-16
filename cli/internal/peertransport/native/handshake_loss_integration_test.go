package native_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/peertransport/mesh"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/native"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/peerquic"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
	"github.com/quic-go/quic-go"
	"tailscale.com/tailcfg"
	"tailscale.com/tstest/integration"
)

func TestNativeNewHandshakeRecoversPacketLossWithoutClosingAdmittedPeer(t *testing.T) {
	for _, live := range []bool{false, true} {
		name := "IdleClientRetired"
		if live {
			name = "LiveClientReused"
		}
		t.Run(name, func(t *testing.T) { testNativeLostHandshake(t, live) })
	}
}

func testNativeLostHandshake(t *testing.T, live bool) {
	t.Setenv("IN_TS_TEST", "true")
	regions := integration.RunDERPAndSTUN(t, func(string, ...any) {}, "127.0.0.1")
	public, signer, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	clientTLS, clientFingerprint := testTLS(t, "cli")
	serverTLS, serverFingerprint := testTLS(t, "machine")
	client := tailnet.NetworkBinding{AccountID: "account_test", EndpointID: "cli_test", Role: "cli", EndpointGeneration: 1, QUICCertificateFingerprint: clientFingerprint, VirtualAddress: "fd7a:115c:a1e0::1"}
	machine := tailnet.NetworkBinding{AccountID: "account_test", EndpointID: "machine_test", Role: "machine", MachineID: "machine_test", EndpointGeneration: 1, MachineGeneration: 1, QUICCertificateFingerprint: serverFingerprint, VirtualAddress: "fd7a:115c:a1e0::2"}
	sender := testAuthority(t, client, testKeys{"native_test": public})
	receiver := testAuthority(t, machine, testKeys{"native_test": public})
	client.WireGuardPublicKey, client.KeyGeneration = prepareBinding(t, sender)
	machine.WireGuardPublicKey, machine.KeyGeneration = prepareBinding(t, receiver)
	now := time.Now().Unix()
	applyTestConfiguration(t, sender, signer, testConfiguration(now, 1, client, machine, "dial"))
	applyTestConfiguration(t, receiver, signer, testConfiguration(now, 1, machine, client, "accept"))
	serverTLS.ClientAuth = tls.RequireAnyClientCert
	serverTLS.VerifyConnection = func(state tls.ConnectionState) error {
		if len(state.PeerCertificates) != 1 {
			return errors.New("missing fixture client certificate")
		}
		fingerprint := sha256.Sum256(state.PeerCertificates[0].Raw)
		if hex.EncodeToString(fingerprint[:]) != clientFingerprint {
			return errors.New("wrong fixture client certificate")
		}
		return nil
	}
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Second)
	defer cancel()
	lost := int32(1)
	if live {
		lost = 2
	}
	address, flows, failures, stop := listenWithLostHandshake(t, ctx, receiver, regions.Regions[1], serverTLS, lost)
	defer stop()
	owner, err := native.NewOwner(native.Config{Authority: sender, TLS: clientTLS})
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	var stream net.Conn
	if live {
		first, err := owner.Dial(ctx, address, "machine_test", peerquic.ClassInteractive)
		if err != nil {
			t.Fatal(err)
		}
		defer first.Close()
		stream, err = first.Open(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer stream.Close()
	}
	exchange := func(value string) {
		t.Helper()
		if err := stream.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
			t.Fatal(err)
		}
		if _, err := stream.Write([]byte(value)); err != nil {
			t.Fatal(err)
		}
		response := make([]byte, len(value))
		if _, err := io.ReadFull(stream, response); err != nil || string(response) != value {
			t.Fatalf("admitted peer lost its byte stream: %v", err)
		}
	}
	if live {
		exchange("before-loss")
	}
	cached, err := sender.Client(address, "machine_test")
	if err != nil {
		t.Fatal(err)
	}
	second, dialErr := owner.Dial(ctx, address, "machine_test", peerquic.ClassInteractive)
	if live {
		exchange("after-loss")
	}
	if dialErr != nil {
		var idle *quic.IdleTimeoutError
		if !errors.As(dialErr, &idle) {
			t.Fatalf("unexpected new-handshake failure: %v", dialErr)
		}
		t.Fatalf("a lost new handshake was not recovered while the admitted peer survived: %v", dialErr)
	}
	defer second.Close()
	again, err := sender.Client(address, "machine_test")
	if err != nil || (again == cached) != live {
		t.Fatal("lost handshake violated idle retirement/live reuse rule", err)
	}
	if flows.Load() != lost+1 {
		t.Fatalf("wanted %d flows including one lost and one replacement; got %d", lost+1, flows.Load())
	}
	select {
	case err := <-failures:
		t.Fatal(err)
	default:
	}
}

func listenWithLostHandshake(t *testing.T, parent context.Context, receiver *tailnet.Authority, region *tailcfg.DERPRegion, serverTLS *tls.Config, lost int32) (mesh.Addr, *atomic.Int32, <-chan error, func()) {
	t.Helper()
	server, err := receiver.Listen(region)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(parent)
	var workers sync.WaitGroup
	var flows atomic.Int32
	failures := make(chan error, 16)
	report := func(err error) {
		if err != nil && ctx.Err() == nil {
			select {
			case failures <- err:
			default:
			}
		}
	}
	workers.Add(1)
	go func() {
		defer workers.Done()
		for {
			socket, err := server.Accept(ctx)
			if err != nil {
				report(err)
				return
			}
			ordinal := flows.Add(1)
			workers.Add(1)
			go func() {
				defer workers.Done()
				defer socket.Close()
				stop := context.AfterFunc(ctx, func() { _ = socket.Close() })
				defer stop()
				// Lose only the next association's datagrams. The already admitted
				// association and all later UDP flows continue using the real peer engine.
				if ordinal == lost {
					buffer := make([]byte, 2048)
					for {
						if _, _, err := socket.ReadFrom(buffer); err != nil {
							return
						}
					}
				}
				listener, err := peerquic.ListenPacket(socket, serverTLS, peerquic.DevelopmentSessionConfig(peerquic.ClassInteractive))
				if err != nil {
					report(err)
					return
				}
				defer listener.Close()
				session, err := listener.Accept(ctx)
				if err != nil {
					report(err)
					return
				}
				defer session.Close()
				stream, err := session.Connection.AcceptStream(ctx)
				if err != nil {
					report(err)
					return
				}
				_, err = io.Copy(stream, stream)
				if err != nil && ctx.Err() == nil && session.Connection.Context().Err() == nil {
					report(err)
				}
			}()
		}
	}()
	return server.Address(), &flows, failures, func() { cancel(); _ = server.Close(); workers.Wait() }
}

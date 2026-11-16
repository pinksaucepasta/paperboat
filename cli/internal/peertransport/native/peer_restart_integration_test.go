package native_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/peertransport/native"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/peerquic"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/streamauth"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
	"tailscale.com/tstest/integration"
)

func TestNativeCachedSenderReconnectsAfterPeerEngineRestart(t *testing.T) {
	t.Setenv("IN_TS_TEST", "true")
	regions := integration.RunDERPAndSTUN(t, func(string, ...any) {}, "127.0.0.1")
	public, signer, _ := ed25519.GenerateKey(rand.Reader)
	clientTLS, clientFingerprint := testTLS(t, "cli")
	serverTLS, serverFingerprint := testTLS(t, "machine")
	client := tailnet.NetworkBinding{
		AccountID: "account_test", EndpointID: "cli_test", Role: "cli", EndpointGeneration: 1,
		QUICCertificateFingerprint: clientFingerprint, VirtualAddress: "fd7a:115c:a1e0::1",
	}
	machine := tailnet.NetworkBinding{
		AccountID: "account_test", EndpointID: "machine_test", Role: "machine", MachineID: "machine_test",
		EndpointGeneration: 1, MachineGeneration: 1,
		QUICCertificateFingerprint: serverFingerprint, VirtualAddress: "fd7a:115c:a1e0::2",
	}
	sender := testAuthority(t, client, testKeys{"native_test": public})
	receiver := testAuthority(t, machine, testKeys{"native_test": public})
	client.WireGuardPublicKey, client.KeyGeneration = prepareBinding(t, sender)
	machine.WireGuardPublicKey, machine.KeyGeneration = prepareBinding(t, receiver)
	now := time.Now().Unix()
	applyTestConfiguration(t, sender, signer, testConfiguration(now, 1, client, machine, "dial"))
	applyTestConfiguration(t, receiver, signer, testConfiguration(now, 1, machine, client, "accept"))
	var dialFailures atomic.Int32
	clientOwner, err := native.NewOwner(native.Config{Authority: sender, TLS: clientTLS, Observe: func(event native.Event) {
		if event.Kind == "dial_failed" {
			dialFailures.Add(1)
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer clientOwner.Close()
	serverOwner, err := native.NewOwner(native.Config{Authority: receiver, TLS: serverTLS})
	if err != nil {
		t.Fatal(err)
	}
	defer serverOwner.Close()
	descriptor := startTestServer(t, serverOwner, receiver, regions.Regions[1])
	exchange := func() {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 35*time.Second)
		defer cancel()
		session, err := clientOwner.Dial(ctx, descriptor, "machine_test", peerquic.ClassInteractive)
		if err != nil {
			t.Fatalf("native exec dial after engine lifecycle change: %v", err)
		}
		defer session.Close()
		header, err := streamauth.New("operation_exec", "exec", "stream_exec", "credential_exec", time.Now().Add(time.Minute), 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		stream, err := session.OpenAuthorized(ctx, header, "grant_test", "exec")
		if err != nil {
			t.Fatal(err)
		}
		defer stream.Close()
		if err := stream.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
			t.Fatal(err)
		}
		if _, err := stream.Write([]byte("verified-exec")); err != nil {
			t.Fatal(err)
		}
		response := make([]byte, len("verified-exec"))
		if _, err := io.ReadFull(stream, response); err != nil || string(response) != "verified-exec" {
			t.Fatalf("exec echo after peer restart: %q %v", response, err)
		}
	}
	exchange()
	cached, err := sender.Client(descriptor, "machine_test")
	if err != nil {
		t.Fatal(err)
	}
	listener, err := receiver.Listen(regions.Regions[1])
	if err != nil {
		t.Fatal(err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	descriptor = startTestServer(t, serverOwner, receiver, regions.Regions[1])
	// Keep the same sender/authority and machine key, just as a remote upgrade.
	// Replacing the sender here would conceal the retained UDP-engine failure.
	dialFailures.Store(0)
	exchange()
	replacement, err := sender.Client(descriptor, "machine_test")
	// The cache rule (package doc): a failed pre-application transport retires that idle client. The restarted peer
	// keeps its key, so its first handshake may also succeed on the cached client (the engine resets the restarted
	// peer's WireGuard state before releasing the dial, mesh/engine.go resetRestartedPeer): Windows run 43960352441
	// took that path in 0.09 s with no failure. Only a route that failed must be gone.
	if err != nil || dialFailures.Load() > 0 && replacement == cached {
		t.Fatalf("peer restart retained its failed idle cached route (dial failures %d): %v", dialFailures.Load(), err)
	}
}

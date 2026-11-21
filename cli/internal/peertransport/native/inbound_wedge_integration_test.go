package native_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/peertransport/mesh"
	"github.com/pinksaucepasta/paperboat/cli/internal/peertransport/native"
	"github.com/pinksaucepasta/paperboat/cli/internal/peertransport/peerquic"
	"github.com/pinksaucepasta/paperboat/cli/internal/peertransport/tailnet"
	"tailscale.com/tstest/integration"
)

// A listener whose inbound handshakes never finish (canary-b 2026-11-21: every
// accept died at the 10 s handshake deadline while outbound stayed healthy)
// trips the watchdog from its real accept_failed events, and a rebuilt
// listener in the same process accepts the same peer again.
func TestWedgedInboundListenerRecoversAfterWatchdogRebuild(t *testing.T) {
	if testing.Short() {
		t.Skip("waits for real 10 s handshake deadlines")
	}
	t.Setenv("IN_TS_TEST", "true")
	dm := integration.RunDERPAndSTUN(t, func(string, ...any) {}, "127.0.0.1")
	signerPublic, signer, _ := ed25519.GenerateKey(rand.Reader)
	clientTLS, clientFingerprint := testTLS(t, "cli")
	serverTLS, serverFingerprint := testTLS(t, "machine")
	client := tailnet.NetworkBinding{AccountID: "account_test", EndpointID: "cli_test", Role: "cli", EndpointGeneration: 1, QUICCertificateFingerprint: clientFingerprint, VirtualAddress: "fd7a:115c:a1e0::1"}
	machine := tailnet.NetworkBinding{AccountID: "account_test", EndpointID: "machine_test", Role: "machine", MachineID: "machine_test", EndpointGeneration: 1, MachineGeneration: 1, QUICCertificateFingerprint: serverFingerprint, VirtualAddress: "fd7a:115c:a1e0::2"}
	keys := testKeys{"native_test": signerPublic}
	clientAuthority := testAuthority(t, client, keys)
	client.WireGuardPublicKey, client.KeyGeneration = prepareBinding(t, clientAuthority)
	clientOwner, err := native.NewOwner(native.Config{Authority: clientAuthority, TLS: clientTLS})
	if err != nil {
		t.Fatal(err)
	}
	defer clientOwner.Close()

	now := time.Unix(1_000, 0)
	// Each handshake timeout stands for 15 s of a sustained wedge; Now runs under the watchdog lock.
	watchdog := &native.InboundWatchdog{Now: func() time.Time { now = now.Add(15 * time.Second); return now }}
	tripped := make(chan struct{}, 1)
	var accepted atomic.Int32
	observe := func(event native.Event) {
		if event.Kind == "accepted" {
			accepted.Add(1)
		}
		if trip, _ := watchdog.Observe(event); trip {
			select {
			case tripped <- struct{}{}:
			default:
			}
		}
	}
	// generation builds what supervise rebuilds: a fresh authority, socket,
	// endpoints and owner, then reissues the peer's config (a renewal).
	generation := func(ordinal uint64, tlsConfig *tls.Config) (mesh.Addr, func()) {
		authority := testAuthority(t, machine, keys)
		bound := machine
		bound.WireGuardPublicKey, bound.KeyGeneration = prepareBinding(t, authority)
		issued := time.Now().Unix()
		applyTestConfiguration(t, authority, signer, testConfiguration(issued, 1, bound, client, "accept"))
		applyTestConfiguration(t, clientAuthority, signer, testConfiguration(issued, ordinal, client, bound, "dial"))
		owner, err := native.NewOwner(native.Config{Authority: authority, TLS: tlsConfig, Observe: observe, Inbound: watchdog})
		if err != nil {
			t.Fatal(err)
		}
		server, err := authority.Listen(dm.Regions[1])
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() {
			defer close(done)
			_ = owner.Listen(ctx, dm.Regions[1], func(context.Context, *native.Session) error { return nil })
		}()
		return server.Address(), func() { cancel(); _ = owner.Close(); <-done }
	}

	// The wedge: the first listener's TLS never answers, so every inbound
	// handshake reaches its deadline, as the stale in-process state did.
	release := make(chan struct{})
	defer close(release)
	wedged := serverTLS.Clone()
	certificate := serverTLS.Certificates[0]
	wedged.GetCertificate = func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
		select {
		case <-release:
		case <-time.After(15 * time.Second):
		}
		return &certificate, nil
	}
	var dials sync.WaitGroup
	defer dials.Wait() // runs after the first generation stops
	address, stop := generation(2, wedged)
	stopped := false
	defer func() {
		if !stopped {
			stop()
		}
	}()
	deadline := time.After(2 * time.Minute)
	for wait := true; wait; {
		address := address
		dials.Add(1)
		go func() {
			defer dials.Done()
			ctx, cancel := context.WithTimeout(t.Context(), 12*time.Second)
			defer cancel()
			if session, err := clientOwner.Dial(ctx, address, "machine_test", peerquic.ClassInteractive); err == nil {
				_ = session.Close()
			}
		}()
		select {
		case <-tripped:
			wait = false
		case <-time.After(3 * time.Second):
		case <-deadline:
			t.Fatal("wedged inbound handshakes never tripped the watchdog")
		}
	}
	if accepted.Load() != 0 || !committed(watchdog) {
		t.Fatal("the wedged listener accepted a peer")
	}

	stop() // supervise's rebuild: no process restart
	stopped = true
	dials.Wait()
	watchdog.Reset()
	address, stop = generation(3, serverTLS)
	defer stop()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	session, err := clientOwner.Dial(ctx, address, "machine_test", peerquic.ClassInteractive)
	if err != nil {
		t.Fatalf("inbound did not recover on the rebuilt listener: %v", err)
	}
	defer session.Close()
	for accepted.Load() == 0 && ctx.Err() == nil {
		time.Sleep(10 * time.Millisecond)
	}
	if accepted.Load() == 0 || committed(watchdog) {
		t.Fatal("the rebuilt listener did not accept the peer")
	}
}

func committed(w *native.InboundWatchdog) bool { needed, _ := w.CommitRebuild(); return needed }

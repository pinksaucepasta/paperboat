package native_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/peertransport/native"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/peerquic"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
	"tailscale.com/tstest/integration"
)

// Dials to one peer take turns on its handshake lease (it keeps a failed attempt's stale WireGuard state from outliving
// the retry). After the peer closed cleanly the first dial gives a silent handshake 300 ms, but the dials queued behind
// it waited out a full second each: on the pbvm3 update the held terminal and a probe took turns for 1018, 1005 and
// 1004 ms (pbvm4: 2.0 s). The restart is already known, so the queue shares the short wait.
func TestDialsQueuedBehindARestartedPeersLostHandshakeShareItsShortWait(t *testing.T) {
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
	server, err := receiver.Listen(regions.Regions[1])
	if err != nil {
		t.Fatal(err)
	}
	address := server.Address()
	accepted := make(chan *peerquic.Session, 1)
	go func() {
		socket, err := server.Accept(ctx)
		if err != nil {
			return
		}
		listener, err := peerquic.ListenPacket(socket, serverTLS, peerquic.DevelopmentSessionConfig(peerquic.ClassInteractive))
		if err != nil {
			return
		}
		session, err := listener.Accept(ctx)
		if err == nil {
			accepted <- session
		}
	}()
	owner, err := native.NewOwner(native.Config{Authority: sender, TLS: clientTLS})
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	warm, err := owner.Dial(ctx, address, "machine_test", peerquic.ClassInteractive)
	if err != nil {
		t.Fatal(err)
	}
	_ = warm
	// The peer closes the session cleanly while its socket is still up, as its carrier does for an update.
	select {
	case session := <-accepted:
		_ = session.Close()
	case <-time.After(5 * time.Second):
		t.Fatal("the peer never accepted the warm session")
	}
	time.Sleep(500 * time.Millisecond) // the close reaches the dialed session and watchPeerClose records the restart
	_ = server.Close()
	_, flows, _, closeNew := listenWithLostHandshake(t, ctx, receiver, regions.Regions[1], serverTLS, 1, 2, 3)
	defer closeNew()
	started := time.Now()
	var wg sync.WaitGroup
	errs := make([]error, 2)
	sessions := make([]*native.Session, 2)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sessions[i], errs[i] = owner.Dial(ctx, address, "machine_test", peerquic.ClassInteractive)
			t.Logf("dial %d finished after %v (%d flows so far)", i, time.Since(started), flows.Load())
		}()
	}
	wg.Wait()
	elapsed := time.Since(started)
	for i, err := range errs {
		if err != nil {
			t.Fatal("a dial to the restarted peer failed:", err)
		}
		defer sessions[i].Close()
	}
	// Three lost first flights. About a second of the total is dial setup after the restart, the same either way;
	// the lost flights cost 3 x 0.3 s when the queue shares the short wait (2.06 s measured) and 0.3 s plus two
	// full seconds when only the first dial knows (3.15-3.47 s measured).
	if elapsed > 2600*time.Millisecond {
		t.Fatalf("two dials to a restarted peer took %v: the queued one waited a full second per lost handshake", elapsed)
	}
}

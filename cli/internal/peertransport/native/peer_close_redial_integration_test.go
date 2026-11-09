package native_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"net"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/peertransport/mesh"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/native"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/peerquic"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/streamauth"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
	"tailscale.com/tailcfg"
)

// pbvm4 run8: after `pb update` the target's carrier closes the held session and restarts under the
// same node key. The client's cached lease still holds the old WireGuard session, so the first QUIC
// flight of the redial was lost and the handshake failed after 1 s before the retry took 6 ms.
func TestRedialAfterPeerClosedSessionDoesNotLoseFirstHandshake(t *testing.T) {
	redialAfterPeerClose(t, false, false)
}

// The same restart while another flow still owns the cached client (the exec probe loop's dial in flight, or a session
// not released yet): the client cannot be retired, and the redial must still hand the restarted peer a handshake, not
// data on the old keys. pbvm4 update check: 7 handshake failures at 1 s each, then success at +12 s, when WireGuard's
// own no-reply rekey fired.
func TestRedialAfterPeerCloseWhileAnotherFlowOwnsTheClient(t *testing.T) {
	redialAfterPeerClose(t, true, false)
}

// The restart also killed a second session to the peer, but its close has not reached the client when the redial
// starts (pbvm4 update check, 2 of 5 runs: the exec loop's session was still "live" for a few milliseconds, so the
// reset was skipped and the held terminal waited 16 s for WireGuard's own rekey). The peer's clean close is the restart
// signal; another session that looks alive does not cancel it.
func TestRedialAfterPeerCloseWhileASecondSessionLooksAlive(t *testing.T) {
	redialAfterPeerClose(t, false, true)
}

func redialAfterPeerClose(t *testing.T, anotherFlow, secondSession bool) {
	t.Setenv("IN_TS_TEST", "true")
	regions, derp := runDERPAndSTUNWithServer(t, "127.0.0.1")
	region := regions.Regions[1]
	public, signer, _ := ed25519.GenerateKey(rand.Reader)
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
	clientOwner, err := native.NewOwner(native.Config{Authority: sender, TLS: clientTLS})
	if err != nil {
		t.Fatal(err)
	}
	defer clientOwner.Close()
	serverOwner, err := native.NewOwner(native.Config{Authority: receiver, TLS: serverTLS})
	if err != nil {
		t.Fatal(err)
	}
	defer serverOwner.Close()
	accepted := make(chan *native.Session, 4)
	descriptor := startCapturingServer(t, serverOwner, receiver, region, accepted)

	ctx, cancel := context.WithTimeout(t.Context(), 35*time.Second)
	defer cancel()
	held, err := clientOwner.Dial(ctx, descriptor, "machine_test", peerquic.ClassInteractive)
	if err != nil {
		t.Fatal(err)
	}
	echoOnce(t, ctx, held)
	if anotherFlow {
		// A live flow on the same cached client, as the exec probe loop holds while the redial runs.
		client, err := sender.Client(descriptor, "machine_test")
		if err != nil {
			t.Fatal(err)
		}
		flow, err := client.Dial(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer flow.Close()
	}
	var second *native.Session
	if secondSession {
		var err error
		if second, err = clientOwner.Dial(ctx, descriptor, "machine_test", peerquic.ClassInteractive); err != nil {
			t.Fatal(err)
		}
		defer second.Close()
		echoOnce(t, ctx, second)
	}
	// The WireGuard keys must be older than wireguard-go's RekeyTimeout so a lost-key send cannot hide
	// behind its handshake rate limit, as in production where the keys are minutes old.
	time.Sleep(5200 * time.Millisecond)

	// Carrier stop: the old carrier closes the held session, then the replacement binds new sockets.
	firstAccepted := <-accepted
	if secondSession {
		<-accepted // the second session's server side stays open: its close never reaches the client
	}
	if err := firstAccepted.Close(); err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := held.Open(ctx); err != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	_ = held.Close()
	listener, err := receiver.Listen(region)
	if err != nil {
		t.Fatal(err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	waitDERPConnected(t, derp, machine.WireGuardPublicKey, false) // the old carrier's engine has left
	descriptor = startCapturingServer(t, serverOwner, receiver, region, accepted)
	// Production redials after the new carrier is up (grant fetch and network refresh come first): a dial before the
	// new engine's DERP connection exists waits for the one-second meow resend, which this check is not about. The
	// wait is for the connection itself, not a sleep a loaded runner can outlast.
	waitDERPConnected(t, derp, machine.WireGuardPublicKey, true)

	started := time.Now()
	session, err := clientOwner.Dial(ctx, descriptor, "machine_test", peerquic.ClassInteractive)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	elapsed := time.Since(started)
	t.Logf("redial after peer close took %v", elapsed)
	if elapsed > 500*time.Millisecond {
		t.Fatalf("redial took %v: the first handshake flight went out on the restarted peer's dead WireGuard keys", elapsed)
	}
	echoOnce(t, ctx, session)
}

func echoOnce(t *testing.T, ctx context.Context, session *native.Session) {
	t.Helper()
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
	if _, err := stream.Write([]byte("echo")); err != nil {
		t.Fatal(err)
	}
	response := make([]byte, 4)
	if _, err := io.ReadFull(stream, response); err != nil || string(response) != "echo" {
		t.Fatalf("echo: %q %v", response, err)
	}
}

// startCapturingServer is startTestServer that also hands out each accepted session.
func startCapturingServer(t *testing.T, owner *native.Owner, authority *tailnet.Authority, region *tailcfg.DERPRegion, accepted chan<- *native.Session) mesh.Addr {
	t.Helper()
	server, err := authority.Listen(region)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	go func() {
		_ = owner.Listen(ctx, region, func(serveCtx context.Context, session *native.Session) error {
			accepted <- session
			for {
				stream, _, acceptErr := session.AcceptAuthorized(serveCtx, func(context.Context, streamauth.Header) (string, error) { return "grant_test", nil })
				if acceptErr != nil {
					return acceptErr
				}
				go func(connection net.Conn) {
					defer connection.Close()
					_, _ = io.Copy(connection, connection)
				}(stream)
			}
		})
	}()
	return server.Address()
}

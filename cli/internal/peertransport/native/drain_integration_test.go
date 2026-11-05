package native_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/peertransport/native"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/peerquic"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/streamauth"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
	"github.com/quic-go/quic-go"
	"tailscale.com/tstest/integration"
)

type drainFixture struct {
	co, mo  *native.Owner
	ma      *tailnet.Authority
	signer  ed25519.PrivateKey
	client  tailnet.NetworkBinding
	machine tailnet.NetworkBinding
	dial    func(t *testing.T) (*native.Session, error)
}

func newDrainFixture(t *testing.T, ctx context.Context) *drainFixture {
	t.Helper()
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
	t.Cleanup(func() { co.Close() })
	mo, err := native.NewOwner(native.Config{Authority: ma, TLS: machineTLS})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { mo.Close() })
	address := startTestServer(t, mo, ma, regions.Regions[1])
	f := &drainFixture{co: co, mo: mo, ma: ma, signer: signer, client: client, machine: machine}
	f.dial = func(t *testing.T) (*native.Session, error) {
		return co.Dial(ctx, address, "machine_test", peerquic.ClassInteractive)
	}
	return f
}

// echo writes and reads one round trip on a held stream and returns how long it took.
func echo(stream net.Conn) (time.Duration, error) {
	started := time.Now()
	stream.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := stream.Write([]byte("held")); err != nil {
		return 0, err
	}
	var b [4]byte
	if _, err := io.ReadFull(stream, b[:]); err != nil {
		return 0, err
	}
	if string(b[:]) != "held" {
		return 0, errors.New("echo altered the stream")
	}
	return time.Since(started), nil
}

func restarted(err error) bool {
	var closed *quic.ApplicationError
	return errors.As(err, &closed) && closed.Remote && closed.ErrorCode == peerquic.RestartingCode
}

// openEcho opens an exec stream on a new connection and makes one round trip.
func (f *drainFixture) openEcho(t *testing.T, ctx context.Context) (net.Conn, error) {
	session, err := f.dial(t)
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() { session.Close() })
	header, err := streamauth.New("exec", "exec", "exec", "credential_exec", time.Now().Add(time.Minute), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := session.OpenAuthorized(ctx, header, "grant_test", "exec")
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() { stream.Close() })
	_, err = echo(stream)
	return stream, err
}

// pb update retires the generation: a connection it holds (here an exec stream, the
// pbvm1 exec_long case) keeps being served with no pause and no reattach while it still
// admits new connections, until the cap, whose restart close reaches the held stream as
// a reconnect.
func TestRetiringGenerationKeepsHeldStreamsUntilItsRestartClose(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	f := newDrainFixture(t, ctx)
	held, err := f.openEcho(t, ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		if i%5 == 0 {
			// A client its signed configuration cannot yet take to the new generation keeps a path.
			if _, err := f.openEcho(t, ctx); err != nil {
				t.Fatal("the retiring generation refused a new connection", i, err)
			}
		}
		// A reattach would mean an error here and a redial (seconds), not one more round trip.
		took, err := echo(held)
		if err != nil {
			t.Fatal("the drain paused or cut a held stream", i, err)
		}
		if took > time.Second {
			t.Fatalf("held round trip %d took %v", i, took)
		}
	}
	if err := f.mo.CloseRestarting(); err != nil {
		t.Fatal(err)
	}
	var reconnect *native.ReconnectRequiredError
	if _, err := echo(held); !restarted(err) || !errors.As(err, &reconnect) {
		t.Fatal("the cap did not hand the held stream the restart code", err)
	}
}

// Revocation still ends a drained stream at once.
func TestRevocationEndsADrainedStream(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	f := newDrainFixture(t, ctx)
	held, err := f.openEcho(t, ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := echo(held); err != nil {
		t.Fatal(err)
	}
	revoked := testConfiguration(time.Now().Unix(), 2, f.machine, f.client, "accept")
	revoked.Peers = nil
	applyTestConfiguration(t, f.ma, f.signer, revoked)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := echo(held); err != nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("a revoked peer's drained stream kept being served")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

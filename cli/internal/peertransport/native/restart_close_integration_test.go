package native_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/peertransport/native"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/peerquic"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
	"github.com/quic-go/quic-go"
	"tailscale.com/tstest/integration"
)

// A retiring generation closes what it still holds with the restart code: the
// client must see a reconnect (it then replays from its sequence), never a
// terminal failure.
func TestRestartingCloseRequiresReconnect(t *testing.T) {
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
	mo, err := native.NewOwner(native.Config{Authority: ma, TLS: machineTLS})
	if err != nil {
		t.Fatal(err)
	}
	defer mo.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	address := startTestServer(t, mo, ma, regions.Regions[1])
	session, err := co.Dial(ctx, address, "machine_test", peerquic.ClassInteractive)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	stream := openConsumer(t, ctx, session, "exec")
	defer stream.Close()
	carrierEcho(t, stream)

	if err := mo.CloseRestarting(); err != nil {
		t.Fatal(err)
	}
	stream.SetDeadline(time.Now().Add(5 * time.Second))
	var b [1]byte
	_, err = stream.Read(b[:])
	var reconnect *native.ReconnectRequiredError
	var closed *quic.ApplicationError
	if !errors.As(err, &reconnect) || !errors.As(err, &closed) || closed.ErrorCode != peerquic.RestartingCode || !closed.Remote {
		t.Fatal("the restart code did not ask the client to reconnect", err)
	}
}

package native_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/peertransport/native"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/peerquic"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/streamauth"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
	"go.uber.org/goleak"
	"tailscale.com/tstest/integration"
)

// A CLI endpoint that signed in after the host's last signed document is not in it, and SSH to a team machine makes no
// control-plane request, so the host refreshes its own state once before refusing (the first `pb ssh` of a new session
// failed until an exec had refreshed the host). A capability the refreshed document still lacks stays refused.
func TestOfflineStreamRefreshesTheHostsSignedStateBeforeRefusing(t *testing.T) {
	leaks := goleak.IgnoreCurrent()
	t.Cleanup(func() { goleak.VerifyNone(t, leaks) })
	t.Setenv("IN_TS_TEST", "true")
	dm := integration.RunDERPAndSTUN(t, func(string, ...any) {}, "127.0.0.1")
	signerPublic, signerPrivate, _ := ed25519.GenerateKey(rand.Reader)
	clientTLS, clientFingerprint := testTLS(t, "cli")
	serverTLS, serverFingerprint := testTLS(t, "machine")
	clientBinding := tailnet.NetworkBinding{AccountID: "account_peer", EndpointID: "cli_client", Role: "cli", EndpointGeneration: 1, QUICCertificateFingerprint: clientFingerprint, VirtualAddress: "fd7a:115c:a1e0::1"}
	serverBinding := tailnet.NetworkBinding{AccountID: "account_host", EndpointID: "machine_host", Role: "machine", MachineID: "machine_host", EndpointGeneration: 1, MachineGeneration: 1, QUICCertificateFingerprint: serverFingerprint, VirtualAddress: "fd7a:115c:a1e0::2"}
	clientAuthority := testAuthority(t, clientBinding, testKeys{"native_test": signerPublic})
	serverAuthority := testAuthority(t, serverBinding, testKeys{"native_test": signerPublic})
	clientBinding.WireGuardPublicKey, clientBinding.KeyGeneration = prepareBinding(t, clientAuthority)
	serverBinding.WireGuardPublicKey, serverBinding.KeyGeneration = prepareBinding(t, serverAuthority)
	now := time.Now().Unix()
	clientConfig := testConfiguration(now, 1, clientBinding, serverBinding, "dial")
	serverConfig := testConfiguration(now, 1, serverBinding, clientBinding, "accept")
	applyTestConfiguration(t, clientAuthority, signerPrivate, clientConfig)
	applyTestConfiguration(t, serverAuthority, signerPrivate, serverConfig)

	var refreshes atomic.Int32
	clientOwner, err := native.NewOwner(native.Config{Authority: clientAuthority, TLS: clientTLS})
	if err != nil {
		t.Fatal(err)
	}
	defer clientOwner.Close()
	serverOwner, err := native.NewOwner(native.Config{Authority: serverAuthority, TLS: serverTLS,
		AdmitOffline: func(context.Context, native.OfflineAdmission) error { return nil },
		RefreshAuthority: func(context.Context) error {
			if refreshes.Add(1) > 1 {
				return nil // the document already holds what this peer may do
			}
			serverConfig.Generation++
			serverConfig.Peers[0].Scopes = append(serverConfig.Peers[0].Scopes,
				tailnet.NetworkScope{ResourceKind: "machine_access", ResourceID: "machine_host", ResourceGeneration: 1, Capability: "managed_ssh", Direction: "accept", Port: tailnet.NetworkPort, ExpiresAt: serverConfig.ExpiresAt})
			applyTestConfiguration(t, serverAuthority, signerPrivate, serverConfig)
			return nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	defer serverOwner.Close()
	descriptor := startOfflineTestServer(t, serverOwner, serverAuthority, dm.Regions[1])
	session, err := clientOwner.Dial(t.Context(), descriptor, "machine_host", peerquic.ClassInteractive)
	if err != nil {
		t.Fatal(err)
	}
	echo := func(consumer string) error {
		header, err := streamauth.NewOffline("op_"+consumer, consumer, "stream_"+consumer, time.Now().Add(time.Minute), 1<<20)
		if err != nil {
			return err
		}
		stream, err := session.OpenOffline(t.Context(), header)
		if err != nil {
			return err
		}
		defer stream.Close()
		if _, err := stream.Write([]byte("x")); err != nil {
			return err
		}
		_ = stream.SetReadDeadline(time.Now().Add(5 * time.Second))
		var response [1]byte
		if _, err := io.ReadFull(stream, response[:]); err != nil || response[0] != 'x' {
			return errors.New("not echoed")
		}
		return nil
	}
	if err := echo("ssh"); err != nil {
		t.Fatalf("ssh from a peer the host's document lacked until its refresh: %v", err)
	}
	if err := echo("ssh"); err != nil || refreshes.Load() != 1 {
		t.Fatalf("a held scope refreshed again or was refused: err=%v refreshes=%d", err, refreshes.Load())
	}
	if err := echo("exec"); err == nil || refreshes.Load() != 2 {
		t.Fatalf("exec without a scope: err=%v refreshes=%d, want refused after one refresh", err, refreshes.Load())
	}
}

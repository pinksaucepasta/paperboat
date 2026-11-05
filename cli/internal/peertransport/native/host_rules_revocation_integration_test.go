package native_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"net"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/peertransport/native"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/peerquic"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/streamauth"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
	"go.uber.org/goleak"
	"tailscale.com/tstest/integration"
)

// A signed-in computer's exec is admitted from the host's standing rule (D1), with no per-operation grant. Revoking
// the user's access withdraws that rule from the host's signed network (the revocation feed wakes the refresh), and
// the withdrawal alone must end the running exec at once and refuse the next one.
func TestWithdrawnStandingRuleEndsHostRulesExecAndRefusesTheNext(t *testing.T) {
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
	applyTestConfiguration(t, clientAuthority, signerPrivate, testConfiguration(now, 1, clientBinding, serverBinding, "dial"))
	serverConfig := testConfiguration(now, 1, serverBinding, clientBinding, "accept")
	serverConfig.Peers[0].Scopes = []tailnet.NetworkScope{{ResourceKind: "machine_access", ResourceID: "machine_host", ResourceGeneration: 1, Capability: "exec", Direction: "accept", Port: tailnet.NetworkPort, ExpiresAt: 0}}
	applyTestConfiguration(t, serverAuthority, signerPrivate, serverConfig)

	clientOwner, err := native.NewOwner(native.Config{Authority: clientAuthority, TLS: clientTLS})
	if err != nil {
		t.Fatal(err)
	}
	defer clientOwner.Close()
	serverOwner, err := native.NewOwner(native.Config{Authority: serverAuthority, TLS: serverTLS, AdmitOffline: func(context.Context, native.OfflineAdmission) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer serverOwner.Close()
	descriptor := startOfflineTestServer(t, serverOwner, serverAuthority, dm.Regions[1])
	session, err := clientOwner.Dial(t.Context(), descriptor, "machine_host", peerquic.ClassInteractive)
	if err != nil {
		t.Fatal(err)
	}
	exec := func(operation string) (net.Conn, error) {
		header, err := streamauth.NewOffline(operation, "exec", "stream_"+operation, time.Now().Add(time.Minute), 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		stream, err := session.OpenOffline(t.Context(), header)
		if err != nil {
			return nil, err
		}
		_ = stream.SetDeadline(time.Now().Add(3 * time.Second))
		if _, err := stream.Write([]byte("x")); err != nil {
			stream.Close()
			return nil, err
		}
		if _, err := io.ReadFull(stream, make([]byte, 1)); err != nil {
			stream.Close()
			return nil, err
		}
		_ = stream.SetDeadline(time.Time{})
		return stream, nil
	}
	held, err := exec("op_live")
	if err != nil {
		t.Fatalf("exec under the standing rule: %v", err)
	}
	defer held.Close()

	serverConfig.Generation++
	serverConfig.Peers[0].Scopes = []tailnet.NetworkScope{{ResourceKind: "machine_access", ResourceID: "other_machine", ResourceGeneration: 1, Capability: "exec", Direction: "accept", Port: tailnet.NetworkPort, ExpiresAt: 0}}
	applyTestConfiguration(t, serverAuthority, signerPrivate, serverConfig)
	start := time.Now()
	_ = held.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := held.Read(make([]byte, 1)); err == nil {
		t.Fatal("a running exec survived the withdrawal of its standing rule")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("the withdrawal took %s to end the running exec", elapsed)
	}
	if next, err := exec("op_next"); err == nil {
		next.Close()
		t.Fatal("an exec was admitted after its standing rule was withdrawn")
	}
}

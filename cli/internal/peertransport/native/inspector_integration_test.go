package native_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/native"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/peerquic"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/streamauth"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
	"go.uber.org/goleak"
	"io"
	"tailscale.com/tstest/integration"
	"testing"
	"time"
)

func TestInspectorOnlyNativeCrossAccount(t *testing.T) {
	leaks := goleak.IgnoreCurrent()
	t.Cleanup(func() { goleak.VerifyNone(t, leaks) })
	t.Setenv("IN_TS_TEST", "true")
	dm := integration.RunDERPAndSTUN(t, func(string, ...any) {}, "127.0.0.1")
	signerPublic, signerPrivate, _ := ed25519.GenerateKey(rand.Reader)
	clientTLS, clientFingerprint := testTLS(t, "cli")
	serverTLS, serverFingerprint := testTLS(t, "machine")
	clientBinding := tailnet.NetworkBinding{AccountID: "inspector_teammate", EndpointID: "cli_test", Role: "cli", EndpointGeneration: 1, QUICCertificateFingerprint: clientFingerprint, VirtualAddress: "fd7a:115c:a1e0::1"}
	serverBinding := tailnet.NetworkBinding{AccountID: "account_test", EndpointID: "machine_test", Role: "machine", MachineID: "machine_test", EndpointGeneration: 1, MachineGeneration: 1, QUICCertificateFingerprint: serverFingerprint, VirtualAddress: "fd7a:115c:a1e0::2"}
	clientAuthority := testAuthority(t, clientBinding, testKeys{"native_test": signerPublic})
	serverAuthority := testAuthority(t, serverBinding, testKeys{"native_test": signerPublic})
	clientBinding.WireGuardPublicKey, clientBinding.KeyGeneration = prepareBinding(t, clientAuthority)
	serverBinding.WireGuardPublicKey, serverBinding.KeyGeneration = prepareBinding(t, serverAuthority)
	now := time.Now().Unix()
	clientConfig := testConfiguration(now, 1, clientBinding, serverBinding, "dial")
	serverConfig := testConfiguration(now, 1, serverBinding, clientBinding, "accept")
	clientConfig.Peers[0].Scopes = []tailnet.NetworkScope{{ResourceKind: "inspector", ResourceID: "grant_test", ResourceGeneration: 1, Capability: "inspector", Direction: "dial", Port: 443, ExpiresAt: now + 300}}
	serverConfig.Peers[0].Scopes = []tailnet.NetworkScope{{ResourceKind: "inspector", ResourceID: "grant_test", ResourceGeneration: 1, Capability: "inspector", Direction: "accept", Port: 443, ExpiresAt: now + 300}}
	applyTestConfiguration(t, clientAuthority, signerPrivate, clientConfig)
	applyTestConfiguration(t, serverAuthority, signerPrivate, serverConfig)
	clientOwner, err := native.NewOwner(native.Config{Authority: clientAuthority, TLS: clientTLS})
	if err != nil {
		t.Fatal(err)
	}
	defer clientOwner.Close()
	serverOwner, err := native.NewOwner(native.Config{Authority: serverAuthority, TLS: serverTLS})
	if err != nil {
		t.Fatal(err)
	}
	defer serverOwner.Close()

	descriptor := startTestServer(t, serverOwner, serverAuthority, dm.Regions[1])
	session, err := clientOwner.Dial(t.Context(), descriptor, "machine_test", peerquic.ClassInteractive)
	if err != nil {
		t.Fatal(err)
	}

	defer session.Close()
	header, err := streamauth.NewNativePrivate("inspector_op", "inspector", "inspector_stream", "credential_inspector", time.Now().Add(time.Minute), 4096, []byte(`{"resource_kind":"preview","resource_id":"preview_test","route_id":"preview_test","action":"inspect"}`))
	if err != nil {
		t.Fatal(err)
	}
	stream, err := session.OpenAuthorized(t.Context(), header, "grant_test", "inspector")
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	_ = stream.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err = stream.Write([]byte("bounded inspector response")); err != nil {
		t.Fatal(err)
	}
	payload := make([]byte, len("bounded inspector response"))
	if _, err = io.ReadFull(stream, payload); err != nil || string(payload) != "bounded inspector response" {
		t.Fatalf("inspector response failed: %v", err)
	}
	terminal, _ := streamauth.New("terminal_op", "terminal", "terminal_stream", "credential_terminal", time.Now().Add(time.Minute), 4096)
	if _, err = session.OpenAuthorized(t.Context(), terminal, "grant_test", "terminal"); err == nil {
		t.Fatal("inspector-only peer admitted terminal")
	}
	clientConfig.Generation++
	clientConfig.Peers = nil
	applyTestConfiguration(t, clientAuthority, signerPrivate, clientConfig)
	if _, err = session.OpenAuthorized(t.Context(), header, "grant_test", "inspector"); err == nil {
		t.Fatal("revoked inspector network scope admitted")
	}
}

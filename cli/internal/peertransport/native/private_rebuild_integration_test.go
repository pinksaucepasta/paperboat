package native_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/nativeprivate"
	"github.com/pinksaucepasta/paperboat/cli/internal/peertransport/mesh"
	"github.com/pinksaucepasta/paperboat/cli/internal/peertransport/native"
	"github.com/pinksaucepasta/paperboat/cli/internal/peertransport/peerquic"
	"github.com/pinksaucepasta/paperboat/cli/internal/peertransport/streamauth"
	"github.com/pinksaucepasta/paperboat/cli/internal/peertransport/tailnet"
	"tailscale.com/tstest/integration"
)

// A watchdog rebuild (and a daemon restart, which rebuilds the same pieces)
// closes the machine's owner: open tunnel and private-access streams have no
// reattach, so they must end promptly with an error, never hang, and the next
// connection must reach the rebuilt listener.
func TestPrivateStreamsEndCleanlyAcrossAListenerRebuildAndNewOnesWork(t *testing.T) {
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
	// generation builds what supervise rebuilds: authority, socket, owner, listener.
	generation := func(ordinal uint64) (*native.Owner, mesh.Addr) {
		authority := testAuthority(t, machine, keys)
		bound := machine
		bound.WireGuardPublicKey, bound.KeyGeneration = prepareBinding(t, authority)
		issued := time.Now().Unix()
		applyTestConfiguration(t, authority, signer, testConfiguration(issued, 1, bound, client, "accept"))
		applyTestConfiguration(t, clientAuthority, signer, testConfiguration(issued, ordinal, client, bound, "dial"))
		owner, err := native.NewOwner(native.Config{Authority: authority, TLS: serverTLS})
		if err != nil {
			t.Fatal(err)
		}
		return owner, startTestServer(t, owner, authority, dm.Regions[1])
	}
	expires := time.Now().Add(time.Minute)
	bindings := map[string]nativeprivate.Binding{
		"tunnel":         {Schema: nativeprivate.SchemaV1, ResourceKind: "tunnel", ResourceID: "tun_native", ResourceGeneration: 1, RouteID: "route_native", RouteGeneration: 1, TargetGeneration: 1, OwnerEndpointID: "machine_test", Protocol: "tcp", TargetScheme: "tcp", TargetAddress: "127.0.0.1:5432", ExpiresAt: expires},
		"private_access": {Schema: nativeprivate.SchemaV1, ResourceKind: "machine_service", ResourceID: "machine_test", ResourceGeneration: 1, RouteID: "tcp:5432", RouteGeneration: 1, TargetGeneration: 1, OwnerEndpointID: "machine_test", Protocol: "tcp", TargetScheme: "tcp", TargetAddress: "127.0.0.1:5432", ExpiresAt: expires, UserID: "user_test", CLIClientSessionID: "cli_session_test", AccessSessionID: "access_test", InstallationGeneration: 1, BootID: "boot_test", PolicyGeneration: 1, AnnouncementGeneration: 1},
	}
	open := func(ctx context.Context, address mesh.Addr, kind string) net.Conn {
		t.Helper()
		session, err := clientOwner.Dial(ctx, address, "machine_test", peerquic.ClassInteractive)
		if err != nil {
			t.Fatalf("%s: dial: %v", kind, err)
		}
		target, _ := json.Marshal(bindings[kind])
		header, err := streamauth.NewNativePrivate("operation_"+kind, "private_tcp", "stream_"+kind, "credential_private_tcp", expires, 1<<20, target)
		if err != nil {
			t.Fatal(err)
		}
		stream, err := session.OpenAuthorized(ctx, header, "grant_test", "private_access")
		if err != nil {
			t.Fatalf("%s: open: %v", kind, err)
		}
		echo(t, stream, kind+"-before")
		return stream
	}
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	first, address := generation(2)
	live := map[string]net.Conn{}
	for kind := range bindings {
		live[kind] = open(ctx, address, kind)
	}
	_ = first.Close() // the rebuild; no process restart
	for kind, stream := range live {
		_ = stream.SetReadDeadline(time.Now().Add(15 * time.Second))
		_, err := stream.Read(make([]byte, 1))
		var timeout net.Error
		if err == nil || errors.As(err, &timeout) && timeout.Timeout() {
			t.Fatalf("%s: open stream did not end after the rebuild: %v", kind, err)
		}
		_ = stream.Close()
	}

	second, address := generation(3)
	defer second.Close()
	for kind := range bindings {
		_ = open(ctx, address, kind).Close()
	}
}

func echo(t *testing.T, stream net.Conn, value string) {
	t.Helper()
	_ = stream.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := stream.Write([]byte(value)); err != nil {
		t.Fatal(err)
	}
	response := make([]byte, len(value))
	if _, err := io.ReadFull(stream, response); err != nil || string(response) != value {
		t.Fatalf("echo %q: %q %v", value, response, err)
	}
	_ = stream.SetDeadline(time.Time{})
}

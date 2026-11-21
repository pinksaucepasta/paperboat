package native_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/nativeprivate"
	"github.com/pinksaucepasta/paperboat/cli/internal/peertransport/native"
	"github.com/pinksaucepasta/paperboat/cli/internal/peertransport/peerquic"
	"github.com/pinksaucepasta/paperboat/cli/internal/peertransport/streamauth"
	"github.com/pinksaucepasta/paperboat/cli/internal/peertransport/tailnet"
	"tailscale.com/tstest/integration"
)

// The watchdog's first response replaces only the machine's accept loop
// (runtime restartListener): ending one Owner.Listen and starting another on
// the same owner and socket must leave an established tunnel stream working
// and accept new peers.
func TestRestartingOnlyTheAcceptLoopKeepsEstablishedStreams(t *testing.T) {
	t.Setenv("IN_TS_TEST", "true")
	dm := integration.RunDERPAndSTUN(t, func(string, ...any) {}, "127.0.0.1")
	signerPublic, signer, _ := ed25519.GenerateKey(rand.Reader)
	clientTLS, clientFingerprint := testTLS(t, "cli")
	serverTLS, serverFingerprint := testTLS(t, "machine")
	client := tailnet.NetworkBinding{AccountID: "account_test", EndpointID: "cli_test", Role: "cli", EndpointGeneration: 1, QUICCertificateFingerprint: clientFingerprint, VirtualAddress: "fd7a:115c:a1e0::1"}
	machine := tailnet.NetworkBinding{AccountID: "account_test", EndpointID: "machine_test", Role: "machine", MachineID: "machine_test", EndpointGeneration: 1, MachineGeneration: 1, QUICCertificateFingerprint: serverFingerprint, VirtualAddress: "fd7a:115c:a1e0::2"}
	keys := testKeys{"native_test": signerPublic}
	clientAuthority := testAuthority(t, client, keys)
	serverAuthority := testAuthority(t, machine, keys)
	client.WireGuardPublicKey, client.KeyGeneration = prepareBinding(t, clientAuthority)
	machine.WireGuardPublicKey, machine.KeyGeneration = prepareBinding(t, serverAuthority)
	now := time.Now().Unix()
	applyTestConfiguration(t, clientAuthority, signer, testConfiguration(now, 1, client, machine, "dial"))
	applyTestConfiguration(t, serverAuthority, signer, testConfiguration(now, 1, machine, client, "accept"))
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
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	firstLoop, endFirst := context.WithCancel(ctx)
	address, firstDone := listenTestServer(firstLoop, t, serverOwner, serverAuthority, dm.Regions[1])
	expires := time.Now().Add(time.Minute)
	target, _ := json.Marshal(nativeprivate.Binding{Schema: nativeprivate.SchemaV1, ResourceKind: "tunnel", ResourceID: "tun_native", ResourceGeneration: 1, RouteID: "route_native", RouteGeneration: 1, TargetGeneration: 1, OwnerEndpointID: "machine_test", Protocol: "tcp", TargetScheme: "tcp", TargetAddress: "127.0.0.1:5432", ExpiresAt: expires})
	header, err := streamauth.NewNativePrivate("operation_tunnel", "private_tcp", "stream_tunnel", "credential_private_tcp", expires, 1<<20, target)
	if err != nil {
		t.Fatal(err)
	}
	session, err := clientOwner.Dial(ctx, address, "machine_test", peerquic.ClassInteractive)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := session.OpenAuthorized(ctx, header, "grant_test", "private_access")
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	echo(t, stream, "before-restart")
	endFirst() // restartListener: end only the accept loop...
	<-firstDone
	again, _ := listenTestServer(ctx, t, serverOwner, serverAuthority, dm.Regions[1]) // ...and start another
	if again != address {
		t.Fatalf("the restart replaced the socket: %v then %v", address, again)
	}
	echo(t, stream, "after-restart") // the established tunnel stream is untouched
	next, err := session.OpenAuthorized(ctx, header, "grant_test", "private_access")
	if err != nil {
		t.Fatalf("established session refused a new stream after the restart: %v", err)
	}
	echo(t, next, "new-stream")
	_ = next.Close()
	other, err := clientOwner.Dial(ctx, address, "machine_test", peerquic.ClassInteractive)
	if err != nil {
		t.Fatalf("the restarted accept loop did not take a new handshake: %v", err)
	}
	_ = other.Close()
}

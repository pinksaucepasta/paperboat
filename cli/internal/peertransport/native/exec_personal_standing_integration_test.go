package native_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/peertransport/native"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/peerquic"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/streamauth"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
	"go.uber.org/goleak"
	"tailscale.com/tstest/integration"
)

// FIX-EXECLAT (pbvm2 dev-8aafdbab9): every warm `pb exec` to the caller's own machine refreshed the signed network
// first (875-1457 ms) because the new exec's access session was not in the cached document, although the host admits
// it by its standing rule for its own account. The client dials on the personal mesh pair that stands for that rule;
// once the pair is withdrawn the stream is refused again.
func TestNewExecToOwnMachineDialsOnThePersonalPair(t *testing.T) {
	leaks := goleak.IgnoreCurrent()
	t.Cleanup(func() { goleak.VerifyNone(t, leaks) })
	t.Setenv("IN_TS_TEST", "true")
	dm := integration.RunDERPAndSTUN(t, func(string, ...any) {}, "127.0.0.1")
	signerPublic, signerPrivate, _ := ed25519.GenerateKey(rand.Reader)
	clientTLS, clientFingerprint := testTLS(t, "cli")
	serverTLS, serverFingerprint := testTLS(t, "machine")
	clientBinding := tailnet.NetworkBinding{AccountID: "account_own", EndpointID: "cli_client", Role: "cli", EndpointGeneration: 1, QUICCertificateFingerprint: clientFingerprint, VirtualAddress: "fd7a:115c:a1e0::1"}
	serverBinding := tailnet.NetworkBinding{AccountID: "account_own", EndpointID: "machine_host", Role: "machine", MachineID: "machine_host", EndpointGeneration: 1, MachineGeneration: 1, QUICCertificateFingerprint: serverFingerprint, VirtualAddress: "fd7a:115c:a1e0::2"}
	clientAuthority := testAuthority(t, clientBinding, testKeys{"native_test": signerPublic})
	serverAuthority := testAuthority(t, serverBinding, testKeys{"native_test": signerPublic})
	clientBinding.WireGuardPublicKey, clientBinding.KeyGeneration = prepareBinding(t, clientAuthority)
	serverBinding.WireGuardPublicKey, serverBinding.KeyGeneration = prepareBinding(t, serverAuthority)
	now := time.Now().Unix()
	mesh := func(direction string) tailnet.NetworkScope {
		return tailnet.NetworkScope{ResourceKind: "machine_network", ResourceID: "network_own_pair", ResourceGeneration: 1, Capability: "connect", Direction: direction, Port: tailnet.NetworkPort}
	}
	// The client holds only the mesh pair; the host holds the pair and its standing exec rule for this CLI.
	clientConfig := testConfiguration(now, 1, clientBinding, serverBinding, "dial")
	clientConfig.Peers[0].Scopes = []tailnet.NetworkScope{mesh("dial")}
	serverConfig := testConfiguration(now, 1, serverBinding, clientBinding, "accept")
	serverConfig.Peers[0].Scopes = []tailnet.NetworkScope{mesh("accept"), {ResourceKind: "machine_access", ResourceID: "machine_host", ResourceGeneration: 1, Capability: "exec", Direction: "accept", Port: tailnet.NetworkPort}}
	applyTestConfiguration(t, clientAuthority, signerPrivate, clientConfig)
	applyTestConfiguration(t, serverAuthority, signerPrivate, serverConfig)
	if got := clientAuthority.DialStanding("machine_host", "machine_host", "exec"); got != "network_own_pair" {
		t.Fatalf("standing scope for exec = %q", got)
	}
	if got := clientAuthority.DialStanding("machine_host", "machine_host", "managed_ssh"); got != "" {
		t.Fatalf("managed_ssh has no standing dial rule, got %q", got)
	}
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
	server, err := serverAuthority.Listen(dm.Regions[1])
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	go func() {
		_ = serverOwner.Listen(ctx, dm.Regions[1], func(serveCtx context.Context, session *native.Session) error {
			for {
				stream, _, err := session.AcceptAuthorized(serveCtx, func(_ context.Context, h streamauth.Header) (string, error) {
					if h.Credential != "credential_exec" {
						return "", errors.New("unexpected credential")
					}
					return "grant_new", nil
				})
				if errors.Is(err, native.ErrStreamRejected) {
					continue
				}
				if err != nil {
					return err
				}
				go func() { defer stream.Close(); _, _ = io.Copy(stream, stream) }()
			}
		})
	}()
	session, err := clientOwner.Dial(ctx, server.Address(), "machine_host", peerquic.ClassInteractive)
	if err != nil {
		t.Fatal(err)
	}
	header, err := streamauth.New("op_exec", "exec", "stream_exec", "credential_exec", time.Now().Add(time.Minute), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := session.OpenAuthorized(ctx, header, "grant_new", "exec")
	if err != nil {
		t.Fatal("a new exec to the caller's own machine was refused before the document named its grant:", err)
	}
	if _, err := stream.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	echo := make([]byte, 4)
	if _, err := io.ReadFull(stream, echo); err != nil || string(echo) != "ping" {
		t.Fatalf("echo %q err=%v", echo, err)
	}
	// The pair is withdrawn on the client (the machine was revoked): the open stream ends and no new one opens.
	clientConfig.Generation++
	clientConfig.Peers[0].Scopes = []tailnet.NetworkScope{{ResourceKind: "machine_access", ResourceID: "other", ResourceGeneration: 1, Capability: "terminal", Direction: "dial", Port: tailnet.NetworkPort, ExpiresAt: now + 300}}
	applyTestConfiguration(t, clientAuthority, signerPrivate, clientConfig)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := stream.Write([]byte("x")); err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the stream outlived the withdrawn personal pair")
		}
		time.Sleep(20 * time.Millisecond)
	}
	stream.Close()
	header.StreamID = "stream_exec_2"
	if stream, err := session.OpenAuthorized(ctx, header, "grant_new", "exec"); err == nil {
		stream.Close()
		t.Fatal("an exec was admitted without any scope")
	}
}

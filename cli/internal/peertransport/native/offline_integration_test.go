package native_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/peertransport/native"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/peerquic"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/streamauth"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
	"go.uber.org/goleak"
	"tailscale.com/tailcfg"
	"tailscale.com/tstest/integration"

	"github.com/pinksaucepasta/paperboat/internal/peertransport/mesh"
)

// F21-11: with Paperboat unreachable, a host starts a session from the signed session scope it
// holds for the peer (resource = the host itself), records the decision first, and refuses a
// capability without a scope or when its audit gate refuses.
func TestOfflineSessionsStartFromTheHostsSignedScopes(t *testing.T) {
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
	// The host holds exec and terminal session scopes for the peer, not managed_ssh.
	serverConfig.Peers[0].Scopes = append(serverConfig.Peers[0].Scopes,
		tailnet.NetworkScope{ResourceKind: "machine_access", ResourceID: "machine_host", ResourceGeneration: 1, Capability: "exec", Direction: "accept", Port: tailnet.NetworkPort, ExpiresAt: now + 300},
		tailnet.NetworkScope{ResourceKind: "machine_access", ResourceID: "machine_host", ResourceGeneration: 1, Capability: "terminal", Direction: "accept", Port: tailnet.NetworkPort, ExpiresAt: now + 300})
	applyTestConfiguration(t, clientAuthority, signerPrivate, clientConfig)
	applyTestConfiguration(t, serverAuthority, signerPrivate, serverConfig)

	var mu sync.Mutex
	var decisions []native.OfflineAdmission
	backlogFull := false
	admit := func(_ context.Context, request native.OfflineAdmission) error {
		mu.Lock()
		defer mu.Unlock()
		decisions = append(decisions, request)
		if backlogFull {
			return errors.New("audit backlog full")
		}
		return nil
	}
	clientOwner, err := native.NewOwner(native.Config{Authority: clientAuthority, TLS: clientTLS})
	if err != nil {
		t.Fatal(err)
	}
	defer clientOwner.Close()
	serverOwner, err := native.NewOwner(native.Config{Authority: serverAuthority, TLS: serverTLS, AdmitOffline: admit})
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
		payload := []byte("offline-" + consumer)
		if _, err := stream.Write(payload); err != nil {
			return err
		}
		_ = stream.SetReadDeadline(time.Now().Add(5 * time.Second))
		response := make([]byte, len(payload))
		if _, err := io.ReadFull(stream, response); err != nil {
			return err
		}
		if string(response) != string(payload) {
			return errors.New("wrong echo")
		}
		return nil
	}
	if err := echo("exec"); err != nil {
		t.Fatalf("offline exec with a signed scope: %v", err)
	}
	if err := echo("terminal"); err != nil {
		t.Fatalf("offline terminal with a signed scope: %v", err)
	}
	if err := echo("ssh"); err == nil {
		t.Fatal("offline ssh without a managed_ssh scope was served")
	}
	mu.Lock()
	backlogFull = true
	mu.Unlock()
	if err := echo("exec"); err == nil {
		t.Fatal("offline exec was served although the audit gate refused it")
	}
	mu.Lock()
	defer mu.Unlock()
	want := []struct {
		capability string
		allowed    bool
	}{{"exec", true}, {"terminal", true}, {"managed_ssh", false}, {"exec", true}}
	if len(decisions) != len(want) {
		for _, d := range decisions {
			t.Logf("decision %s allowed=%v", d.Capability, d.Allowed)
		}
		t.Fatalf("recorded %d offline decisions, want %d", len(decisions), len(want))
	}
	for i, w := range want {
		d := decisions[i]
		if d.Capability != w.capability || d.Allowed != w.allowed || d.Peer.EndpointID != "cli_client" || d.Peer.AccountID != "account_peer" || !d.Header.Offline {
			t.Fatalf("decision %d = %+v, want %s allowed=%v from machine_client", i, d, w.capability, w.allowed)
		}
	}
}

// startOfflineTestServer echoes admitted streams and, like nativesession.Service, keeps serving
// the session after a rejected stream.
func startOfflineTestServer(t *testing.T, owner *native.Owner, authority *tailnet.Authority, region *tailcfg.DERPRegion) mesh.Addr {
	t.Helper()
	server, err := authority.Listen(region)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	go func() {
		_ = owner.Listen(ctx, region, func(serveCtx context.Context, session *native.Session) error {
			for {
				stream, _, err := session.AcceptAuthorized(serveCtx, func(context.Context, streamauth.Header) (string, error) {
					return "", errors.New("online credentials are not used here")
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
	return server.Address()
}

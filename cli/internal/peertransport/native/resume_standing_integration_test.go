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

// pbvm4 update check: the held terminal's resume was refused on the client at stream_authorize ("internal") because
// its access session had left the signed scope (native scope admission: resource_found=false), although the host's
// standing rule for the peer was still in force, which is what admits the resume on the host. The client must dial
// with the same standing rule, and a withdrawn standing rule must still refuse it.
func TestResumeFallsBackToTheHostsStandingRule(t *testing.T) {
	leaks := goleak.IgnoreCurrent()
	t.Cleanup(func() { goleak.VerifyNone(t, leaks) })
	t.Setenv("IN_TS_TEST", "true")
	dm := integration.RunDERPAndSTUN(t, func(string, ...any) {}, "127.0.0.1")
	signerPublic, signerPrivate, _ := ed25519.GenerateKey(rand.Reader)
	clientTLS, clientFingerprint := testTLS(t, "cli")
	serverTLS, serverFingerprint := testTLS(t, "machine")
	clientBinding := tailnet.NetworkBinding{AccountID: "account_peer", EndpointID: "cli_client", Role: "cli", EndpointGeneration: 1, QUICCertificateFingerprint: clientFingerprint, VirtualAddress: "fd7a:115c:a1e0::1"}
	serverBinding := tailnet.NetworkBinding{AccountID: "account_peer", EndpointID: "machine_host", Role: "machine", MachineID: "machine_host", EndpointGeneration: 1, MachineGeneration: 1, QUICCertificateFingerprint: serverFingerprint, VirtualAddress: "fd7a:115c:a1e0::2"}
	clientAuthority := testAuthority(t, clientBinding, testKeys{"native_test": signerPublic})
	serverAuthority := testAuthority(t, serverBinding, testKeys{"native_test": signerPublic})
	clientBinding.WireGuardPublicKey, clientBinding.KeyGeneration = prepareBinding(t, clientAuthority)
	serverBinding.WireGuardPublicKey, serverBinding.KeyGeneration = prepareBinding(t, serverAuthority)
	now := time.Now().Unix()
	standing := func(direction string) []tailnet.NetworkScope {
		return []tailnet.NetworkScope{{ResourceKind: "machine_access", ResourceID: "machine_host", ResourceGeneration: 1, Capability: "terminal", Direction: direction, Port: tailnet.NetworkPort, ExpiresAt: now + 300}}
	}
	// Both sides hold only the standing rule: the held terminal's own access session is no longer in either scope.
	clientConfig := testConfiguration(now, 1, clientBinding, serverBinding, "dial")
	clientConfig.Peers[0].Scopes = standing("dial")
	serverConfig := testConfiguration(now, 1, serverBinding, clientBinding, "accept")
	serverConfig.Peers[0].Scopes = standing("accept")
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
	server, err := serverAuthority.Listen(dm.Regions[1])
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	go func() {
		_ = serverOwner.Listen(ctx, dm.Regions[1], func(serveCtx context.Context, session *native.Session) error {
			for {
				stream, _, err := session.AcceptAuthorized(serveCtx, func(context.Context, streamauth.Header) (string, error) {
					return "", errors.New("a resume must not reach the credential check")
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
	header, err := streamauth.NewResume("ses_resume", "stream_resume", "grant_gone", time.Now().Add(time.Minute), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := session.OpenAuthorized(ctx, header, "grant_gone", "terminal")
	if err != nil {
		t.Fatal("a resume whose access session left the scope was refused although the standing rule holds:", err)
	}
	stream.Close()
	// The standing terminal rule is withdrawn on the client (it keeps exec): no resume.
	clientConfig.Generation++
	clientConfig.Peers[0].Scopes = []tailnet.NetworkScope{{ResourceKind: "machine_access", ResourceID: "machine_host", ResourceGeneration: 1, Capability: "exec", Direction: "dial", Port: tailnet.NetworkPort, ExpiresAt: now + 300}}
	applyTestConfiguration(t, clientAuthority, signerPrivate, clientConfig)
	if stream, err := session.OpenAuthorized(ctx, header, "grant_gone", "terminal"); err == nil {
		stream.Close()
		t.Fatal("a resume was admitted without any scope")
	}
}

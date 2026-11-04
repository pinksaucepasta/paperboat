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
	"go.uber.org/goleak"
	"tailscale.com/tstest/integration"
)

// A terminal move carries no credential: the host admits it only while its current authority still
// gives this exact peer terminal access, and names the proven peer to the terminal owner. A withdrawn
// grant or a peer no longer in the network refuses the move.
func TestResumeAdmitsOnlyFromCurrentAuthority(t *testing.T) {
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
	serverConfig := testConfiguration(now, 1, serverBinding, clientBinding, "accept")
	applyTestConfiguration(t, clientAuthority, signerPrivate, testConfiguration(now, 1, clientBinding, serverBinding, "dial"))
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
	peers := make(chan [4]string, 4)
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
				if resume, ok := stream.(*native.ResumeConn); ok {
					account, endpoint, sessionID, resource, _ := resume.ResumePeer()
					peers <- [4]string{account, endpoint, sessionID, resource}
				}
				go func() { defer stream.Close(); _, _ = io.Copy(stream, stream) }()
			}
		})
	}()
	session, err := clientOwner.Dial(ctx, server.Address(), "machine_host", peerquic.ClassInteractive)
	if err != nil {
		t.Fatal(err)
	}
	resume := func() (net.Conn, error) {
		// A revoked peer's packets are dropped, so its attempt ends only at this bound.
		ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		header, err := streamauth.NewResume("ses_resume", "stream_resume", "grant_test", time.Now().Add(time.Minute), 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		return session.OpenAuthorized(ctx, header, "grant_test", "terminal")
	}
	stream, err := resume()
	if err != nil {
		t.Fatal("resume under a live grant was refused:", err)
	}
	if peer := <-peers; peer != [4]string{"account_peer", "cli_client", "ses_resume", "grant_test"} {
		t.Fatalf("resume named %v", peer)
	}
	stream.Close()

	refused := func(why string) {
		t.Helper()
		stream, err := resume()
		if stream != nil {
			stream.Close()
		}
		if err == nil {
			t.Fatalf("%s: resume was admitted", why)
		}
		select {
		case peer := <-peers:
			t.Fatalf("%s: host admitted %v", why, peer)
		default:
		}
	}
	// The terminal grant is withdrawn while the peer keeps exec on it: no resume.
	serverConfig.Generation++
	serverConfig.Peers[0].Scopes = []tailnet.NetworkScope{{ResourceKind: "machine_access", ResourceID: "grant_test", ResourceGeneration: 1, Capability: "exec", Direction: "accept", Port: tailnet.NetworkPort, ExpiresAt: now + 300}}
	applyTestConfiguration(t, serverAuthority, signerPrivate, serverConfig)
	refused("withdrawn grant")
	// A withdrawal ends the old session; a fresh one is admitted to the network but not to resume.
	if session, err = clientOwner.Dial(ctx, server.Address(), "machine_host", peerquic.ClassInteractive); err != nil {
		t.Fatal(err)
	}
	var refusal *native.AdmissionRefusal
	if _, err := resume(); !errors.As(err, &refusal) || refusal.Code != native.AdmissionDenied {
		t.Fatal("withdrawn grant: no typed denial:", err)
	}
	// The peer is revoked: it leaves the host's network.
	serverConfig.Generation++
	serverConfig.Peers = nil
	applyTestConfiguration(t, serverAuthority, signerPrivate, serverConfig)
	refused("revoked peer")
}

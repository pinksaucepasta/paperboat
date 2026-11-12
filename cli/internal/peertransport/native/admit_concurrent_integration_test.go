package native_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/peertransport/native"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/peerquic"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/streamauth"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
	"tailscale.com/tstest/integration"
)

const admissionBound = 90 * time.Second

// A slow admission must not hold up the streams behind it (F21-164): the listener accepts, then admits each stream in its
// own goroutine, so a second stream is admitted while the first one's authorization is still running.
func TestNativeAdmitsStreamsConcurrently(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	serveConcurrentAdmission := func(ctx context.Context, session *native.Session) error {
		for {
			connection, err := session.Accept(ctx)
			if err != nil {
				return err
			}
			go func() {
				admitted, _, err := session.Admit(ctx, connection, func(_ context.Context, header streamauth.Header) (string, error) {
					if header.OperationID == "admission_slow" { // this stream's authorization waits for the test
						<-release
					}
					return "grant_test", nil
				})
				if err != nil {
					return
				}
				defer admitted.Close()
				_, _ = io.Copy(io.Discard, admitted)
			}()
		}
	}
	t.Setenv("IN_TS_TEST", "true")
	ctx, cancel := context.WithTimeout(t.Context(), admissionBound)
	defer cancel()
	dm := integration.RunDERPAndSTUN(t, func(string, ...any) {}, "127.0.0.1")
	signerPublic, signerPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	clientTLS, clientFingerprint := testTLS(t, "admission-cli")
	serverTLS, serverFingerprint := testTLS(t, "admission-machine")
	clientBinding := tailnet.NetworkBinding{AccountID: "account_admission", EndpointID: "cli_admission", Role: "cli", EndpointGeneration: 1, QUICCertificateFingerprint: clientFingerprint, VirtualAddress: "fd7a:115c:a1e0::81"}
	serverBinding := tailnet.NetworkBinding{AccountID: "account_admission", EndpointID: "machine_admission", Role: "machine", MachineID: "machine_admission", EndpointGeneration: 1, MachineGeneration: 1, QUICCertificateFingerprint: serverFingerprint, VirtualAddress: "fd7a:115c:a1e0::82"}
	clientAuthority := testAuthority(t, clientBinding, testKeys{"native_test": signerPublic})
	serverAuthority := testAuthority(t, serverBinding, testKeys{"native_test": signerPublic})
	clientBinding.WireGuardPublicKey, clientBinding.KeyGeneration = prepareBinding(t, clientAuthority)
	serverBinding.WireGuardPublicKey, serverBinding.KeyGeneration = prepareBinding(t, serverAuthority)
	now := time.Now().Unix()
	applyTestConfiguration(t, clientAuthority, signerPrivate, testConfiguration(now, 1, clientBinding, serverBinding, "dial"))
	applyTestConfiguration(t, serverAuthority, signerPrivate, testConfiguration(now, 1, serverBinding, clientBinding, "accept"))
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

	descriptor, err := serverAuthority.Listen(dm.Regions[1])
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- serverOwner.Listen(ctx, dm.Regions[1], serveConcurrentAdmission) }()
	defer func() {
		cancel()
		_ = serverOwner.Close()
		select {
		case <-serveDone:
		case <-time.After(2 * time.Second):
			t.Error("admission listener did not stop")
		}
	}()
	session, err := clientOwner.Dial(ctx, descriptor.Address(), "machine_admission", peerquic.ClassInteractive)
	if err != nil {
		t.Fatal(err)
	}
	slowOpened := make(chan error, 1)
	go func() {
		stream, err := openAdmissionStream(ctx, session, "slow")
		if err == nil {
			defer stream.Close()
		}
		slowOpened <- err
	}()
	fast, err := openAdmissionStream(ctx, session, "fast")
	if err != nil {
		t.Fatalf("the second stream was not admitted while the first one's authorization was blocked: %v", err)
	}
	defer fast.Close()
	select {
	case err := <-slowOpened:
		t.Fatalf("the blocked stream was admitted before it was released: %v", err)
	default:
	}
	releaseOnce.Do(func() { close(release) })
	if err := <-slowOpened; err != nil {
		t.Fatalf("the first stream failed after its authorization was released: %v", err)
	}
}

func openAdmissionStream(ctx context.Context, session *native.Session, consumer string) (net.Conn, error) {
	header, err := streamauth.New("admission_"+consumer, "terminal", "admission_"+consumer, "credential_"+consumer, time.Now().Add(time.Minute), 1<<20)
	if err != nil {
		return nil, err
	}
	return session.OpenAuthorized(ctx, header, "grant_test", "terminal")
}

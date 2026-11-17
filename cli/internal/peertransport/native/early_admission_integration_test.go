package native_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/nativesession"
	"github.com/pinksaucepasta/paperboat/cli/internal/peertransport/native"
	"github.com/pinksaucepasta/paperboat/cli/internal/peertransport/peerquic"
	"github.com/pinksaucepasta/paperboat/cli/internal/peertransport/streamauth"
	"github.com/pinksaucepasta/paperboat/cli/internal/peertransport/tailnet"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=native-early-admission-regression
	"tailscale.com/tstest/integration"
)

// An exec stream sends its first bytes in the same flight as its credential. A refused stream must fail closed on the
// first Read with the typed refusal and the host must never see those bytes; a temporary refusal resends them on a
// new stream exactly once.
func TestNativeExecEarlyAdmission(t *testing.T) {
	t.Setenv("IN_TS_TEST", "true")
	dm := integration.RunDERPAndSTUN(t, func(string, ...any) {}, "127.0.0.1")
	signerPublic, signerPrivate, _ := ed25519.GenerateKey(rand.Reader)
	clientTLS, clientFingerprint := testTLS(t, "cli")
	serverTLS, serverFingerprint := testTLS(t, "machine")
	clientBinding := tailnet.NetworkBinding{AccountID: "account_test", EndpointID: "cli_test", Role: "cli", EndpointGeneration: 1, QUICCertificateFingerprint: clientFingerprint, VirtualAddress: "fd7a:115c:a1e0::1"}
	serverBinding := tailnet.NetworkBinding{AccountID: "account_test", EndpointID: "machine_test", Role: "machine", MachineID: "machine_test", EndpointGeneration: 1, MachineGeneration: 1, QUICCertificateFingerprint: serverFingerprint, VirtualAddress: "fd7a:115c:a1e0::2"}
	filter := &directFilter{}
	filter.allowDERP.Store(true)
	clientAuthority := testAuthority(t, clientBinding, testKeys{"native_test": signerPublic}, filter)
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
	var temporary atomic.Int32
	var mu sync.Mutex
	served := map[string][]byte{} // operation -> bytes the host application read
	apps, err := nativesession.New(nativesession.Config{Authorize: func(_ context.Context, h streamauth.Header) (string, error) {
		switch h.Credential {
		case "invalid":
			return "", errors.New("denied")
		case "starting":
			if temporary.Add(1) == 1 {
				return "", &native.AdmissionRefusal{Code: native.AdmissionUnavailable, Err: errors.New("worker starting")}
			}
		}
		return "grant_test", nil
	}, ServeStream: func(_ context.Context, h streamauth.Header, c net.Conn) error {
		var b [5]byte
		if _, err := io.ReadFull(c, b[:]); err != nil {
			return err
		}
		mu.Lock()
		served[h.OperationID] = append(served[h.OperationID], b[:]...)
		mu.Unlock()
		_, err := c.Write(b[:])
		return err
	}, ServeTransfer: func(context.Context, net.Conn) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	server, err := serverAuthority.Listen(dm.Regions[1])
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = serverOwner.Listen(ctx, dm.Regions[1], apps.Serve)
	}()
	defer func() { cancel(); serverOwner.Close(); <-done; apps.Wait() }()
	session, err := clientOwner.Dial(ctx, server.Address(), "machine_test", peerquic.ClassInteractive)
	if err != nil {
		t.Fatal(err)
	}
	open := func(id, credential string) net.Conn {
		t.Helper()
		h, err := streamauth.New(id, "exec", id, credential, time.Now().Add(time.Minute), 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		c, err := session.OpenAuthorized(ctx, h, "grant_test", "exec")
		if err != nil {
			t.Fatalf("%s: an exec stream must open before its admission: %v", id, err)
		}
		_ = c.SetDeadline(time.Now().Add(5 * time.Second))
		if _, err := c.Write([]byte("start")); err != nil {
			t.Fatalf("%s: write ahead of admission: %v", id, err)
		}
		return c
	}

	denied := open("denied", "invalid")
	var b [5]byte
	_, err = io.ReadFull(denied, b[:])
	var refusal *native.AdmissionRefusal
	if !errors.As(err, &refusal) || refusal.Code != native.AdmissionDenied {
		t.Fatalf("refused exec stream read %v, want the typed denial", err)
	}
	if _, err := denied.Write([]byte("again")); err == nil {
		t.Fatal("a refused exec stream accepted a write after its refusal")
	}
	denied.Close()

	retried := open("retried", "starting")
	if _, err := io.ReadFull(retried, b[:]); err != nil || string(b[:]) != "start" {
		t.Fatalf("exec stream after a temporary refusal: %q %v", b, err)
	}
	retried.Close()
	if temporary.Load() != 2 {
		t.Fatalf("admission ran %d times, want one refusal then one admission", temporary.Load())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(served["denied"]) != 0 {
		t.Fatal("the host application read bytes of a refused stream")
	}
	if string(served["retried"]) != "start" {
		t.Fatalf("host read %q on the retried stream, want the request exactly once", served["retried"])
	}
}

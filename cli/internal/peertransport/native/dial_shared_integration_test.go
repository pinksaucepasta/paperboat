package native_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/peertransport/native"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/peerquic"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/streamauth"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
	"tailscale.com/tstest/integration"
)

// One client holds many terminals on one machine over ONE QUIC association (one UDP flow), not one association each
// (F21-164: tailnet.MaxFlows = 64 flows per client stopped it at ~40 terminals).
func TestNativeDialSharedHoldsHundredTerminalsOnOneAssociation(t *testing.T) {
	serveConcurrentAdmission := func(ctx context.Context, session *native.Session) error {
		for {
			connection, err := session.Accept(ctx)
			if err != nil {
				return err
			}
			go func() {
				admitted, _, err := session.Admit(ctx, connection, func(context.Context, streamauth.Header) (string, error) { return "grant_test", nil })
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
	const terminals = 100
	var wg sync.WaitGroup
	handles := make([]*native.Session, terminals)
	errs := make([]error, terminals)
	for i := range handles {
		wg.Add(1)
		go func() {
			defer wg.Done()
			handles[i], errs[i] = clientOwner.DialShared(ctx, descriptor.Address(), "machine_admission", peerquic.ClassInteractive)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("dial %d: %v", i, err)
		}
	}
	if got := clientOwner.OpenSessions(); got != 1 {
		t.Fatalf("%d dials hold %d QUIC associations, want 1", terminals, got)
	}
	streams := make([]interface{ Close() error }, 0, terminals)
	for i, handle := range handles {
		stream, err := openAdmissionStream(ctx, handle, fmt.Sprintf("s%d", i))
		if err != nil {
			t.Fatalf("terminal %d: %v", i, err)
		}
		streams = append(streams, stream)
	}
	for i := 0; i < terminals-1; i++ {
		_ = streams[i].Close()
		_ = handles[i].Close()
	}
	if got := clientOwner.OpenSessions(); got != 1 {
		t.Fatalf("with one handle left the association count is %d, want 1 (closing a handle must not close the shared association)", got)
	}
	// The last handle closing ends the association; the next dial makes a new one.
	_ = streams[terminals-1].Close()
	_ = handles[terminals-1].Close()
	deadline := time.Now().Add(5 * time.Second)
	for clientOwner.OpenSessions() != 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := clientOwner.OpenSessions(); got != 0 {
		t.Fatalf("after the last handle closed %d associations remain", got)
	}
	again, err := clientOwner.DialShared(ctx, descriptor.Address(), "machine_admission", peerquic.ClassInteractive)
	if err != nil {
		t.Fatalf("dial after the association closed: %v", err)
	}
	defer again.Close()
	if got := clientOwner.OpenSessions(); got != 1 {
		t.Fatalf("a new dial after close holds %d associations, want 1", got)
	}
}

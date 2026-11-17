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

	"github.com/pinksaucepasta/paperboat/cli/internal/peertransport/native"
	"github.com/pinksaucepasta/paperboat/cli/internal/peertransport/peerquic"
	"github.com/pinksaucepasta/paperboat/cli/internal/peertransport/streamauth"
	"github.com/pinksaucepasta/paperboat/cli/internal/peertransport/tailnet"
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
	ctx, clientOwner, dial := startSharedPair(t, serveConcurrentAdmission)
	const terminals = 100
	var wg sync.WaitGroup
	handles := make([]*native.Session, terminals)
	errs := make([]error, terminals)
	for i := range handles {
		wg.Add(1)
		go func() {
			defer wg.Done()
			handles[i], errs[i] = dial(ctx)
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
	// The last handle closing leaves the association open for associationLinger: a dial right after reuses it.
	_ = streams[terminals-1].Close()
	_ = handles[terminals-1].Close()
	if got := clientOwner.OpenSessions(); got != 1 {
		t.Fatalf("right after the last handle closed %d associations remain, want the lingering 1", got)
	}
	reused, err := dial(ctx)
	if err != nil {
		t.Fatalf("dial during the linger: %v", err)
	}
	stream, err := openAdmissionStream(ctx, reused, "reused")
	if err != nil {
		t.Fatalf("stream on the lingering association: %v", err)
	}
	if got := clientOwner.OpenSessions(); got != 1 {
		t.Fatalf("a dial during the linger holds %d associations, want the same 1", got)
	}
	_ = stream.Close()
	_ = reused.Close()
	// Idle past the linger, it closes; the next dial makes a new one.
	deadline := time.Now().Add(15 * time.Second)
	for clientOwner.OpenSessions() != 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if got := clientOwner.OpenSessions(); got != 0 {
		t.Fatalf("after the linger %d associations remain", got)
	}
	again, err := dial(ctx)
	if err != nil {
		t.Fatalf("dial after the association closed: %v", err)
	}
	defer again.Close()
	if got := clientOwner.OpenSessions(); got != 1 {
		t.Fatalf("a new dial after close holds %d associations, want 1", got)
	}
}

// startSharedPair runs a server owner whose accepted streams go to serve and returns a client owner with a dial of its
// shared association, and the test context.
func startSharedPair(t *testing.T, serve func(context.Context, *native.Session) error) (context.Context, *native.Owner, func(context.Context) (*native.Session, error)) {
	t.Setenv("IN_TS_TEST", "true")
	ctx, cancel := context.WithTimeout(t.Context(), admissionBound)
	t.Cleanup(cancel)
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
	t.Cleanup(func() { _ = clientOwner.Close() })
	serverOwner, err := native.NewOwner(native.Config{Authority: serverAuthority, TLS: serverTLS})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = serverOwner.Close() })

	descriptor, err := serverAuthority.Listen(dm.Regions[1])
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- serverOwner.Listen(ctx, dm.Regions[1], serve) }()
	t.Cleanup(func() {
		cancel()
		_ = serverOwner.Close()
		select {
		case <-serveDone:
		case <-time.After(2 * time.Second):
			t.Error("admission listener did not stop")
		}
	})
	return ctx, clientOwner, func(c context.Context) (*native.Session, error) {
		return clientOwner.DialShared(c, descriptor.Address(), "machine_admission", peerquic.ClassInteractive)
	}
}

// pb exec with a big stdin that the command never reads, interrupted with Ctrl-C, then another command seconds later (inside
// associationLinger): the second command's streams must open on the lingering association (device repro, 2026-11-16: the
// second run's cancel took 25 s and every exec after it hung until the daemon restarted).
func TestNativeDialSharedOpensStreamsAfterAClogReset(t *testing.T) {
	notReading := func(ctx context.Context, session *native.Session) error {
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
				<-ctx.Done() // never reads: the sender's window fills
			}()
		}
	}
	ctx, owner, dial := startSharedPair(t, notReading)
	first, err := dial(ctx)
	if err != nil {
		t.Fatal(err)
	}
	header, err := streamauth.New("admission_clog", "terminal", "admission_clog", "credential_clog", time.Now().Add(time.Minute), 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	clogged, err := first.OpenAuthorized(ctx, header, "grant_test", "terminal")
	if err != nil {
		t.Fatal(err)
	}
	writing := make(chan struct{})
	go func() { // fills the connection window and blocks in Write until the stream is reset
		defer close(writing)
		chunk := make([]byte, 64<<10)
		for {
			if _, err := clogged.Write(chunk); err != nil {
				return
			}
		}
	}()
	time.Sleep(time.Second)
	// A detached exec attachment resets its stream (Close would leave the unread data in the connection window).
	clogged.(interface{ Abort() }).Abort()
	_ = first.Close()
	select {
	case <-writing:
	case <-time.After(10 * time.Second):
		t.Fatal("the clogged write did not end when its stream was closed")
	}
	time.Sleep(time.Second) // seconds later, inside the 5 s linger
	second, err := dial(ctx)
	if err != nil {
		t.Fatalf("dial during the linger: %v", err)
	}
	defer second.Close()
	openCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	stream, err := openAdmissionStream(openCtx, second, "after_clog")
	if err != nil {
		t.Fatalf("a stream on the association after a clogged stream was reset did not open: %v", err)
	}
	_ = stream.Close()
	if got := owner.OpenSessions(); got != 1 {
		t.Fatalf("%d associations, want the one lingering", got)
	}
}

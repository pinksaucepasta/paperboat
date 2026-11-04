//go:build linux || darwin

package runtime

import (
	"context"
	"crypto/ed25519"
	"errors"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/session"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workloadbridge"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/native"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/peerquic"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
)

// pb update, real machine state: a stream opened on the serving (primary)
// carrier survives the next generation's start and the move; the move routes
// the client's new connections to the delegated generation, which keeps
// serving them after the old carrier's last stream closed and it exited. The
// generation runs on its own authority instance over the same enrolled state,
// as the separate carrier process does, and never binds the primary network.
func TestDelegatedCarrierGenerationServesAfterThePrimaryRetires(t *testing.T) {
	if !targetCarrierRoot(t) {
		return
	}
	f := newTargetCarrierFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	if _, err := f.client.TerminalCarrier(ctx, workloadbridge.CarrierRequest{Operation: "start", Slot: 1}); err != nil {
		t.Fatal(err)
	}
	f.ensureNativePrimary(t, ctx)
	old, err := f.nativeClient.Dial(ctx, f.primaryAddress, "machine_target", peerquic.ClassInteractive)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := session.Snapshot{ID: "ses_target", Generation: 1}
	held := f.admit(t, ctx, old, snapshot)

	s := f.startDelegatedGeneration(t, ctx)
	offer, err := s.generationOffer()
	if err != nil || !offer.Claims.AllApplications {
		t.Fatal("delegated generation has no offer for every application", err)
	}
	assertOpen(t, held)
	if err := f.primaryOwner.OfferCarrier(ctx, offer); err != nil {
		t.Fatal("the serving carrier's client did not take the generation", err)
	}
	assertOpen(t, held)
	moved, err := f.nativeClient.Dial(ctx, f.primaryAddress, "machine_target", peerquic.ClassInteractive)
	if err != nil {
		t.Fatal(err)
	}
	f.admit(t, ctx, moved, snapshot)

	// The old carrier exits once its last stream is done.
	held.Close()
	old.Close()
	if err := f.primaryOwner.Close(); err != nil {
		t.Fatal(err)
	}
	fresh, err := f.nativeClient.Dial(ctx, f.primaryAddress, "machine_target", peerquic.ClassInteractive)
	if err != nil {
		t.Fatal("a new connection after the old carrier exited did not reach the generation", err)
	}
	defer fresh.Close()
	f.admit(t, ctx, fresh, snapshot)
	assertOpen(t, f.admit(t, ctx, moved, snapshot))
}

// startDelegatedGeneration starts a delegated generation from the fixture's
// enrolled state, serving the staged terminal endpoint's applications.
func (f *targetCarrierFixture) startDelegatedGeneration(t *testing.T, ctx context.Context) *productionNativePeerService {
	t.Helper()
	tlsConfig, _, self, err := loadNativePeerTLS(f.root, "machine_target", 1)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := tailnet.NewAuthority(tailnet.AuthorityOptions{Store: config.ProfileStore{Path: filepath.Join(f.root, "peer-network"), Secrets: config.FileSecretStore{Dir: filepath.Join(f.root, "peer-network", "secrets")}}, Issuer: f.issuer, Self: self, Keys: targetCarrierKeys{f.signer.Public().(ed25519.PublicKey)}})
	if err != nil {
		t.Fatal(err)
	}
	if err := authority.Restore(ctx); err != nil {
		t.Fatal(err)
	}
	owner, err := native.NewOwner(native.Config{Authority: authority, TLS: tlsConfig})
	if err != nil {
		t.Fatal(err)
	}
	_, source, err := installsource.Current()
	if err != nil {
		t.Fatal(err)
	}
	run, cancel := context.WithCancel(t.Context())
	s := &productionNativePeerService{config: productionNativePeerConfig{carrierGeneration: source.SHA256[:32]}}
	g := &productionNativePeerGeneration{cancel: cancel, owner: owner, apps: f.owner.carrier.apps, errors: make(chan error, 2), authority: authority, control: f.owner.carrier.control}
	if err := s.startDelegatedGeneration(run, ctx, g); err != nil {
		cancel()
		_ = owner.Close()
		t.Fatal(err)
	}
	s.current = g
	f.mu.Lock()
	f.generation = s
	f.mu.Unlock()
	// The applications are the staged endpoint's, which joins them itself.
	t.Cleanup(func() { g.apps = nil; _ = g.stop() })
	return s
}

func assertOpen(t *testing.T, stream net.Conn) {
	t.Helper()
	stream.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	defer stream.SetReadDeadline(time.Time{})
	var b [1]byte
	_, err := stream.Read(b[:])
	var deadline net.Error
	if !errors.As(err, &deadline) || !deadline.Timeout() {
		t.Fatal("an open stream was closed", err)
	}
}

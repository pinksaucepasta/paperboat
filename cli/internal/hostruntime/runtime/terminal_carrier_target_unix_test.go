//go:build linux || darwin

package runtime

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostinstall"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/pty"
	nativeservice "github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/session"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/sessionowner"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workloadbridge"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/mesh"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/native"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
)

// The child exercises protected declarations and the real production factory,
// not an injected service constructor. CI's runner has passwordless sudo.
func TestTargetCarrierStagesThroughOwnerRPCAndLeavesPTYRunning(t *testing.T) {
	if !targetCarrierRoot(t) {
		return
	}
	f := newTargetCarrierFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	shell, err := filepath.EvalSymlinks("/bin/sh")
	if err != nil {
		t.Fatal(err)
	}
	created, err := f.owner.sessions.Create(ctx, session.CreateRequest{ID: "ses_target", Name: "target", Command: pty.Command{Path: shell, Args: []string{"-c", "stty -echo; exec cat"}, CWD: f.root, Dimensions: pty.Dimensions{Columns: 80, Rows: 24}}})
	if err != nil {
		t.Fatal(err)
	}
	ready, err := f.client.TerminalCarrier(ctx, workloadbridge.CarrierRequest{Operation: "start", Slot: 1})
	if err != nil || ready.Slot != 1 || len(ready.Offer) == 0 {
		t.Fatal("target did not stage its actual production listener", err)
	}
	offer, err := native.ParseCarrierOffer(ready.Offer)
	if err != nil {
		t.Fatal(err)
	}
	connection := f.dial(t, ctx, offer)
	defer connection.Close()
	held := f.admit(t, ctx, connection, created)
	if err := f.client.Retire(ctx); !errors.Is(err, session.ErrUpdateBusy) {
		t.Fatal("retirement did not preserve its running PTY", err)
	}
	// A rejected catalogue retirement must leave the transport serving too.
	f.admit(t, ctx, connection, created)
	if _, err := f.client.TerminalCarrier(ctx, workloadbridge.CarrierRequest{Operation: "seal"}); err != nil {
		t.Fatal(err)
	}
	// Closing new admission must not disturb the accepted terminal connection.
	f.admit(t, ctx, connection, created)
	f.assertNoNewCarrierAdmission(t, ctx, offer)
	// Fresh preparation must decline while the server is unreachable, while
	// the already accepted generation keeps its independently owned terminal.
	f.closeEvents()
	f.control.CloseClientConnections()
	f.control.Close()
	environ, err := f.owner.carrierEnvironment(ctx)
	if err != nil {
		t.Fatal(err)
	}
	staging, stopStaging := context.WithTimeout(ctx, 3*time.Second)
	failed, err := newTerminalCarrier(staging, "target-test", environ, 2, f.owner.durable)
	stopStaging()
	if err == nil {
		failed.Close(ctx)
		t.Fatal("offline preparation advertised a ready generation")
	}

	f.admit(t, ctx, connection, created)
	stopped := make(chan error, 1)
	go func() {
		_, err := f.client.TerminalCarrier(ctx, workloadbridge.CarrierRequest{Operation: "stop"})
		stopped <- err
	}()
	f.assertDisconnected(t, held, time.Second)

	if err := <-stopped; err != nil {
		t.Fatal("target generation did not stop", err)
	}
	if after, err := f.owner.sessions.Snapshot(created.ID); err != nil || after.State != session.Running || after.Generation != created.Generation {
		t.Fatal("transport shutdown replaced or stopped the independent PTY", err)
	}
	state, err := f.client.TerminalCarrier(ctx, workloadbridge.CarrierRequest{Operation: "status"})
	if err != nil || state.Slot != 0 || len(state.Offer) != 0 {
		t.Fatal("finished drain retained its generation", err)
	}

}

func TestTargetCarrierMachineRevocationKillsBothGenerations(t *testing.T) {
	if !targetCarrierRoot(t) {
		return
	}
	f := newTargetCarrierFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	first, err := f.client.TerminalCarrier(ctx, workloadbridge.CarrierRequest{Operation: "start", Slot: 1})
	if err != nil {
		t.Fatal(err)
	}
	environ, err := f.owner.carrierEnvironment(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Two production factories: separate authorities, child keys, TLS sessions
	// and renewal loops, sharing only the enrolled machine's identity/store.
	second, err := newTerminalCarrier(ctx, "target-test", environ, 2, f.owner.durable)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if err := second.Close(cleanup); err != nil {
			t.Error(err)
		}
	})
	offer, err := native.ParseCarrierOffer(first.Offer)
	if err != nil {
		t.Fatal(err)
	}
	a, b := f.dial(t, ctx, offer), f.dial(t, ctx, second.offer, second)
	defer a.Close()
	defer b.Close()
	snapshot := session.Snapshot{ID: "ses_target", Generation: 1}
	heldA := f.admit(t, ctx, a, snapshot)
	heldB := f.admit(t, ctx, b, snapshot)
	f.mu.Lock()
	f.revoked = true
	f.closeEvents()
	f.mu.Unlock()
	f.assertDisconnected(t, heldA, 3*time.Second)
	f.assertDisconnected(t, heldB, 3*time.Second)

	state, err := f.client.TerminalCarrier(ctx, workloadbridge.CarrierRequest{Operation: "status"})
	if err != nil || len(state.Offer) != 0 || second.ready() {
		t.Fatal("revoked machine still advertises usable delegation", err)
	}
}

type targetCarrierFixture struct {
	root            string
	owner           *IndependentSessionOwner
	client          *workloadbridge.Client
	control         *httptest.Server
	clientTLS       tls.Certificate
	clientAuthority *tailnet.Authority
	nativeClient    *native.Owner
	primaryOwner    *native.Owner
	primaryAddress  mesh.Addr
	parent          *native.Session
	extraTarget     *terminalCarrier
	issuer          string
	signer          ed25519.PrivateKey
	mu              sync.Mutex
	network         tailnet.NetworkConfiguration
	events          chan struct{}
	eventsOnce      sync.Once
	networkChanged  chan struct{}
	eventRevision   int64
	revoked         bool
	requests        map[string]int
}

func newTargetCarrierFixture(t *testing.T) *targetCarrierFixture {
	t.Helper()
	layout, err := nativeservice.UserLayout(runtime.GOOS, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(layout.InstallRoot); !os.IsNotExist(err) {
		t.Fatal("isolated root fixture installation path is already occupied")
	}
	if err := os.MkdirAll(layout.InstallRoot, 0755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(layout.InstallRoot) })
	root, err := os.MkdirTemp("/var/lib", "pb-carrier-test-")
	if runtime.GOOS == "darwin" {
		root, err = os.MkdirTemp("/Library", "pb-carrier-test-")
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	f := &targetCarrierFixture{root: root, events: make(chan struct{}), networkChanged: make(chan struct{}), requests: make(map[string]int)}
	public, signer, _ := ed25519.GenerateKey(rand.Reader)
	f.signer = signer
	f.control = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { f.serve(t, public, w, r) }))
	f.issuer = f.control.URL
	t.Cleanup(func() { f.closeEvents(); f.control.Close() })
	writeTargetJSON(t, filepath.Join(root, "ca.pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.control.Certificate().Raw}), 0600)
	identity := f.identity(t)
	_, source, err := installsource.Current()
	if err != nil {
		t.Fatal(err)
	}
	pin := filepath.Join(layout.ReleasesRoot, "pins", source.SHA256, "pb")
	if err := os.MkdirAll(filepath.Dir(pin), 0755); err != nil {
		t.Fatal(err)
	}
	executable, _ := os.Executable()
	input, err := os.Open(executable)
	if err != nil {
		t.Fatal(err)
	}
	output, err := os.OpenFile(pin, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0755)
	if err != nil {
		t.Fatal(err)
	}
	_, copyErr := io.Copy(output, input)
	input.Close()
	if err := errors.Join(copyErr, output.Close()); err != nil {
		t.Fatal(err)
	}
	shell, _ := filepath.EvalSymlinks("/bin/sh")
	catalog := sessionowner.Catalog{Root: filepath.Join(layout.InstallRoot, "session-owners"), ConfigRoot: layout.InstallRoot, User: "root", Group: "root", Maximum: 257}
	d := sessionowner.Descriptor{Schema: sessionowner.Schema, Generation: source.SHA256[:32], Instance: layout.Instance, Source: source, Executable: pin, RuntimeRoot: root, InitialStore: true, Endpoint: filepath.Join(root, "owner.sock"), UID: 0, GID: 0, Workspace: root, Shell: shell}
	if err := catalog.Publish(t.Context(), d); err != nil {
		t.Fatal(err)
	}
	environment := map[string]string{"PAPERBOAT_RUNTIME_STATE_ROOT": root, "PAPERBOAT_RUNTIME_LISTEN_ADDRESS": "127.0.0.1:1", "PAPERBOAT_MACHINE_ID": "machine_target", "PAPERBOAT_CONTROL_URL": f.issuer, "PAPERBOAT_CONTROL_CA_FILE": filepath.Join(root, "ca.pem"), "PAPERBOAT_SESSION_OWNER_CATALOG": catalog.Root, "PAPERBOAT_WORKSPACE_ROOT": root, "PAPERBOAT_SHELL": shell}
	cfg := hostinstall.CarrierConfig{Schema: "paperboat.native-carrier/v1", Instance: layout.Instance, Source: source, Executable: pin, UID: 0, GID: 0, User: "root", Group: "root", RuntimeRoot: root, Environment: environment, WorkerAddress: environment["PAPERBOAT_RUNTIME_LISTEN_ADDRESS"], HostdEndpoint: layout.HostdSocket, ControlEndpoint: layout.HostdSocket + ".carrier", HostdTokenFile: filepath.Join("/var/lib/paperboat/users", layout.Instance, "hostd.token"), DispatchCapabilityFile: filepath.Join(layout.InstallRoot, "carrier", "dispatch.cap"), LockPath: filepath.Join(layout.InstallRoot, "carrier", "owner.lock")}
	if runtime.GOOS == "darwin" {
		cfg.HostdTokenFile = filepath.Join("/Library/Application Support/Paperboat/users", layout.Instance, "hostd.token")
	}
	writeTargetJSON(t, filepath.Join(layout.InstallRoot, "carrier", "config.json"), cfg, 0640)
	f.prepareAuthority(t, identity)
	ctx, cancel := context.WithCancel(t.Context())
	f.owner, err = NewIndependentSessionOwner(ctx, d, catalog.Root)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- f.owner.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-done
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if err := f.owner.Close(cleanup); err != nil {
			t.Error("target owner cleanup failed", err)
		}
	})
	if err := <-f.owner.Ready(); err != nil {
		t.Fatal(err)
	}
	f.client, err = sessionowner.Connect(t.Context(), d)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.client.Close)
	return f
}

func targetCarrierRoot(t *testing.T) bool {
	t.Helper()
	const child = "PAPERBOAT_TEST_TARGET_CARRIER_ROOT"
	if os.Getenv(child) != "1" {
		ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, "sudo", "-n", "env", child+"=1", os.Args[0], "-test.run=^"+t.Name()+"$", "-test.timeout=75s")
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("privileged target fixture failed: %s", output)
		}
		return false
	}
	return true
}

func (f *targetCarrierFixture) closeEvents() { f.eventsOnce.Do(func() { close(f.events) }) }

func TestTargetCarrierRenewsAuthorityWithoutReplacingLiveConnection(t *testing.T) {
	if !targetCarrierRoot(t) {
		return
	}
	f := newTargetCarrierFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	ready, err := f.client.TerminalCarrier(ctx, workloadbridge.CarrierRequest{Operation: "start", Slot: 1})
	if err != nil {
		t.Fatal(err)
	}
	offer, err := native.ParseCarrierOffer(ready.Offer)
	if err != nil {
		t.Fatal(err)
	}
	connection := f.dial(t, ctx, offer)
	defer connection.Close()
	snapshot := session.Snapshot{ID: "ses_target", Generation: 1}
	f.admit(t, ctx, connection, snapshot)
	authority := f.owner.carrier.authority
	verified := authority.LastVerifiedAt()
	changed := authority.ScopeChanges()
	f.mu.Lock()
	f.network.Generation++
	f.network.IssuedAt = time.Now().Unix()
	f.network.ExpiresAt = f.network.IssuedAt + 300
	for i := range f.network.Peers[0].Scopes {
		f.network.Peers[0].Scopes[i].ExpiresAt = f.network.ExpiresAt
	}
	f.eventRevision++
	close(f.networkChanged)
	f.networkChanged = make(chan struct{})
	f.mu.Unlock()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for !authority.LastVerifiedAt().After(verified) {
		select {
		case <-changed:
			changed = authority.ScopeChanges()
		case <-deadline.C:
			t.Fatal("machine event did not renew the delegated carrier's signed authority")
		}
	}

	f.admit(t, ctx, connection, snapshot)
}

func (f *targetCarrierFixture) assertDisconnected(t *testing.T, stream net.Conn, timeout time.Duration) {
	t.Helper()
	stream.SetReadDeadline(time.Now().Add(timeout))
	var b [1]byte
	_, err := stream.Read(b[:])
	var deadline net.Error
	if err == nil || errors.As(err, &deadline) && deadline.Timeout() {
		t.Fatal("generation did not close its accepted terminal", err)
	}
}

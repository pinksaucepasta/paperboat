package tunnel

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/auth"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/filetransfer"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/health"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/nativesession"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/operation"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/process"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/pty"
	hostruntime "github.com/pinksaucepasta/paperboat/internal/hostruntime/runtime"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/server"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/session"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workloadbridge"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/native"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/peerquic"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/streamauth"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
	"github.com/pinksaucepasta/paperboat/internal/resolver"
	"github.com/pinksaucepasta/paperboat/internal/shorttemp"
)

func TestTerminalMovesAcrossRealCarriersWithoutReplayingInputOrOutput(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("requires a Unix PTY")
	}
	connectedProductionRuntime(t, "terminal_move")
}

type connectedTerminalMove struct {
	ownerSocket     string
	ownerToken      []byte
	oldRemote       *workloadbridge.Client
	sessions        *session.Manager
	authorizer      server.AuthorizerFactory
	journal         *operation.Journal
	root            string
	token           string
	target          *resolver.TerminalTarget
	oldServer       *server.Server
	oldApps         *nativesession.Service
	deliveredCursor atomic.Int64
}

func newConnectedTerminalMove(t *testing.T, ctx context.Context, issuer, machine, client string, public ed25519.PublicKey, private ed25519.PrivateKey) *connectedTerminalMove {
	t.Helper()
	root := shorttemp.Dir(t)
	adapter, err := pty.NewAdapter(root)
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := session.NewManager(session.ManagerConfig{Launch: func(command pty.Command) (session.PTYProcess, error) { return adapter.Start(command) }, MaxSessions: 4, MaxAttachments: 4})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		sessions.Shutdown(cleanup)
	})
	snapshot, err := sessions.Create(ctx, session.CreateRequest{ID: "ses_carrier_move", Name: canonicalSessionName("ses_carrier_move"), Command: pty.Command{Path: "/bin/sh", Args: []string{"-c", "stty -echo -icanon -opost; printf _READY000; exec cat"}, CWD: root, Dimensions: pty.Dimensions{Columns: 80, Rows: 24}}})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	factory, err := hostruntime.NewStaticAuthorizer(hostruntime.StaticAuthConfig{Issuer: issuer, EnvironmentID: "env_connected", MachineID: machine, HelperID: "hlp_connected", Keys: map[string]ed25519.PublicKey{"connected_key": public}, Clock: nativeRuntimeClock{}})
	if err != nil {
		t.Fatal(err)
	}
	claims := auth.Claims{Issuer: issuer, Audience: "paperboat-machine", Subject: "account_connected", JTI: "jti_carrier", IssuedAt: now.Add(-time.Minute).Unix(), ExpiresAt: now.Add(5 * time.Minute).Unix(), Scope: []string{"terminal:operate"}, CredentialClass: "terminal_operation", EnvironmentID: "env_connected", AccountID: "account_connected", MachineID: machine, UserID: "account_connected", CLIClientSessionID: client, SourceMachineID: "machine_controller", SessionID: snapshot.ID, ExpectedGeneration: int64(snapshot.Generation), AssignmentID: "access_connected"}
	token := connectedSignedJWT(t, private, "paperboat-credential+jwt", claims)
	journal, err := operation.NewJournal(1024)
	if err != nil {
		t.Fatal(err)
	}
	fixture := &connectedTerminalMove{sessions: sessions, authorizer: factory, journal: journal, root: root, token: token}
	fixture.ownerSocket = filepath.Join(root, "owner.sock")
	fixture.ownerToken = make([]byte, 32)
	if _, err := rand.Read(fixture.ownerToken); err != nil {
		t.Fatal(err)
	}
	ownerCtx, stopOwner := context.WithCancel(context.Background())
	owner := &workloadbridge.Server{Endpoint: fixture.ownerSocket, Token: fixture.ownerToken, Sessions: sessions, SessionOnly: true, Ready: make(chan error, 1), MaximumConcurrent: 32, Fence: func() hostdproto.Status {
		return hostdproto.Status{State: hostdproto.StateActive, WorkerID: "session-owner", Epoch: 1, APIVersion: 1}
	}, AcquireActive: func(worker string, epoch uint64) (func(), error) {
		if worker != "session-owner" || epoch != 1 {
			return nil, hostdproto.ErrFenced
		}
		return func() {}, nil
	}}
	ownerDone := make(chan error, 1)
	go func() { ownerDone <- owner.Run(ownerCtx) }()
	t.Cleanup(func() {
		stopOwner()
		if err := <-ownerDone; err != nil {
			t.Error("independent owner stopped incorrectly", err)
		}
	})
	if err := <-owner.Ready; err != nil {
		t.Fatal(err)
	}
	fixture.target = &resolver.TerminalTarget{EnvironmentID: "env_connected", SessionID: snapshot.ID, CWD: root, AfterSequence: 1, Auth: resolver.AuthTarget{Method: "bearer", Token: token, ExpiresAt: time.Unix(claims.ExpiresAt, 0).Format(time.RFC3339), ResourceID: "access_connected"}}
	fixture.target.SequenceSink = func(value int) { fixture.deliveredCursor.Store(int64(value)) }
	fixture.oldServer, fixture.oldApps = fixture.endpoint(t)
	return fixture
}

func (f *connectedTerminalMove) endpoint(t *testing.T) (*server.Server, *nativesession.Service) {
	t.Helper()
	shell, err := filepath.EvalSymlinks("/bin/sh")
	if err != nil {
		t.Fatal(err)
	}
	client, err := workloadbridge.NewClient(f.ownerSocket, f.ownerToken, "session-owner", 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	if f.oldRemote == nil {
		f.oldRemote = client
	}
	sessions, err := client.Sessions()
	if err != nil {
		t.Fatal(err)
	}
	launcher, err := process.NewShellLauncher(shell, nil, sessions)
	if err != nil {
		t.Fatal(err)
	}
	status := health.New("carrier-test", []string{"terminal.v1", "health.v1"}, nil)
	status.Set("terminal.v1", health.Ready, "", 0)
	dispatcher, err := server.NewDispatcher(server.DispatcherConfig{Sessions: sessions, Health: status, SessionLauncher: launcher, WorkspaceRoot: f.root, Random: rand.Reader})
	if err != nil {
		t.Fatal(err)
	}
	available, err := protocol.AvailableCapabilities(dispatcher)
	if err != nil {
		t.Fatal(err)
	}
	endpoint, err := server.New(server.Config{Negotiator: protocol.Negotiator{Available: available}, Journal: f.journal, Handler: dispatcher, MaxConcurrent: 16, HeartbeatInterval: 5 * time.Second, MutationDeadline: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		endpoint.Shutdown(ctx)
	})
	limiter, err := server.NewConnectionLimiter(16)
	if err != nil {
		t.Fatal(err)
	}
	associations, err := server.NewNativeAssociationManager(server.NativeAssociationConfig{Server: endpoint, Authorizer: f.authorizer, Limiter: limiter})
	if err != nil {
		t.Fatal(err)
	}
	authorize := server.CredentialStreamAuthorizer(f.authorizer)
	serve := func(_ context.Context, _ streamauth.Header, connection net.Conn) error {
		return associations.Serve(connection)
	}
	apps, err := nativesession.New(nativesession.Config{Authorize: func(ctx context.Context, header streamauth.Header) (string, error) {
		value, err := authorize(ctx, header)
		return value.ResourceID, err
	}, ServeStream: serve, ServeAuthorizedTransfer: serve})
	if err != nil {
		t.Fatal(err)
	}
	return endpoint, apps
}

func (f *connectedTerminalMove) serve(ctx context.Context, s *native.Session) error {
	return f.oldApps.Serve(ctx, s)
}

func (f *connectedTerminalMove) exercise(t *testing.T, ctx context.Context, runtime *cliNativeRuntime, old *native.Owner, authority *tailnet.Authority, store config.ProfileStore, leaf tls.Certificate, public ed25519.PublicKey) {
	t.Helper()
	app := peerApplication{helper: func(ctx context.Context, message helperMessageConnection, target *resolver.TerminalTarget) (Conn, error) {
		return newInitializedHelperTerminalConn(ctx, message, target, 4)
	}}
	conn, err := runtime.openApplication(ctx, "machine_connected", "terminal", app, f.target, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	carrier := conn.(*carrierTerminal)
	for {
		carrier.outputMu.Lock()
		queued := carrier.produced >= 8
		carrier.outputMu.Unlock()
		if queued {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("initial native output was not queued")
		case <-time.After(time.Millisecond):
		}
	}
	if f.deliveredCursor.Load() != 0 {
		t.Fatal("queued output was acknowledged before the local reader received it")
	}
	ready := make([]byte, 8)
	if _, err := io.ReadFull(conn, ready[:4]); err != nil {
		t.Fatal(err)
	}
	if f.deliveredCursor.Load() != 5 {
		t.Fatal("partial local read did not retain its exact replay cursor")
	}
	if _, err := io.ReadFull(conn, ready[4:]); err != nil || !bytes.Equal(ready, []byte("READY000")) {
		t.Fatal("initial PTY output unavailable", err)
	}
	if f.deliveredCursor.Load() != 9 {
		t.Fatal("complete local read cursor mismatch")
	}
	if err := conn.Resize(31, 91); err != nil {
		t.Fatal(err)
	}
	// Geometry changes are canonical history too; consume their actual VT
	// notification before comparing the input workload byte-for-byte.
	resize := []byte("\x1b[8;31;91t")
	resized := make([]byte, len(resize))
	if _, err := io.ReadFull(conn, resized); err != nil || !bytes.Equal(resized, resize) {
		t.Fatal("resize history notification mismatch", err)
	}

	offer := f.parallelCarrier(t, ctx, authority, store, leaf, public)
	var expected []byte
	for i := 0; i < 64; i++ {
		expected = append(expected, []byte(fmt.Sprintf("%04d:%s\n", i, bytes.Repeat([]byte("x"), 250)))...)
	}
	written := make(chan error, 1)
	go func() {
		for i := 0; i < len(expected); i += 256 {
			if _, err := conn.Write(expected[i : i+256]); err != nil {
				written <- err
				return
			}
		}
		written <- nil
	}()
	received := make(chan []byte, 1)
	go func() {
		data := make([]byte, len(expected))
		if _, err := io.ReadFull(conn, data); err != nil {
			received <- nil
			return
		}
		received <- data
	}()
	if err := old.OfferCarrier(ctx, offer); err != nil {
		t.Fatal("planned terminal move failed", err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.oldServer.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	f.oldRemote.Close()
	if err := <-written; err != nil {
		t.Fatal("input failed across carrier replacement", err)
	}
	if data := <-received; !bytes.Equal(data, expected) {
		first := 0
		for first < min(len(data), len(expected)) && data[first] == expected[first] {
			first++
		}
		t.Fatalf("terminal output mismatch: first_offset=%d received_bytes=%d expected_bytes=%d", first, len(data), len(expected))
	}
	tail := []byte("FINAL001")
	if _, err := conn.Write(tail); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(tail))
	if _, err := io.ReadFull(conn, got); err != nil || !bytes.Equal(got, tail) {
		t.Fatal("input was replayed or terminal stopped after old host shutdown", err)
	}
	f.checkWriterOwner(t)
	snapshot, err := f.sessions.Snapshot(f.target.SessionID)
	if err != nil || snapshot.Generation != 1 || snapshot.Dimensions.Rows != 31 || snapshot.Dimensions.Columns != 91 {
		t.Fatal("PTY generation or window changed during carrier move", err)
	}
}

func (f *connectedTerminalMove) parallelCarrier(t *testing.T, ctx context.Context, authority *tailnet.Authority, store config.ProfileStore, leaf tls.Certificate, public ed25519.PublicKey) native.CarrierOffer {
	t.Helper()
	self, err := authority.Self()
	if err != nil {
		t.Fatal(err)
	}
	nextAuthority, err := tailnet.NewAuthority(tailnet.AuthorityOptions{Store: store, Issuer: "https://api.example.test", Self: self, Keys: connectedNetworkKeys{public}})
	if err != nil {
		t.Fatal(err)
	}
	if err := nextAuthority.Restore(ctx); err != nil {
		t.Fatal(err)
	}
	nextOwner, err := native.NewOwner(native.Config{Authority: nextAuthority, TLS: &tls.Config{MinVersion: tls.VersionTLS13, InsecureSkipVerify: true, Certificates: []tls.Certificate{leaf}, NextProtos: []string{peerquic.ALPN}}}) //nolint:gosec
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { nextOwner.Close() })
	socket, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	identity, err := nextOwner.NewCarrierIdentity([]netip.AddrPort{socket.LocalAddr().(*net.UDPAddr).AddrPort()})
	if err != nil {
		socket.Close()
		t.Fatal(err)
	}
	_, apps := f.endpoint(t)
	nextCtx, stopNext := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- nextOwner.ListenCarrier(nextCtx, socket, identity, apps.Serve) }()
	t.Cleanup(func() { stopNext(); socket.Close(); <-done; apps.Wait() })
	return identity.Offer
}

func (f *connectedTerminalMove) checkWriterOwner(t *testing.T) {
	t.Helper()
	// A fresh worker connection has none of the old carrier's process-local
	// registry. File delivery must still find the exact current writer.
	client, err := workloadbridge.NewClient(f.ownerSocket, f.ownerToken, "session-owner", 1)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	remote, err := client.Sessions()
	if err != nil {
		t.Fatal(err)
	}
	writers, err := remote.ActiveWriters(f.target.SessionID)
	if err != nil || len(writers) != 1 || writers[0].MachineID != "machine_controller" || writers[0].ClientID != "cli_connected" || writers[0].LastInput.IsZero() {
		t.Fatal("writer authority or activity was lost across carrier/worker replacement", err)
	}
	if recipient, err := filetransfer.WriterRecipient(writers, "machine_controller"); err != nil || recipient != "cli_connected" {
		t.Fatal("replacement worker cannot resolve the exact file recipient", err)
	}

}

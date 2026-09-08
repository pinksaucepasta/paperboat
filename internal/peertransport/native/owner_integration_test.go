package native_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/config"
	clienttransfer "github.com/pinksaucepasta/paperboat/internal/filetransfer"
	hostconfig "github.com/pinksaucepasta/paperboat/internal/hostruntime/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/execprocess"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/health"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/nativesession"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/operation"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/process"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/pty"
	hostserver "github.com/pinksaucepasta/paperboat/internal/hostruntime/server"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/session"
	"github.com/pinksaucepasta/paperboat/internal/nativeprivate"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/native"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/peerquic"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/streamauth"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
	"github.com/pinksaucepasta/paperboat/internal/resolver"
	"github.com/pinksaucepasta/paperboat/internal/tunnel"
	"github.com/tailscale/tailcat"
	"go.uber.org/goleak"
	"tailscale.com/tailcfg"
	"tailscale.com/tstest/integration"
	"tailscale.com/types/nettype"
)

type testKeys map[string]ed25519.PublicKey

func (k testKeys) Lookup(context.Context, string) (ed25519.PublicKey, bool, error) {
	p, ok := k["native_test"]
	return p, ok, nil
}

type testSecrets map[string]string

func (s testSecrets) Get(key string) (string, error) {
	value, ok := s[key]
	if !ok {
		return "", config.ErrSecretNotFound
	}
	return value, nil
}
func (s testSecrets) Set(key, value string) error { s[key] = value; return nil }
func (s testSecrets) Delete(key string) error     { delete(s, key); return nil }

func TestAuthorizedTerminalAndLiveTransferRevocationReconnect(t *testing.T) {
	leaks := goleak.IgnoreCurrent()
	t.Cleanup(func() { goleak.VerifyNone(t, leaks) })
	t.Setenv("IN_TS_TEST", "true")
	dm := integration.RunDERPAndSTUN(t, func(string, ...any) {}, "127.0.0.1")
	signerPublic, signerPrivate, _ := ed25519.GenerateKey(rand.Reader)
	clientTLS, clientFingerprint := testTLS(t, "cli")
	serverTLS, serverFingerprint := testTLS(t, "machine")
	clientBinding := tailnet.NetworkBinding{AccountID: "account_test", EndpointID: "cli_test", Role: "cli", EndpointGeneration: 1, QUICCertificateFingerprint: clientFingerprint, VirtualAddress: "fd7a:115c:a1e0::1"}
	serverBinding := tailnet.NetworkBinding{AccountID: "account_test", EndpointID: "machine_test", Role: "machine", MachineID: "machine_test", EndpointGeneration: 1, MachineGeneration: 1, QUICCertificateFingerprint: serverFingerprint, VirtualAddress: "fd7a:115c:a1e0::2"}
	clientAuthority := testAuthority(t, clientBinding, testKeys{"native_test": signerPublic})
	serverAuthority := testAuthority(t, serverBinding, testKeys{"native_test": signerPublic})
	clientBinding.WireGuardPublicKey, clientBinding.KeyGeneration = prepareBinding(t, clientAuthority)
	serverBinding.WireGuardPublicKey, serverBinding.KeyGeneration = prepareBinding(t, serverAuthority)
	now := time.Now().Unix()
	clientConfig := testConfiguration(now, 1, clientBinding, serverBinding, "dial")
	serverConfig := testConfiguration(now, 1, serverBinding, clientBinding, "accept")
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

	descriptor := startTestServer(t, serverOwner, serverAuthority, dm.Regions[1])
	session, err := clientOwner.Dial(t.Context(), descriptor, "machine_test", peerquic.ClassInteractive)
	if err != nil {
		t.Fatal(err)
	}
	for _, consumer := range []string{"terminal", "file_transfer", "private_tcp"} {
		header, headerErr := streamauth.New("operation_"+consumer, consumer, "stream_"+consumer, "credential_"+consumer, time.Now().Add(time.Minute), 1<<20)
		if consumer == "private_tcp" {
			target, _ := json.Marshal(nativeprivate.Binding{Schema: nativeprivate.SchemaV1, ResourceKind: "tunnel", ResourceID: "tun_native", ResourceGeneration: 1, RouteID: "route_native", RouteGeneration: 1, TargetGeneration: 1, OwnerEndpointID: "machine_test", Protocol: "tcp", TargetScheme: "tcp", TargetAddress: "127.0.0.1:22", ExpiresAt: time.Now().Add(time.Minute)})
			header, headerErr = streamauth.NewNativePrivate("operation_"+consumer, consumer, "stream_"+consumer, "credential_"+consumer, time.Now().Add(time.Minute), 1<<20, target)
		}
		if headerErr != nil {
			t.Fatal(headerErr)
		}
		stream, openErr := session.OpenAuthorized(t.Context(), header, "grant_test", capability(consumer))
		if openErr != nil {
			t.Fatal(openErr)
		}
		payload := []byte("real-" + consumer + "-protocol-bytes")
		if _, openErr = stream.Write(payload); openErr != nil {
			t.Fatal(openErr)
		}
		response := make([]byte, len(payload))
		if _, openErr = io.ReadFull(stream, response); openErr != nil || string(response) != string(payload) {
			t.Fatalf("%s response=%q err=%v", consumer, response, openErr)
		}
		_ = stream.Close()
	}
	sshTunnel := tunnel.TailnetTerminalTunnel{DialSession: func(context.Context, resolver.ConnectInfo) (*native.Session, error) {
		return session, nil
	}}
	sshExpires := time.Now().Add(time.Minute).UTC().Truncate(time.Second)
	sshConn, err := sshTunnel.DialSSH(t.Context(), resolver.ConnectInfo{Terminal: &resolver.TerminalTarget{Auth: resolver.AuthTarget{Token: "credential_ssh", ExpiresAt: sshExpires.Format(time.RFC3339), ResourceID: "grant_test"}}}, "operation_native_ssh")
	if err != nil {
		t.Fatal(err)
	}
	sshPayload := []byte("opaque-openssh-transport-bytes")
	if _, err = sshConn.Write(sshPayload); err != nil {
		t.Fatal(err)
	}
	sshResponse := make([]byte, len(sshPayload))
	if _, err = io.ReadFull(sshConn, sshResponse); err != nil || string(sshResponse) != string(sshPayload) {
		t.Fatalf("SSH response=%q err=%v", sshResponse, err)
	}
	if err = sshConn.(tunnel.InputHalfCloser).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	_ = sshConn.Close()
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := session.Open(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled open: %v", err)
	}

	clientConfig.Generation++
	clientConfig.Peers = nil
	serverConfig.Generation++
	serverConfig.Peers = nil
	applyTestConfiguration(t, clientAuthority, signerPrivate, clientConfig)
	applyTestConfiguration(t, serverAuthority, signerPrivate, serverConfig)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		probeCtx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		probe, probeErr := session.Open(probeCtx)
		cancel()
		if probeErr != nil {
			break
		}
		_ = probe.Close()
		time.Sleep(10 * time.Millisecond)
	}
	probeCtx, cancelProbe := context.WithTimeout(t.Context(), 50*time.Millisecond)
	probe, probeErr := session.Open(probeCtx)
	cancelProbe()
	if probeErr == nil {
		_ = probe.Close()
		t.Fatal("active QUIC session survived signed peer removal")
	}

	clientConfig.Generation++
	clientConfig.Peers = testPeers(serverBinding, "dial", clientConfig.ExpiresAt)
	serverConfig.Generation++
	serverConfig.Peers = testPeers(clientBinding, "accept", serverConfig.ExpiresAt)
	applyTestConfiguration(t, clientAuthority, signerPrivate, clientConfig)
	applyTestConfiguration(t, serverAuthority, signerPrivate, serverConfig)
	descriptor = startTestServer(t, serverOwner, serverAuthority, dm.Regions[1])
	reconnected, err := clientOwner.Dial(t.Context(), descriptor, "machine_test", peerquic.ClassInteractive)
	if err != nil {
		t.Fatal(err)
	}
	header, _ := streamauth.New("operation_reconnect", "terminal", "stream_reconnect", "credential_terminal", time.Now().Add(time.Minute), 1<<20)
	stream, err := reconnected.OpenAuthorized(t.Context(), header, "grant_test", "terminal")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = stream.Write([]byte("reconnected")); err != nil {
		t.Fatal(err)
	}
	response := make([]byte, len("reconnected"))
	if _, err = io.ReadFull(stream, response); err != nil || string(response) != "reconnected" {
		t.Fatalf("reconnect response=%q err=%v", response, err)
	}
}

type sliceAuthorizer func(context.Context, protocol.Frame) (hostserver.Authorization, error)

func (f sliceAuthorizer) Authorize(ctx context.Context, frame protocol.Frame) (hostserver.Authorization, error) {
	return f(ctx, frame)
}

type sliceLauncher struct {
	sessions *session.Manager
	shell    string
	root     string
}

func (l sliceLauncher) Launch(ctx context.Context, request process.LaunchRequest) (session.Snapshot, error) {
	return l.sessions.Create(ctx, session.CreateRequest{ID: request.ID, Name: request.Name, Command: pty.Command{Path: l.shell, Args: []string{"-c", "printf ready; read first; printf 'paperboat:%s\\n' \"$first\"; read second; printf 'resumed:%s\\n' \"$second\"; exit 7"}, Env: []string{"HOME=" + l.root, "PATH=/usr/bin:/bin", "TERM=xterm"}, CWD: request.CWD, Dimensions: request.Dimensions}})
}

func TestRealTerminalAndFileProtocolsOverNativeTailnet(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("requires /bin/sh")
	}
	t.Setenv("IN_TS_TEST", "true")
	dm := integration.RunDERPAndSTUN(t, func(string, ...any) {}, "127.0.0.1")
	signerPublic, signerPrivate, _ := ed25519.GenerateKey(rand.Reader)
	clientTLS, clientFingerprint := testTLS(t, "cli-slice")
	serverTLS, serverFingerprint := testTLS(t, "machine-slice")
	clientBinding := tailnet.NetworkBinding{AccountID: "account_slice", EndpointID: "cli_slice", Role: "cli", EndpointGeneration: 1, QUICCertificateFingerprint: clientFingerprint, VirtualAddress: "fd7a:115c:a1e0::11"}
	serverBinding := tailnet.NetworkBinding{AccountID: "account_slice", EndpointID: "machine_slice", Role: "machine", MachineID: "machine_slice", EndpointGeneration: 1, MachineGeneration: 1, QUICCertificateFingerprint: serverFingerprint, VirtualAddress: "fd7a:115c:a1e0::12"}
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
	_ = runRealTerminalAndFileProtocols(t, clientOwner, serverOwner, serverAuthority, dm.Regions[1], nil)
}

func runRealTerminalAndFileProtocols(t *testing.T, clientOwner, serverOwner *native.Owner, serverAuthority *tailnet.Authority, region *tailcfg.DERPRegion, beforeProtocols func(tailcat.Addr), afterProtocol ...func(string)) func(context.Context) error {
	t.Helper()
	root := t.TempDir()
	adapter, err := pty.NewAdapter(root)
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := session.NewManager(session.ManagerConfig{Launch: func(command pty.Command) (session.PTYProcess, error) { return adapter.Start(command) }, MaxSessions: 2, MaxAttachments: 2, MaxInputDecisions: 32})
	if err != nil {
		t.Fatal(err)
	}
	executions, err := execprocess.New(execprocess.Config{WorkspaceRoot: root, BaseEnvironment: []string{"HOME=" + root, "PATH=/usr/bin:/bin", "LANG=C"}, MaximumActive: 2, MaximumOperations: 8, ReplayBytes: 64 << 10})
	if err != nil {
		t.Fatal(err)
	}
	readiness := health.New("native-slice", []string{"terminal.v1", "health.v1", "exec.v1"}, nil)
	readiness.Set("terminal.v1", health.Ready, "", 0)
	readiness.Set("health.v1", health.Ready, "", 0)
	readiness.Set("exec.v1", health.Ready, "", 0)
	dispatcher, err := hostserver.NewDispatcher(hostserver.DispatcherConfig{Sessions: sessions, Health: readiness, SessionLauncher: sliceLauncher{sessions: sessions, shell: "/bin/sh", root: root}, WorkspaceRoot: root, Random: rand.Reader, Exec: executions})
	if err != nil {
		t.Fatal(err)
	}
	journal, _ := operation.NewJournal(32)
	protocolServer, err := hostserver.New(hostserver.Config{Negotiator: protocol.Negotiator{Profile: hostconfig.BYOD, Available: map[string]bool{"terminal.v1": true, "health.v1": true, "exec.v1": true}}, Journal: journal, Handler: dispatcher, MaxConcurrent: 4, HeartbeatInterval: time.Hour, MutationDeadline: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if err := protocolServer.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = protocolServer.Shutdown(ctx)
		_ = sessions.Shutdown(ctx)
	})
	limiter, _ := hostserver.NewConnectionLimiter(4)
	association, err := hostserver.NewNativeAssociationManager(hostserver.NativeAssociationConfig{Server: protocolServer, Authorizer: func(token string) (hostserver.Authorizer, error) {
		if token != "terminal-token" && token != "exec-token" {
			return nil, errors.New("credential rejected")
		}
		return sliceAuthorizer(func(context.Context, protocol.Frame) (hostserver.Authorization, error) {
			return hostserver.Authorization{JournalBinding: "slice", EnvironmentID: "env_slice", MachineID: "machine_slice", UserID: "account_slice", ClientID: "cli_slice", ResourceID: "grant_test"}, nil
		}), nil
	}, Limiter: limiter})
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("live-transfer-payload")
	digest := sha256.Sum256(payload)
	transferHandler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer file-token" || request.Method != http.MethodGet || !strings.HasSuffix(request.URL.Path, "/content") {
			http.Error(writer, "denied", http.StatusUnauthorized)
			return
		}
		writer.Header().Set("ETag", `"sha256:`+hex.EncodeToString(digest[:])+`"`)
		_, _ = writer.Write(payload)
	})
	service, err := nativesession.New(nativesession.Config{Authorize: func(_ context.Context, header streamauth.Header) (string, error) {
		if header.Credential != "terminal-token" && header.Credential != "file-token" && header.Credential != "exec-token" {
			return "", errors.New("credential rejected")
		}
		return "grant_test", nil
	}, ServeStream: func(ctx context.Context, header streamauth.Header, connection net.Conn) error {
		return association.Serve(connection)
	}, ServeTransfer: func(ctx context.Context, connection net.Conn) error {
		return hostserver.ServeHTTPConnection(ctx, connection, transferHandler)
	}})
	if err != nil {
		t.Fatal(err)
	}
	serverUDP, err := serverAuthority.Listen(region)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = serverOwner.Listen(t.Context(), region, service.Serve) }()
	descriptor := serverUDP.Address()
	if beforeProtocols != nil {
		beforeProtocols(descriptor)
	}
	dialSession := func(ctx context.Context, _ resolver.ConnectInfo) (*native.Session, error) {
		return clientOwner.Dial(ctx, descriptor, "machine_slice", peerquic.ClassInteractive)
	}
	expires := time.Now().Add(time.Minute).UTC().Truncate(time.Second)
	terminalTunnel := tunnel.TailnetTerminalTunnel{DialSession: dialSession}
	var firstSequence atomic.Int64
	terminalConn, err := terminalTunnel.Dial(t.Context(), resolver.ConnectInfo{Terminal: &resolver.TerminalTarget{EnvironmentID: "env_slice", SessionID: "session_slice", CWD: root, Cols: 80, Rows: 24, RestartIfNotRunning: true, ReplayHistory: true, InputAttachmentID: "attachment_slice", SequenceSink: func(sequence int) { firstSequence.Store(int64(sequence)) }, Auth: resolver.AuthTarget{Method: "bearer", Token: "terminal-token", ExpiresAt: expires.Format(time.RFC3339), ResourceID: "grant_test"}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := readTerminalUntil(t, terminalConn, "ready"); !strings.Contains(got, "ready") {
		t.Fatalf("initial terminal output=%q", got)
	}
	secondConn, err := terminalTunnel.Dial(t.Context(), resolver.ConnectInfo{Terminal: &resolver.TerminalTarget{EnvironmentID: "env_slice", SessionID: "session_slice", CWD: root, Cols: 132, Rows: 43, ReplayHistory: true, InputAttachmentID: "attachment_second", Auth: resolver.AuthTarget{Method: "bearer", Token: "terminal-token", ExpiresAt: expires.Format(time.RFC3339), ResourceID: "grant_test"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer secondConn.Close()
	if err := secondConn.Resize(43, 132); err != nil {
		t.Fatal(err)
	}
	resizeDeadline := time.Now().Add(time.Second)
	for {
		snapshot, snapshotErr := sessions.Snapshot("session_slice")
		if snapshotErr == nil && snapshot.Dimensions.Columns == 132 && snapshot.Dimensions.Rows == 43 {
			break
		}
		if time.Now().After(resizeDeadline) {
			t.Fatalf("second attachment dimensions=%+v err=%v", snapshot.Dimensions, snapshotErr)
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := terminalConn.Write([]byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	for label, connection := range map[string]tunnel.Conn{"first": terminalConn, "second": secondConn} {
		if got := readTerminalUntil(t, connection, "paperboat:hello"); !strings.Contains(got, "paperboat:hello") {
			t.Fatalf("%s attachment output=%q", label, got)
		}
	}
	resumeFrom := firstSequence.Load()
	if err := terminalConn.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := secondConn.Write([]byte("again\n")); err != nil {
		t.Fatal(err)
	}
	if got := readTerminalUntil(t, secondConn, "resumed:again"); !strings.Contains(got, "resumed:again") {
		t.Fatalf("second attachment resumed output=%q", got)
	}
	resumedConn, err := terminalTunnel.Dial(t.Context(), resolver.ConnectInfo{Terminal: &resolver.TerminalTarget{EnvironmentID: "env_slice", SessionID: "session_slice", CWD: root, Cols: 100, Rows: 31, ReplayHistory: false, AfterSequence: int(resumeFrom), InputAttachmentID: "attachment_slice", Auth: resolver.AuthTarget{Method: "bearer", Token: "terminal-token", ExpiresAt: expires.Format(time.RFC3339), ResourceID: "grant_test"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer resumedConn.Close()
	if got := readTerminalUntil(t, resumedConn, "resumed:again"); !strings.Contains(got, "resumed:again") {
		t.Fatalf("resumed attachment replay=%q", got)
	}
	for label, connection := range map[string]tunnel.Conn{"resumed": resumedConn, "second": secondConn} {
		if code, waitErr := connection.Wait(); waitErr != nil || code != 7 {
			t.Fatalf("%s terminal exit=%d err=%v", label, code, waitErr)
		}
	}

	for _, check := range afterProtocol {
		check("terminal")
	}

	execConn, err := terminalTunnel.DialExec(t.Context(), resolver.ConnectInfo{Terminal: &resolver.TerminalTarget{EnvironmentID: "env_slice", CWD: root, Auth: resolver.AuthTarget{Method: "bearer", Token: "exec-token", ExpiresAt: expires.Format(time.RFC3339), ResourceID: "grant_test"}}}, tunnel.ExecRequest{OperationID: "operation_exec_slice", Argv: []string{"/bin/sh", "-c", `printf %s "$EXACT"; printf %s "$PWD" >&2; exit 23`}, CWD: root, Environment: map[string]string{"EXACT": "exact-argv"}})
	if err != nil {
		t.Fatal(err)
	}
	var execStdout, execStderr strings.Builder
	for event := range execConn.Events() {
		switch event.Stream {
		case "stdout":
			execStdout.Write(event.Data)
		case "stderr":
			execStderr.Write(event.Data)
		}
	}
	if code, waitErr := execConn.Wait(); waitErr != nil || code != 23 || execStdout.String() != "exact-argv" || execStderr.String() != root {
		t.Fatalf("exec exit=%d err=%v stdout=%q stderr=%q", code, waitErr, execStdout.String(), execStderr.String())
	}
	_ = execConn.Close()
	for _, check := range afterProtocol {
		check("exec")
	}
	cancelConn, err := terminalTunnel.DialExec(t.Context(), resolver.ConnectInfo{Terminal: &resolver.TerminalTarget{EnvironmentID: "env_slice", CWD: root, Auth: resolver.AuthTarget{Method: "bearer", Token: "exec-token", ExpiresAt: expires.Format(time.RFC3339), ResourceID: "grant_test"}}}, tunnel.ExecRequest{OperationID: "operation_exec_cancel_slice", Argv: []string{"/bin/sh", "-c", "sleep 30"}, CWD: root})
	if err != nil {
		t.Fatal(err)
	}
	started := false
	var cancelCallErr error
	for event := range cancelConn.Events() {
		if event.State == "started" {
			started = true
			cancelCallErr = cancelConn.Cancel()
		}
	}
	cancelCode, cancelErr := cancelConn.Wait()
	var remoteExecError *tunnel.RemoteExecError
	var cancelCallRemote *tunnel.RemoteExecError
	callConfirmed := cancelCallErr == nil || errors.As(cancelCallErr, &cancelCallRemote) && cancelCallRemote.Code == "exec_canceled"
	if !started || !callConfirmed || !errors.As(cancelErr, &remoteExecError) || remoteExecError.Code != "exec_canceled" {
		t.Fatalf("exec cancellation started=%v call_err=%v exit=%d err=%v", started, cancelCallErr, cancelCode, cancelErr)
	}
	_ = cancelConn.Close()
	for _, check := range afterProtocol {
		check("exec cancellation")
	}

	fileSession, err := dialSession(t.Context(), resolver.ConnectInfo{})
	if err != nil {
		t.Fatal(err)
	}
	transport, err := clienttransfer.NewNativeRoundTripper(func(ctx context.Context) (net.Conn, error) {
		header, headerErr := streamauth.New("operation_file_transfer", "file_transfer", "stream_file_transfer", "file-token", expires, 1<<20)
		if headerErr != nil {
			return nil, headerErr
		}
		return fileSession.OpenAuthorized(ctx, header, "grant_test", "file_transfer")
	})
	if err != nil {
		t.Fatal(err)
	}
	fileClient := clienttransfer.NewClient("http://machine.slice/v1/file-transfers", clienttransfer.Auth{Token: "file-token", ExpiresAt: expires}, clienttransfer.Binding{SourceMachineID: "machine_slice", DestinationMachineID: "cli_slice", InitiatingUserID: "account_slice"}, &http.Client{Transport: transport, Timeout: 5 * time.Second})
	manifest := clienttransfer.Manifest{TransferID: "transfer_slice", Size: int64(len(payload)), SHA256: hex.EncodeToString(digest[:])}
	assertTransfer := func(ctx context.Context) error {
		response, contentErr := fileClient.Content(ctx, manifest, 0)
		if contentErr != nil {
			return contentErr
		}
		received, readErr := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if readErr != nil {
			return readErr
		}
		if string(received) != string(payload) {
			return errors.New("file transfer payload differed")
		}
		return nil
	}
	if err := assertTransfer(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, check := range afterProtocol {
		check("file transfer")
	}
	return assertTransfer
}

func readTerminalUntil(t *testing.T, connection tunnel.Conn, marker string) string {
	t.Helper()
	result := make(chan struct {
		value string
		err   error
	}, 1)
	go func() {
		var output strings.Builder
		buffer := make([]byte, 1024)
		for !strings.Contains(output.String(), marker) {
			count, err := connection.Read(buffer)
			if count > 0 {
				output.Write(buffer[:count])
			}
			if err != nil {
				result <- struct {
					value string
					err   error
				}{output.String(), err}
				return
			}
		}
		result <- struct {
			value string
			err   error
		}{output.String(), nil}
	}()
	select {
	case got := <-result:
		if got.err != nil {
			t.Fatalf("read terminal through %q: %v (output %q)", marker, got.err, got.value)
		}
		return got.value
	case <-time.After(5 * time.Second):
		_ = connection.Close()
		t.Fatalf("timed out reading terminal through %q", marker)
		return ""
	}
}

func startTestServer(t *testing.T, owner *native.Owner, authority *tailnet.Authority, region *tailcfg.DERPRegion) tailcat.Addr {
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
				stream, _, acceptErr := session.AcceptAuthorized(serveCtx, func(_ context.Context, header streamauth.Header) (string, error) {
					if header.Credential != "credential_"+header.Consumer && header.Credential != "credential_terminal" {
						return "", errors.New("credential rejected")
					}
					return "grant_test", nil
				})
				if acceptErr != nil {
					return acceptErr
				}
				go func(connection net.Conn) {
					defer connection.Close()
					buffer := make([]byte, 1024)
					count, readErr := connection.Read(buffer)
					if readErr == nil {
						_, _ = connection.Write(buffer[:count])
					}
				}(stream)
			}
		})
	}()
	return server.Address()
}

func capability(consumer string) string {
	if consumer == "file_transfer" {
		return "file_transfer"
	}
	if consumer == "private_tcp" || consumer == "private_http" {
		return "private_access"
	}
	return "terminal"
}

func testAuthority(t *testing.T, self tailnet.NetworkBinding, keys testKeys, listeners ...nettype.PacketListener) *tailnet.Authority {
	t.Helper()
	if public, ok := testQUICKeys.Load(self.QUICCertificateFingerprint); ok {
		self.QUICPublicKey = public.(string)
	}
	var listener nettype.PacketListener
	if len(listeners) > 0 {
		listener = listeners[0]
	}
	options := tailnet.AuthorityOptions{Store: config.ProfileStore{Path: t.TempDir(), Secrets: testSecrets{}}, Issuer: "https://api.example.test", Self: self, Keys: keys, TestOnlyPacketListener: listener}
	if filter, ok := listener.(*directFilter); ok {
		options.TestOnlyDERPCarrier = filter.wrapCarrier
	}
	authority, err := tailnet.NewAuthority(options)
	if err != nil {
		t.Fatal(err)
	}
	return authority
}

func prepareBinding(t *testing.T, authority *tailnet.Authority) (string, uint64) {
	t.Helper()
	public, generation, err := authority.PrepareKey(false)
	if err != nil {
		t.Fatal(err)
	}
	disco, err := authority.DiscoveryPublicKey()
	if err != nil {
		t.Fatal(err)
	}
	testDiscoKeys.Store(public, disco)
	return public, generation + 1
}

var testDiscoKeys sync.Map
var testQUICKeys sync.Map

func bindTestQUICKey(binding tailnet.NetworkBinding) tailnet.NetworkBinding {
	if public, ok := testQUICKeys.Load(binding.QUICCertificateFingerprint); ok {
		binding.QUICPublicKey = public.(string)
	}
	return binding
}

func testConfiguration(now int64, generation uint64, self, peer tailnet.NetworkBinding, direction string) tailnet.NetworkConfiguration {
	self = bindTestQUICKey(self)
	peer = bindTestQUICKey(peer)
	if disco, ok := testDiscoKeys.Load(self.WireGuardPublicKey); ok {
		self.DiscoPublicKey = disco.(string)
	}
	if disco, ok := testDiscoKeys.Load(peer.WireGuardPublicKey); ok {
		peer.DiscoPublicKey = disco.(string)
	}
	return tailnet.NetworkConfiguration{Version: 1, Issuer: "https://api.example.test", Audience: "paperboat-network", IssuedAt: now, ExpiresAt: now + 300, Generation: generation, Self: self, Peers: testPeers(peer, direction, now+300)}
}

func testPeers(peer tailnet.NetworkBinding, direction string, expires int64) []tailnet.NetworkPeer {
	peer = bindTestQUICKey(peer)
	if disco, ok := testDiscoKeys.Load(peer.WireGuardPublicKey); ok {
		peer.DiscoPublicKey = disco.(string)
	}
	return []tailnet.NetworkPeer{{Identity: peer, Scopes: []tailnet.NetworkScope{
		{ResourceKind: "machine_access", ResourceID: "grant_test", ResourceGeneration: 1, Capability: "terminal", Direction: direction, Port: tailnet.NetworkPort, ExpiresAt: expires},
		{ResourceKind: "machine_access", ResourceID: "grant_test", ResourceGeneration: 1, Capability: "file_transfer", Direction: direction, Port: tailnet.NetworkPort, ExpiresAt: expires},
		{ResourceKind: "machine_access", ResourceID: "grant_test", ResourceGeneration: 1, Capability: "private_access", Direction: direction, Port: tailnet.NetworkPort, ExpiresAt: expires},
	}}}
}

func applyTestConfiguration(t *testing.T, authority *tailnet.Authority, signer ed25519.PrivateKey, value tailnet.NetworkConfiguration) {
	t.Helper()
	header, _ := json.Marshal(map[string]string{"alg": "EdDSA", "typ": "paperboat-network-config+jwt", "kid": "native_test"})
	body, _ := json.Marshal(value)
	unsigned := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(body)
	token := unsigned + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(signer, []byte(unsigned)))
	if err := authority.Apply(t.Context(), token); err != nil {
		t.Fatal(err)
	}
}

func testTLS(t *testing.T, name string) (*tls.Config, string) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}, DNSNames: []string{name}}
	raw, err := x509.CreateCertificate(rand.Reader, template, template, public, private)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := sha256.Sum256(raw)
	testQUICKeys.Store(hex.EncodeToString(fingerprint[:]), base64.RawURLEncoding.EncodeToString(public))
	return &tls.Config{MinVersion: tls.VersionTLS13, InsecureSkipVerify: true, NextProtos: []string{peerquic.ALPN}, Certificates: []tls.Certificate{{Certificate: [][]byte{raw}, PrivateKey: private}}}, hex.EncodeToString(fingerprint[:])
}

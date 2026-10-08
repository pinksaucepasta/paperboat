//go:build darwin || linux

package runtime

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	osexec "os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/auth"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/browserbroadcast"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/browserbroadcastserver"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/health"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/operation"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/process"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/pty"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/server"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/session"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/endpointidentity"
)

func TestBrowserTerminalCredentialUsesRealHostProtocolAndPTY(t *testing.T) {
	const childEnv = "PAPERBOAT_TEST_BROWSER_TERMINAL_CHILD"
	if os.Getenv(childEnv) != "1" {
		command := osexec.Command(os.Args[0], "-test.run=^TestBrowserTerminalCredentialUsesRealHostProtocolAndPTY$")
		command.Env = append(os.Environ(), childEnv+"=1")
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("connected host-runtime child failed: %v\n%s", err, output)
		}
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()

	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	const (
		issuer        = "https://control.browser-terminal.test"
		environmentID = "env_browser_terminal"
		machineID     = "machine_browser_terminal"
		helperID      = "helper_browser_terminal"
		accountID     = "account_browser"
	)
	rootPublic, rootPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	machinePublic, machinePrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	const endpointGeneration = uint64(4)
	endpointCertificate, err := endpointidentity.Sign(rootPrivate, endpointidentity.Claims{AccountID: accountID, Role: endpointidentity.RoleMachine, EndpointID: machineID, QUICPublicKey: machinePublic, Generation: endpointGeneration, Serial: 1, IssuedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	endpointCertificateBytes, err := endpointCertificate.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	serverLeaf, err := endpointidentity.NewTLSCertificate(endpointCertificate, rootPublic, machinePrivate, now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	rootFingerprint := sha256.Sum256(rootPublic)
	terminalIdentity := server.BrowserTerminalIdentity{Certificate: endpointCertificateBytes, RootKeyID: "aek_" + hex.EncodeToString(rootFingerprint[:]), TLSCertificate: serverLeaf}
	revocations := auth.NewRevocationCache()
	verifier := auth.Verifier{Keys: task44RuntimeKeys{"browser-terminal-key", public}, Clock: task44RuntimeClock{}, Revocations: revocations, ClockSkew: time.Minute}
	authorizerFactory, err := NewCredentialAuthorizer(CredentialAuthConfig{Issuer: issuer, EnvironmentID: environmentID, MachineID: machineID, HelperID: helperID, Verifier: verifier, Revocations: revocations})
	if err != nil {
		t.Fatal(err)
	}
	browserAuthorizerFactory, err := NewBrowserTerminalCredentialAuthorizer(CredentialAuthConfig{Issuer: issuer, EnvironmentID: environmentID, MachineID: machineID, HelperID: helperID, Verifier: verifier, Revocations: revocations})
	if err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	adapter, err := pty.NewAdapter(root)
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := session.NewManager(session.ManagerConfig{Launch: func(command pty.Command) (session.PTYProcess, error) { return adapter.Start(command) }, MaxSessions: 4, MaxAttachments: 8})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		shutdown, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		if shutdownErr := sessions.Shutdown(shutdown); shutdownErr != nil {
			t.Errorf("PTY shutdown: %v", shutdownErr)
		}
	}()
	shell := browserTestShell(t)
	shellProgram := `printf 'browser-terminal-ready\n'; stty raw -echo; key_bytes=$(dd bs=1 count=6 2>/dev/null | od -An -tx1); printf 'INPUT_BYTES=%s\n' "$key_bytes"; stty sane; while IFS= read -r line; do if [ "$line" = exit ]; then printf 'shell-exit-requested\n'; exit 0; fi; printf 'line=%s\n' "$line"; done`
	created, err := sessions.Create(ctx, session.CreateRequest{ID: "default", Name: "browser-default", Command: pty.Command{Path: shell, Args: []string{"-c", shellProgram}, CWD: root, Env: []string{"PATH=/usr/bin:/bin", "TERM=xterm"}, Dimensions: pty.Dimensions{Columns: 80, Rows: 24}}})
	if err != nil {
		t.Fatal(err)
	}
	launcher, err := process.NewShellLauncher(shell, []string{"PATH=/usr/bin:/bin", "TERM=xterm", "SHELL=" + shell}, sessions)
	if err != nil {
		t.Fatal(err)
	}
	broadcast, err := browserbroadcastserver.NewRegistry(sessions, func(context.Context) (ed25519.PrivateKey, error) { return machinePrivate, nil })
	if err != nil {
		t.Fatal(err)
	}
	outputReader, outputWriter := io.Pipe()
	defer outputReader.Close()
	records := make(chan []byte, 256)
	go func() {
		defer close(records)
		for {
			var header [4]byte
			if _, err := io.ReadFull(outputReader, header[:]); err != nil {
				return
			}
			n := binary.BigEndian.Uint32(header[:])
			if n == 0 || n > browserbroadcast.MaxRecordBytes {
				return
			}
			payload := make([]byte, n)
			if _, err := io.ReadFull(outputReader, payload); err != nil {
				return
			}
			records <- payload
		}
	}()
	broadcast.SetPublisher(func(context.Context, string) (io.WriteCloser, error) { return outputWriter, nil })
	defer broadcast.SetPublisher(nil)
	dispatcher, err := server.NewDispatcher(server.DispatcherConfig{Sessions: sessions, BrowserOutput: broadcast, Health: health.New("browser-terminal", []string{"terminal.v1", "health.v1"}, nil), SessionLauncher: launcher, WorkspaceRoot: root, Random: rand.Reader})
	if err != nil {
		t.Fatal(err)
	}
	journal, err := operation.NewJournal(32)
	if err != nil {
		t.Fatal(err)
	}
	runtimeServer, err := server.New(server.Config{Negotiator: protocol.Negotiator{Available: map[string]bool{"terminal.v1": true, "health.v1": true}}, Journal: journal, Handler: dispatcher, MaxConcurrent: 8, HeartbeatInterval: time.Hour, MutationDeadline: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		shutdown, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		if shutdownErr := runtimeServer.Shutdown(shutdown); shutdownErr != nil {
			t.Errorf("host protocol shutdown: %v", shutdownErr)
		}
	}()
	websocketHandler, err := server.NewWebSocketHandler(server.WebSocketHandlerConfig{Server: runtimeServer, MaxConnections: 8, Authorizer: authorizerFactory})
	if err != nil {
		t.Fatal(err)
	}
	browserTerminalHandler, err := server.NewBrowserTerminalWebSocketHandler(server.BrowserTerminalWebSocketHandlerConfig{Server: runtimeServer, MaxConnections: 8, Authorizer: browserAuthorizerFactory, Identity: func(context.Context) (server.BrowserTerminalIdentity, error) { return terminalIdentity, nil }})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/v1/runtime", websocketHandler)
	mux.Handle("/v1/browser-terminal", browserTerminalHandler)
	host := httptest.NewTLSServer(mux)
	defer host.Close()
	wsURL := "wss" + strings.TrimPrefix(host.URL, "https") + "/v1/browser-terminal"

	makeToken := func(scope, jti, attachmentID string, generation uint64, browserKeyDigest string) string {
		t.Helper()
		claims := auth.Claims{Issuer: issuer, Audience: "paperboat-machine", Subject: "user_browser", JTI: jti, IssuedAt: now.Add(-time.Minute).Unix(), ExpiresAt: now.Add(4 * time.Minute).Unix(), Scope: []string{scope}, CredentialClass: "browser_terminal_operation", EnvironmentID: environmentID, AccountID: accountID, UserID: "user_browser", MachineID: machineID, SessionID: created.ID, BrowserAttachmentID: attachmentID, BrowserPublicKeySHA256: browserKeyDigest, ExpectedGeneration: int64(generation), PolicyGeneration: 4}
		return signStaticCredential(t, private, "browser-terminal-key", claims)
	}
	dial := func(scope, jti, attachmentID string, generation uint64) *browserTerminalTestClient {
		t.Helper()
		browserCertificate, browserKeyDigest := newBrowserTerminalClientCertificate(t, now)
		token := makeToken(scope, jti, attachmentID, generation, browserKeyDigest)
		connection, response, dialErr := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPClient: host.Client(), HTTPHeader: http.Header{"Authorization": []string{"Bearer " + token}}, Subprotocols: []string{server.BrowserTerminalWebSocketSubprotocol}})
		if dialErr != nil {
			if response != nil {
				response.Body.Close()
			}
			t.Fatalf("host WebSocket dial: %v", dialErr)
		}
		identityType, identityBody, identityErr := connection.Read(ctx)
		if identityErr != nil || identityType != websocket.MessageBinary {
			t.Fatalf("host did not send its binary identity envelope first: type=%v err=%v", identityType, identityErr)
		}
		var envelope struct {
			Certificate string `json:"certificate"`
			RootKeyID   string `json:"root_key_id"`
		}
		if json.Unmarshal(identityBody, &envelope) != nil || envelope.RootKeyID != terminalIdentity.RootKeyID {
			t.Fatalf("invalid host identity envelope: %s", identityBody)
		}
		peerCertificate, decodeErr := base64.RawURLEncoding.Strict().DecodeString(envelope.Certificate)
		if decodeErr != nil {
			t.Fatalf("decode endpoint certificate: %v", decodeErr)
		}
		clientTLSConfig, tlsConfigErr := endpointidentity.ClientTLS(browserCertificate, endpointidentity.PeerExpectation{RootPublic: rootPublic, Certificate: peerCertificate, Expected: endpointidentity.Expected{AccountID: accountID, Role: endpointidentity.RoleMachine, EndpointID: machineID, Generation: endpointGeneration}}, server.BrowserTerminalTLSALPN, time.Now)
		if tlsConfigErr != nil {
			t.Fatalf("build pinned browser TLS client: %v", tlsConfigErr)
		}
		stream, streamCancel := browserTerminalTestPipe(ctx, connection)
		tlsConnection := tls.Client(stream, clientTLSConfig)
		handshakeCtx, handshakeCancel := context.WithTimeout(ctx, 3*time.Second)
		tlsErr := tlsConnection.HandshakeContext(handshakeCtx)
		handshakeCancel()
		if tlsErr != nil {
			streamCancel()
			_ = connection.Close(websocket.StatusInternalError, "tls_failed")
			t.Fatalf("inner TLS handshake: %v", tlsErr)
		}
		client := &browserTerminalTestClient{tls: tlsConnection, websocket: connection, stream: stream, cancel: streamCancel, token: token, broadcast: records, inbox: make(chan browserTestMessage, 128), done: make(chan struct{}), epochs: make(map[[16]byte]browserbroadcast.Epoch), keyChanged: make(chan struct{}, 1), machinePublic: machinePublic, sessionID: created.ID, generation: created.Generation}
		go client.readLoop()
		t.Cleanup(func() { _ = client.Close(websocket.StatusNormalClosure, "test cleanup") })
		writeBrowserProtocolFrame(t, client, protocol.Frame{Type: "hello", RequestID: "hello_browser", Version: protocol.ProtocolVersion, Payload: json.RawMessage(`{"min_version":"1.0","max_version":"1.0","capabilities":["terminal.v1","health.v1"]}`)})
		welcome := readBrowserProtocolFrame(t, client)
		if welcome.Type != "welcome" {
			t.Fatalf("welcome frame=%#v", welcome)
		}
		return client
	}
	attach := func(connection *browserTerminalTestClient, opID, requestID string) (uint32, uint64, string, *protocol.Frame) {
		t.Helper()
		payload, marshalErr := json.Marshal(map[string]any{"action": "attach", "session_id": created.ID, "from_sequence": 0})
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		writeBrowserProtocolFrame(t, connection, protocol.Frame{Type: "request", RequestID: requestID, Version: protocol.ProtocolVersion, OperationID: opID, Capability: "terminal.v1", DeadlineMS: 3000, Payload: payload})
		response := readBrowserProtocolFrame(t, connection)
		if response.Type == "error" {
			return 0, 0, "", &response
		}
		var envelope struct {
			Result struct {
				StreamID      uint32 `json:"stream_id"`
				AttachmentID  string `json:"attachment_id"`
				InputSequence uint64 `json:"input_sequence"`
			} `json:"result"`
		}
		if response.Type != "response" || json.Unmarshal(response.Payload, &envelope) != nil || envelope.Result.StreamID == 0 || envelope.Result.AttachmentID == "" {
			t.Fatalf("invalid attach response=%#v", response)
		}
		return envelope.Result.StreamID, envelope.Result.InputSequence, envelope.Result.AttachmentID, nil
	}

	owner := dial("terminal:operate", "jti_browser_owner", "att_browser_owner", created.Generation)
	ownerStreamID, ownerSequence, ownerAttachmentID, failure := attach(owner, "op_browser_owner_attach_001", "req_browser_owner_attach")
	if failure != nil || ownerAttachmentID != "att_browser_owner" {
		t.Fatalf("owner attach failed or changed bound attachment: failure=%#v attachment=%q", failure, ownerAttachmentID)
	}
	if got := readBrowserTerminalOutput(t, ctx, owner, "browser-terminal-ready"); !bytes.Contains(got, []byte("browser-terminal-ready")) {
		t.Fatalf("initial PTY output missing: %q", got)
	}
	ownerResize, err := protocol.EncodeTerminalResize(protocol.TerminalResizeFrame{StreamID: ownerStreamID, Columns: 96, Rows: 32, Sequence: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := owner.Write(ctx, websocket.MessageBinary, ownerResize); err != nil {
		t.Fatal(err)
	}
	resizeDeadline := time.Now().Add(2 * time.Second)
	for {
		resized, snapshotErr := sessions.SnapshotAtGeneration(created.ID, created.Generation)
		if snapshotErr != nil {
			t.Fatalf("snapshot PTY after browser resize: %v", snapshotErr)
		}
		if resized.Dimensions.Columns == 96 && resized.Dimensions.Rows == 32 {
			break
		}
		if time.Now().After(resizeDeadline) {
			t.Fatalf("owner browser resize did not reach the PTY: dimensions=%+v", resized.Dimensions)
		}
		time.Sleep(10 * time.Millisecond)
	}
	keySequence := []byte{0x04, 0x03, 0x1b, '[', 'A', 'x'}
	keySequenceInput, err := protocol.EncodeTerminalInput(protocol.TerminalInputFrame{StreamID: ownerStreamID, Sequence: ownerSequence + 1, Data: keySequence}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := owner.Write(ctx, websocket.MessageBinary, keySequenceInput); err != nil {
		t.Fatal(err)
	}
	if got := readBrowserTerminalOutput(t, ctx, owner, "INPUT_BYTES="); !bytes.Contains(got, []byte("04 03 1b 5b 41 78")) {
		t.Fatalf("PTY program did not receive the exact control/escape/ordinary input bytes: %q", got)
	}
	if afterEOT, snapshotErr := sessions.SnapshotAtGeneration(created.ID, created.Generation); snapshotErr != nil || afterEOT.State != session.Running {
		t.Fatalf("control input closed a program that consumed the bytes: snapshot=%#v err=%v", afterEOT, snapshotErr)
	}
	lineInput, err := protocol.EncodeTerminalInput(protocol.TerminalInputFrame{StreamID: ownerStreamID, Sequence: ownerSequence + 2, Data: []byte("alive-after-eot\n")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := owner.Write(ctx, websocket.MessageBinary, lineInput); err != nil {
		t.Fatal(err)
	}
	if got := readBrowserTerminalOutput(t, ctx, owner, "line=alive-after-eot"); !bytes.Contains(got, []byte("line=alive-after-eot")) {
		t.Fatalf("PTY program did not remain interactive after control input: %q", got)
	}
	if stillLive, snapshotErr := sessions.SnapshotAtGeneration(created.ID, created.Generation); snapshotErr != nil || stillLive.State != session.Running {
		t.Fatalf("PTY program exited before its exit command: snapshot=%#v err=%v", stillLive, snapshotErr)
	}

	for _, action := range []string{"create", "close", "delete", "restart"} {
		payload, _ := json.Marshal(map[string]any{"action": action, "session_id": created.ID, "name": "should-not-be-created", "attachment_id": ownerAttachmentID, "columns": 90, "rows": 30})
		writeBrowserProtocolFrame(t, owner, protocol.Frame{Type: "request", RequestID: "req_browser_forbid_" + action, Version: protocol.ProtocolVersion, OperationID: "op_browser_forbid_" + action + "_001", Capability: "terminal.v1", DeadlineMS: 1000, Payload: payload})
		response := readBrowserProtocolFrame(t, owner)
		if response.Type != "error" || !bytes.Contains(response.Payload, []byte("not_found_or_forbidden")) {
			t.Fatalf("browser class allowed lifecycle action %q: %#v", action, response)
		}
	}
	if stillLive, snapshotErr := sessions.SnapshotAtGeneration(created.ID, created.Generation); snapshotErr != nil || stillLive.State != session.Running {
		t.Fatalf("forbidden lifecycle requests changed host session: snapshot=%#v err=%v", stillLive, snapshotErr)
	}

	viewer := dial("terminal:view", "jti_browser_viewer", "att_browser_viewer", created.Generation)
	viewerStreamID, viewerSequence, _, failure := attach(viewer, "op_browser_viewer_attach_001", "req_browser_viewer_attach")
	if failure != nil {
		t.Fatalf("viewer attach failed: %#v", failure)
	}
	viewerInput, err := protocol.EncodeTerminalInput(protocol.TerminalInputFrame{StreamID: viewerStreamID, Sequence: viewerSequence + 1, Data: []byte("viewer-must-not-type\n")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := viewer.Write(ctx, websocket.MessageBinary, viewerInput); err != nil {
		t.Fatal(err)
	}
	if status, code := readBrowserInputDecision(t, ctx, viewer); status != string(session.InputRejected) || code != "invalid_input" {
		t.Fatalf("viewer input decision status=%q code=%q", status, code)
	}
	viewerResize, err := protocol.EncodeTerminalResize(protocol.TerminalResizeFrame{StreamID: viewerStreamID, Columns: 100, Rows: 36, Sequence: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := viewer.Write(ctx, websocket.MessageBinary, viewerResize); err != nil {
		t.Fatal(err)
	}
	waitBrowserSocketClosed(t, ctx, viewer, "viewer resize did not close the unauthorized terminal stream")
	if afterDeniedResize, snapshotErr := sessions.Snapshot(created.ID); snapshotErr != nil || afterDeniedResize.Dimensions.Columns != 96 || afterDeniedResize.Dimensions.Rows != 32 {
		t.Fatalf("viewer changed shared PTY dimensions: snapshot=%#v err=%v", afterDeniedResize, snapshotErr)
	}

	stale := dial("terminal:operate", "jti_browser_stale", "att_browser_stale", created.Generation+1)
	_, _, _, staleFailure := attach(stale, "op_browser_stale_attach_001", "req_browser_stale_attach")
	if staleFailure == nil || !bytes.Contains(staleFailure.Payload, []byte("stale_generation")) {
		t.Fatalf("stale host process generation accepted: %#v", staleFailure)
	}
	if afterStale, snapshotErr := sessions.Snapshot(created.ID); snapshotErr != nil {
		t.Fatalf("snapshot after stale attach: %v", snapshotErr)
	} else {
		for _, participant := range afterStale.Participants {
			if participant.AttachmentID == "att_browser_stale" {
				t.Fatalf("stale attach left a participant: %#v", afterStale.Participants)
			}
		}
	}

	revoked := dial("terminal:operate", "jti_browser_revoked", "att_browser_revoked", created.Generation)
	if _, _, attachmentID, failure := attach(revoked, "op_browser_revoked_attach_001", "req_browser_revoked_attach"); failure != nil || attachmentID != "att_browser_revoked" {
		t.Fatalf("revocation test attach failed: failure=%#v attachment=%q", failure, attachmentID)
	}
	if err := revocations.Replace([]string{"jti_browser_revoked"}); err != nil {
		t.Fatal(err)
	}
	waitBrowserSocketClosed(t, ctx, revoked, "active browser terminal did not fail closed on revocation")

	exitInput, err := protocol.EncodeTerminalInput(protocol.TerminalInputFrame{StreamID: ownerStreamID, Sequence: ownerSequence + 3, Data: []byte("exit\n")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := owner.Write(ctx, websocket.MessageBinary, exitInput); err != nil {
		t.Fatal(err)
	}
	if got := readBrowserTerminalOutput(t, ctx, owner, "shell-exit-requested"); !bytes.Contains(got, []byte("shell-exit-requested")) {
		t.Fatalf("shell did not process its exit command: %q", got)
	}
	if state := readBrowserTerminalEnd(t, ctx, owner); state != string(session.Exited) {
		t.Fatalf("natural shell exit did not produce a user-visible terminal end: state=%q", state)
	}
	if exited, snapshotErr := sessions.Snapshot(created.ID); snapshotErr != nil || exited.State != session.Exited {
		t.Fatalf("exit command did not end the shell: snapshot=%#v err=%v", exited, snapshotErr)
	}
	restarted, err := sessions.Restart(created.ID)
	if err != nil || restarted.Generation <= created.Generation || restarted.State != session.Running {
		t.Fatalf("test restart failed to advance host process generation: snapshot=%#v err=%v", restarted, err)
	}
	_, _, _, afterRestartFailure := attach(owner, "op_browser_old_generation_001", "req_browser_old_generation")
	if afterRestartFailure == nil || !bytes.Contains(afterRestartFailure.Payload, []byte("stale_generation")) {
		t.Fatalf("browser credential attached after host process restart: %#v", afterRestartFailure)
	}
	var beforeClose session.Snapshot
	outputDeadline := time.Now().Add(2 * time.Second)
	for {
		beforeClose, err = sessions.SnapshotAtGeneration(created.ID, restarted.Generation)
		if err != nil {
			t.Fatalf("snapshot restarted shell before close: %v", err)
		}
		if beforeClose.LatestSequence > beforeClose.EarliestSequence {
			break
		}
		if time.Now().After(outputDeadline) {
			t.Fatalf("test close had no retained output to clear: snapshot=%#v", beforeClose)
		}
		time.Sleep(10 * time.Millisecond)
	}
	closed, err := sessions.CloseAtGeneration(ctx, created.ID, restarted.Generation)
	if err != nil || closed.State != session.Closed || closed.EarliestSequence != closed.LatestSequence {
		t.Fatalf("closing the restarted PTY did not clear retained output: snapshot=%#v err=%v", closed, err)
	}
}

func browserTestShell(t *testing.T) string {
	t.Helper()
	for _, candidate := range []string{"/bin/bash", "/usr/bin/bash", "/bin/zsh", "/usr/bin/zsh", "/bin/sh", "/usr/bin/sh"} {
		info, err := os.Lstat(candidate)
		if err == nil && info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
			return candidate
		}
	}
	t.Skip("requires a canonical shell executable")
	return ""
}

type browserTerminalTestClient struct {
	tls           *tls.Conn
	websocket     *websocket.Conn
	stream        net.Conn
	cancel        context.CancelFunc
	token         string
	broadcast     <-chan []byte
	inbox         chan browserTestMessage
	done          chan struct{}
	readErr       error
	mu            sync.Mutex
	epochs        map[[16]byte]browserbroadcast.Epoch
	keyChanged    chan struct{}
	machinePublic ed25519.PublicKey
	sessionID     string
	generation    uint64
	screen        []byte
	cursor        uint64
}

type browserTestMessage struct {
	kind    websocket.MessageType
	payload []byte
}

func (c *browserTerminalTestClient) Write(ctx context.Context, messageType websocket.MessageType, payload []byte) error {
	if messageType != websocket.MessageText && messageType != websocket.MessageBinary {
		return errors.New("unsupported browser terminal test record")
	}
	limit := protocol.MaxStructuredFrame
	kind := byte(1)
	if messageType == websocket.MessageBinary {
		limit = protocol.MaxBinaryFrame
		kind = 2
	}
	if len(payload) == 0 || len(payload) > limit {
		return errors.New("invalid browser terminal test record size")
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = c.tls.SetWriteDeadline(deadline)
	} else {
		_ = c.tls.SetWriteDeadline(time.Time{})
	}
	defer c.tls.SetWriteDeadline(time.Time{})
	var header [5]byte
	header[0] = kind
	putBrowserTestLength(header[1:], uint32(len(payload)))
	if err := writeBrowserTestAll(c.tls, header[:]); err != nil {
		return err
	}
	return writeBrowserTestAll(c.tls, payload)
}

func (c *browserTerminalTestClient) Read(ctx context.Context) (websocket.MessageType, []byte, error) {
	select {
	case value := <-c.inbox:
		return value.kind, value.payload, nil
	default:
	}
	select {
	case value := <-c.inbox:
		return value.kind, value.payload, nil
	case <-c.done:
		c.mu.Lock()
		err := c.readErr
		c.mu.Unlock()
		return 0, nil, err
	case <-ctx.Done():
		return 0, nil, ctx.Err()
	}
}

func (c *browserTerminalTestClient) readLoop() {
	defer close(c.done)
	for {
		kind, payload, err := c.readRaw(context.Background())
		if err != nil {
			c.mu.Lock()
			c.readErr = err
			c.mu.Unlock()
			return
		}
		if kind == websocket.MessageBinary {
			frame, decodeErr := protocol.DecodeTerminalOutput(payload)
			if decodeErr == nil && frame.Channel == protocol.TerminalScreenCheckpoint && len(frame.Data) > 0 {
				c.mu.Lock()
				c.screen = append(c.screen, frame.Data[1:]...)
				c.cursor = frame.StartSequence
				c.mu.Unlock()
				select {
				case c.keyChanged <- struct{}{}:
				default:
				}
				continue
			}
			if decodeErr == nil && frame.Channel == protocol.TerminalBroadcastKey && len(frame.Data) == 49 && frame.Data[0] == 1 {
				var epoch browserbroadcast.Epoch
				copy(epoch.ID[:], frame.Data[1:17])
				copy(epoch.Key[:], frame.Data[17:])
				c.mu.Lock()
				c.epochs[epoch.ID] = epoch
				c.mu.Unlock()
				select {
				case c.keyChanged <- struct{}{}:
				default:
				}
				continue
			}
		}
		select {
		case c.inbox <- browserTestMessage{kind, payload}:
		default:
			c.mu.Lock()
			c.readErr = errors.New("test client inbox full")
			c.mu.Unlock()
			return
		}
	}
}

func (c *browserTerminalTestClient) readRaw(ctx context.Context) (websocket.MessageType, []byte, error) {
	if deadline, ok := ctx.Deadline(); ok {
		_ = c.tls.SetReadDeadline(deadline)
	} else {
		_ = c.tls.SetReadDeadline(time.Time{})
	}
	defer c.tls.SetReadDeadline(time.Time{})
	var header [5]byte
	if _, err := io.ReadFull(c.tls, header[:]); err != nil {
		return 0, nil, err
	}
	length := browserTestLength(header[1:])
	limit := uint32(protocol.MaxStructuredFrame)
	messageType := websocket.MessageText
	if header[0] == 2 {
		limit = uint32(protocol.MaxBinaryFrame)
		messageType = websocket.MessageBinary
	} else if header[0] != 1 {
		return 0, nil, errors.New("invalid browser terminal test record kind")
	}
	if length == 0 || length > limit {
		return 0, nil, errors.New("invalid browser terminal test record length")
	}
	payload := make([]byte, int(length))
	if _, err := io.ReadFull(c.tls, payload); err != nil {
		return 0, nil, err
	}
	return messageType, payload, nil
}

func (c *browserTerminalTestClient) Close(code websocket.StatusCode, reason string) error {
	_ = c.tls.Close()
	c.cancel()
	_ = c.stream.Close()
	return c.websocket.Close(code, reason)
}

func newBrowserTerminalClientCertificate(t *testing.T, now time.Time) (tls.Certificate, string) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "paperboat browser test"},
		NotBefore:    now.UTC().Add(-time.Minute).Truncate(time.Second),
		NotAfter:     now.UTC().Add(4 * time.Minute).Truncate(time.Second),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	encoded, err := x509.CreateCertificate(rand.Reader, template, template, public, private)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(encoded)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
	return tls.Certificate{Certificate: [][]byte{encoded}, PrivateKey: private, Leaf: leaf}, base64.RawURLEncoding.EncodeToString(digest[:])
}

func browserTerminalTestPipe(parent context.Context, connection *websocket.Conn) (net.Conn, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	serverSide, clientSide := net.Pipe()
	go func() {
		defer serverSide.Close()
		for {
			messageType, payload, err := connection.Read(ctx)
			if err != nil || messageType != websocket.MessageBinary || len(payload) == 0 {
				return
			}
			if err := writeBrowserTestAll(serverSide, payload); err != nil {
				return
			}
		}
	}()
	go func() {
		defer serverSide.Close()
		buffer := make([]byte, 32<<10)
		for {
			count, err := serverSide.Read(buffer)
			if count > 0 {
				payload := append([]byte(nil), buffer[:count]...)
				if writeErr := connection.Write(ctx, websocket.MessageBinary, payload); writeErr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	return clientSide, cancel
}

func putBrowserTestLength(destination []byte, value uint32) {
	destination[0] = byte(value >> 24)
	destination[1] = byte(value >> 16)
	destination[2] = byte(value >> 8)
	destination[3] = byte(value)
}

func browserTestLength(value []byte) uint32 {
	return uint32(value[0])<<24 | uint32(value[1])<<16 | uint32(value[2])<<8 | uint32(value[3])
}

func writeBrowserTestAll(writer io.Writer, payload []byte) error {
	for len(payload) > 0 {
		count, err := writer.Write(payload)
		if err != nil {
			return err
		}
		if count == 0 {
			return io.ErrShortWrite
		}
		payload = payload[count:]
	}
	return nil
}

func waitBrowserSocketClosed(t *testing.T, ctx context.Context, connection *browserTerminalTestClient, failureMessage string) {
	t.Helper()
	deadline, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	for {
		_, _, err := connection.Read(deadline)
		if err == nil {
			continue
		}
		if errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("%s: %v", failureMessage, err)
		}
		return
	}
}

func readBrowserTerminalEnd(t *testing.T, ctx context.Context, connection *browserTerminalTestClient) string {
	t.Helper()
	deadline, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	for {
		messageType, encoded, err := connection.Read(deadline)
		if err != nil {
			t.Fatalf("read terminal stream end: %v", err)
		}
		if messageType != websocket.MessageText {
			continue
		}
		var frame protocol.Frame
		if json.Unmarshal(encoded, &frame) != nil || frame.Type != "event" {
			continue
		}
		var event struct {
			Event string `json:"event"`
			State string `json:"state"`
		}
		if json.Unmarshal(frame.Payload, &event) == nil && event.Event == "terminal_stream_end" {
			return event.State
		}
	}
}

func writeBrowserProtocolFrame(t *testing.T, connection *browserTerminalTestClient, frame protocol.Frame) {
	t.Helper()
	encoded, err := json.Marshal(frame)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := connection.Write(ctx, websocket.MessageText, encoded); err != nil {
		t.Fatal(err)
	}
}

func readBrowserProtocolFrame(t *testing.T, connection *browserTerminalTestClient) protocol.Frame {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	for {
		messageType, encoded, err := connection.Read(ctx)
		if err != nil {
			t.Fatalf("read host protocol frame: %v", err)
		}
		if messageType != websocket.MessageText {
			continue
		}
		var frame protocol.Frame
		if err := json.Unmarshal(encoded, &frame); err != nil {
			t.Fatalf("decode host protocol frame: %v", err)
		}
		if frame.Type == "event" {
			continue
		}
		return frame
	}
}

func readBrowserTerminalOutput(t *testing.T, ctx context.Context, connection *browserTerminalTestClient, marker string) []byte {
	t.Helper()
	deadline, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	var pending []byte
	for {
		connection.mu.Lock()
		output := bytes.Clone(connection.screen)
		cursor := connection.cursor
		connection.mu.Unlock()
		if bytes.Contains(output, []byte(marker)) {
			return output
		}
		if len(pending) != 0 {
			hint, err := browserbroadcast.EpochHint(pending)
			if err != nil {
				t.Fatal(err)
			}
			connection.mu.Lock()
			epoch, ok := connection.epochs[hint]
			connection.mu.Unlock()
			if ok {
				record, err := browserbroadcast.Open(pending, epoch, connection.machinePublic)
				if err != nil || record.SessionID != connection.sessionID || record.Generation != connection.generation {
					t.Fatalf("invalid authenticated output: %v", err)
				}
				end := record.StartSequence + uint64(len(record.Data))
				if end > cursor {
					if record.StartSequence > cursor {
						t.Fatalf("shared output gap: got %d want %d", record.StartSequence, cursor)
					}
					data := record.Data
					if record.StartSequence < cursor {
						data = data[cursor-record.StartSequence:]
					}
					connection.mu.Lock()
					connection.screen = append(connection.screen, data...)
					connection.cursor = end
					connection.mu.Unlock()
				}
				pending = nil
				continue
			}
		}
		// Keys and ciphertext arrive on independent streams. Keep later records
		// in the bounded feed until this record's authenticated key arrives.
		broadcast := connection.broadcast
		if len(pending) != 0 {
			broadcast = nil
		}
		select {
		case raw, ok := <-broadcast:
			if !ok {
				t.Fatal("shared output publisher stopped")
			}
			pending = raw
		case <-connection.keyChanged:
		case <-deadline.Done():
			t.Fatalf("shared output %q did not arrive: %v", marker, deadline.Err())
		}
	}
}

func readBrowserInputDecision(t *testing.T, ctx context.Context, connection *browserTerminalTestClient) (string, string) {
	t.Helper()
	deadline, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	for {
		messageType, encoded, err := connection.Read(deadline)
		if err != nil {
			t.Fatalf("read terminal input decision: %v", err)
		}
		if messageType != websocket.MessageText {
			continue
		}
		var frame protocol.Frame
		if json.Unmarshal(encoded, &frame) != nil || frame.Type != "event" {
			continue
		}
		var decision struct {
			Status    string `json:"status"`
			ErrorCode string `json:"error_code"`
		}
		if json.Unmarshal(frame.Payload, &decision) == nil && decision.Status != "" {
			return decision.Status, decision.ErrorCode
		}
	}
}

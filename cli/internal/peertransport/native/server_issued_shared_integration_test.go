package native_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/config"
	clienttransfer "github.com/pinksaucepasta/paperboat/internal/filetransfer"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/auth"
	hostconfig "github.com/pinksaucepasta/paperboat/internal/hostruntime/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/execprocess"
	hosttransfer "github.com/pinksaucepasta/paperboat/internal/hostruntime/filetransfer"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/health"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/nativesession"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/operation"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/peerrelay"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/pty"
	hostruntime "github.com/pinksaucepasta/paperboat/internal/hostruntime/runtime"
	hostserver "github.com/pinksaucepasta/paperboat/internal/hostruntime/server"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/session"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/store"
	"github.com/pinksaucepasta/paperboat/internal/managedssh"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/native"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/peerquic"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/streamauth"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
	"github.com/pinksaucepasta/paperboat/internal/resolver"
	"github.com/pinksaucepasta/paperboat/internal/tunnel"
	"tailscale.com/tstest/integration"
)

type serverIssuedDescriptor struct {
	OperationID string         `json:"operation_id"`
	Auth        map[string]any `json:"auth"`
	ExpiresAt   time.Time      `json:"expires_at"`
}

type serverIssuedTerminalDescriptor struct {
	ExpiresAt time.Time      `json:"expires_at"`
	Terminal  map[string]any `json:"terminal"`
}

type serverIssuedFileDescriptor struct {
	Endpoint             string         `json:"endpoint"`
	SourceMachineID      string         `json:"source_machine_id"`
	DestinationMachineID string         `json:"destination_machine_id"`
	InitiatingUserID     string         `json:"initiating_user_id"`
	Auth                 map[string]any `json:"auth"`
}

type serverIssuedSharedFixture struct {
	Issuer                                  string                         `json:"issuer"`
	KeyID                                   string                         `json:"signing_key_id"`
	Public                                  string                         `json:"signing_public_key"`
	CLI                                     tailnet.NetworkBinding         `json:"cli"`
	SharedCLI                               tailnet.NetworkBinding         `json:"shared_cli"`
	Machine                                 tailnet.NetworkBinding         `json:"machine"`
	CLIPrivate                              string                         `json:"cli_private_key"`
	SharedCLIPrivate                        string                         `json:"shared_cli_private_key"`
	MachinePrivate                          string                         `json:"machine_private_key"`
	OwnerConfiguration                      string                         `json:"owner_shared_configuration"`
	SharedConfiguration                     string                         `json:"shared_cli_configuration"`
	SharedRevoked                           string                         `json:"shared_cli_revoked_configuration"`
	MachineShared                           string                         `json:"machine_shared_configuration"`
	SharedDeviceDisabled                    string                         `json:"shared_device_disabled_configuration"`
	MachineDeviceDisabled                   string                         `json:"machine_device_disabled_configuration"`
	MachineRevoked                          string                         `json:"machine_shared_revoked_configuration"`
	EnvironmentID                           string                         `json:"environment_id"`
	HelperID                                string                         `json:"helper_id"`
	OwnerTLSCert                            string                         `json:"owner_cli_tls_der"`
	OwnerTLSPrivate                         string                         `json:"owner_cli_tls_private"`
	SharedTLSCert                           string                         `json:"shared_cli_tls_der"`
	SharedTLSPrivate                        string                         `json:"shared_cli_tls_private"`
	MachineTLSCert                          string                         `json:"machine_tls_der"`
	MachineTLSPrivate                       string                         `json:"machine_tls_private"`
	ExecDescriptor                          serverIssuedDescriptor         `json:"exec_descriptor"`
	ActiveExecDescriptor                    serverIssuedDescriptor         `json:"active_exec_descriptor"`
	AfterDisableExecDescriptor              serverIssuedDescriptor         `json:"after_disable_exec_descriptor"`
	OwnerExecDescriptor                     serverIssuedDescriptor         `json:"owner_exec_descriptor"`
	TerminalDescriptor                      serverIssuedTerminalDescriptor `json:"terminal_descriptor"`
	TerminalOwnerDescriptor                 serverIssuedTerminalDescriptor `json:"terminal_owner_descriptor"`
	TerminalViewerDescriptor                serverIssuedTerminalDescriptor `json:"terminal_viewer_descriptor"`
	TerminalInteractiveDescriptor           serverIssuedTerminalDescriptor `json:"terminal_interactive_descriptor"`
	TerminalViewerJTI                       string                         `json:"terminal_viewer_jti"`
	TerminalViewerConfiguration             string                         `json:"terminal_viewer_configuration"`
	TerminalViewerMachineConfiguration      string                         `json:"terminal_viewer_machine_configuration"`
	TerminalInteractiveConfiguration        string                         `json:"terminal_interactive_configuration"`
	TerminalInteractiveMachineConfiguration string                         `json:"terminal_interactive_machine_configuration"`
	TerminalRevokedConfiguration            string                         `json:"terminal_revoked_configuration"`
	FileBatchID                             string                         `json:"file_batch_id"`
	FileDescriptor                          serverIssuedFileDescriptor     `json:"file_descriptor"`
	SSHDescriptor                           serverIssuedDescriptor         `json:"ssh_descriptor"`
	SSHHostPublicKey                        string                         `json:"ssh_host_public_key"`
	SSHHostPrivateKey                       string                         `json:"ssh_host_private_key"`
	SSHClientPublicKey                      string                         `json:"ssh_client_public_key"`
	SSHClientPrivateKey                     string                         `json:"ssh_client_private_key"`
	FinalSharedExecDescriptor               serverIssuedDescriptor         `json:"final_shared_exec_descriptor"`
	FinalSharedConfiguration                string                         `json:"final_shared_configuration"`
	FinalMachineConfiguration               string                         `json:"final_machine_configuration"`
	RemovedOwnerConfiguration               string                         `json:"removed_owner_configuration"`
}

type liveCredentialClock struct{}

func (liveCredentialClock) Now() time.Time { return time.Now().UTC() }

type liveCredentialKeys map[string]ed25519.PublicKey

func (k liveCredentialKeys) Lookup(_ context.Context, keyID string) (ed25519.PublicKey, bool, error) {
	key, ok := k[keyID]
	return key, ok, nil
}
func (liveCredentialKeys) Refresh(context.Context) error { return nil }

func TestServerIssuedCrossAccountNativeRuntime(t *testing.T) {
	path := os.Getenv("PAPERBOAT_SHARED_NETWORK_FIXTURE")
	terminalOnly := false
	if path == "" {
		path = os.Getenv("PAPERBOAT_TERMINAL_SHARING_FIXTURE")
		terminalOnly = path != ""
	}
	if path == "" {
		t.Skip("requires SQL-produced shared network fixture")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	succeeded := false
	defer func() {
		clear(raw)
		if succeeded {
			_ = os.Remove(path)
		}
	}()
	var fixture serverIssuedSharedFixture
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.SharedCLI.AccountID == fixture.Machine.AccountID || fixture.CLI.AccountID != fixture.Machine.AccountID {
		t.Fatalf("fixture account binding shared=%q owner=%q machine=%q", fixture.SharedCLI.AccountID, fixture.CLI.AccountID, fixture.Machine.AccountID)
	}
	publicRaw, err := base64.RawURLEncoding.DecodeString(fixture.Public)
	if err != nil || len(publicRaw) != ed25519.PublicKeySize {
		t.Fatal("invalid fixture signing key")
	}
	keys := testKeys{fixture.KeyID: ed25519.PublicKey(publicRaw)}
	makeAuthority := func(binding tailnet.NetworkBinding, private, token string) *tailnet.Authority {
		store := config.ProfileStore{Path: t.TempDir(), Secrets: testSecrets{}}
		privateRaw, decodeErr := base64.RawURLEncoding.DecodeString(private)
		if decodeErr != nil {
			t.Fatal(decodeErr)
		}
		if updateErr := store.UpdatePeerNetworkState(fixture.Issuer, binding.AccountID, binding.EndpointID, func(state *config.PeerNetworkState) error {
			state.PrivateKey = append([]byte(nil), privateRaw...)
			state.KeyGeneration = binding.KeyGeneration
			return nil
		}); updateErr != nil {
			t.Fatal(updateErr)
		}
		clear(privateRaw)
		authority, createErr := tailnet.NewAuthority(tailnet.AuthorityOptions{Store: store, Issuer: fixture.Issuer, Self: binding, Keys: keys})
		if createErr != nil {
			t.Fatal(createErr)
		}
		if applyErr := authority.Apply(t.Context(), token); applyErr != nil {
			t.Fatalf("apply signed network configuration for %s/%s: %v", binding.Role, binding.EndpointID, applyErr)
		}
		t.Cleanup(func() { _ = authority.Close() })
		return authority
	}
	makeTLS := func(certValue, privateValue string) *tls.Config {
		certDER, certErr := base64.RawURLEncoding.DecodeString(certValue)
		privateRaw, keyErr := base64.RawURLEncoding.DecodeString(privateValue)
		if certErr != nil || keyErr != nil {
			t.Fatal("invalid fixture TLS material")
		}
		leaf, parseErr := x509.ParseCertificate(certDER)
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		return &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{certDER}, PrivateKey: ed25519.PrivateKey(privateRaw), Leaf: leaf}}, MinVersion: tls.VersionTLS13, NextProtos: []string{peerquic.ALPN}, InsecureSkipVerify: true}
	}
	ownerAuthority := makeAuthority(fixture.CLI, fixture.CLIPrivate, fixture.OwnerConfiguration)
	// Consume signed projections in issuance order: the later broad snapshot
	// has already withdrawn the viewer grant and cannot authorize this phase.
	sharedConfiguration, machineConfiguration := fixture.TerminalViewerConfiguration, fixture.TerminalViewerMachineConfiguration
	sharedAuthority := makeAuthority(fixture.SharedCLI, fixture.SharedCLIPrivate, sharedConfiguration)
	machineAuthority := makeAuthority(fixture.Machine, fixture.MachinePrivate, machineConfiguration)
	owner, _ := native.NewOwner(native.Config{Authority: ownerAuthority, TLS: makeTLS(fixture.OwnerTLSCert, fixture.OwnerTLSPrivate)})
	shared, _ := native.NewOwner(native.Config{Authority: sharedAuthority, TLS: makeTLS(fixture.SharedTLSCert, fixture.SharedTLSPrivate)})
	machine, _ := native.NewOwner(native.Config{Authority: machineAuthority, TLS: makeTLS(fixture.MachineTLSCert, fixture.MachineTLSPrivate)})
	defer owner.Close()
	defer shared.Close()
	defer machine.Close()

	root := t.TempDir()
	ptyAdapter, err := pty.NewAdapter(root)
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := session.NewManager(session.ManagerConfig{Launch: func(command pty.Command) (session.PTYProcess, error) { return ptyAdapter.Start(command) }, MaxSessions: 3, MaxAttachments: 4, MaxInputDecisions: 32})
	if err != nil {
		t.Fatal(err)
	}
	executions, err := execprocess.New(execprocess.Config{WorkspaceRoot: root, BaseEnvironment: []string{"HOME=" + root, "PATH=/usr/bin:/bin", "LANG=C"}, MaximumActive: 3, MaximumOperations: 8, ReplayBytes: 64 << 10})
	if err != nil {
		t.Fatal(err)
	}
	readiness := health.New("shared-native", []string{"terminal.v1", "health.v1", "exec.v1"}, nil)
	readiness.Set("terminal.v1", health.Ready, "", 0)
	readiness.Set("health.v1", health.Ready, "", 0)
	readiness.Set("exec.v1", health.Ready, "", 0)
	// This transport consumer does not run an audit control plane; Task43 owns connected join recording.
	dispatcher, err := hostserver.NewDispatcher(hostserver.DispatcherConfig{RecordTerminalJoin: func(context.Context, hostserver.TerminalJoin) error { return nil }, Sessions: sessions, Health: readiness, SessionLauncher: sliceLauncher{sessions: sessions, shell: "/bin/sh", root: root}, WorkspaceRoot: root, Random: strings.NewReader(strings.Repeat("r", 256)), Exec: executions})
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
	revocations := auth.NewRevocationCache()
	verifier := auth.Verifier{Keys: liveCredentialKeys{fixture.KeyID: ed25519.PublicKey(publicRaw)}, Clock: liveCredentialClock{}, Revocations: revocations, ClockSkew: time.Minute}
	authorizer, err := hostruntime.NewCredentialAuthorizer(hostruntime.CredentialAuthConfig{Issuer: fixture.Issuer, EnvironmentID: fixture.EnvironmentID, MachineID: fixture.Machine.EndpointID, HelperID: fixture.HelperID, Verifier: verifier, Revocations: revocations})
	if err != nil {
		t.Fatal(err)
	}
	association, err := hostserver.NewNativeAssociationManager(hostserver.NativeAssociationConfig{Server: protocolServer, Authorizer: authorizer, Limiter: mustConnectionLimiter(t, 4)})
	if err != nil {
		t.Fatal(err)
	}
	var terminalAuth map[string]any
	if !terminalOnly {
		terminalAuth = fixture.TerminalDescriptor.Terminal["auth"].(map[string]any)
	}
	streamAuthorize := peerrelay.CredentialStreamAuthorizer(authorizer)
	var sshHost *managedssh.Host
	var sshIdentity, sshKnownHosts, sshRemoteFile string
	sshAvailable := true
	if terminalOnly {
		sshAvailable = false
	}
	for _, executable := range []string{"ssh", "sshd", "ssh-keygen", "nc", "scp", "sftp", "rsync"} {
		if _, lookupErr := exec.LookPath(executable); lookupErr != nil {
			sshAvailable = false
		}
	}
	if sshAvailable {
		sshdPort, identity, knownHosts, remoteFile := startServerIssuedTestSSHD(t, fixture)
		sshIdentity, sshKnownHosts = identity, knownHosts
		sshRemoteFile = remoteFile
		sshHost, err = managedssh.NewHost(managedssh.HostConfig{MaxStreams: 4, ProbeTimeout: time.Second, DialTimeout: time.Second})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = sshHost.ReconcileTarget(t.Context(), 1, sshdPort); err != nil {
			t.Fatal(err)
		}
	}
	filePayload := []byte("server-issued-native-file")
	fileDigest := sha256.Sum256(filePayload)
	transferRoot := t.TempDir()
	durable, err := store.Open(t.Context(), store.Config{Root: filepath.Join(transferRoot, "state")})
	if err != nil {
		t.Fatal(err)
	}
	defer durable.Close()
	transferService, err := hosttransfer.New(hosttransfer.Config{Root: filepath.Join(transferRoot, "spool"), PublishRoot: filepath.Join(transferRoot, "inbox"), LocalMachineID: fixture.Machine.EndpointID, Store: durable})
	if err != nil {
		t.Fatal(err)
	}
	transferHandler, err := hostserver.NewNativeFileTransferHandler(hostserver.FileTransferHandlerConfig{Service: transferService, Journal: journal, Authorizer: authorizer, AuthorizeCreate: func(authorization hostserver.Authorization, request hostserver.CreateFileTransferRequest) bool {
		return authorization.MachineID == fixture.Machine.EndpointID && authorization.UserID == fixture.FileDescriptor.InitiatingUserID && authorization.SourceMachineID == fixture.FileDescriptor.SourceMachineID && request.SourceMachineID == authorization.SourceMachineID && request.DestinationMachineID == authorization.MachineID && request.InitiatingUserID == authorization.UserID && request.SessionID == "" && authorization.RequestID != "" && authorization.IdempotencyKey == request.BatchID && authorization.RequestHash == hostserver.FileTransferManifestDigest(request.Files)
	}})
	if err != nil {
		t.Fatal(err)
	}
	service, err := nativesession.New(nativesession.Config{Authorize: func(ctx context.Context, header streamauth.Header) (string, error) {
		authorization, authorizeErr := streamAuthorize(ctx, header)
		if authorizeErr != nil || authorization.ResourceID == "" {
			return "", errors.Join(tailnet.ErrAdmission, authorizeErr)
		}
		return authorization.ResourceID, nil
	}, ServeStream: func(ctx context.Context, header streamauth.Header, connection net.Conn) error {
		if header.Consumer == "ssh" && sshHost != nil {
			_, serveErr := sshHost.Serve(ctx, 1, connection)
			return serveErr
		}
		serveErr := association.Serve(connection)
		if terminalOnly && serveErr != nil {
			t.Logf("terminal association ended: %v", serveErr)
		}
		return serveErr
	}, ServeTransfer: func(ctx context.Context, connection net.Conn) error {
		return hostserver.ServeHTTPConnection(ctx, connection, transferHandler)
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("IN_TS_TEST", "true")
	dm := integration.RunDERPAndSTUN(t, func(string, ...any) {}, "127.0.0.1")
	serverUDP, err := machineAuthority.Listen(dm.Regions[1])
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = machine.Listen(t.Context(), dm.Regions[1], service.Serve) }()
	descriptor := serverUDP.Address()
	if _, err = sharedAuthority.Peer(fixture.Machine.EndpointID); err != nil {
		t.Fatalf("resolve shared machine peer: %v", err)
	}
	if _, err = sharedAuthority.Client(descriptor, fixture.Machine.EndpointID); err != nil {
		t.Fatalf("bind shared machine endpoint: %v", err)
	}
	if err = sharedAuthority.PrepareRegional(t.Context(), fixture.Machine.EndpointID); err != nil {
		t.Fatalf("prepare shared machine route: %v", err)
	}
	sharedSession, err := shared.Dial(t.Context(), descriptor, fixture.Machine.EndpointID, peerquic.ClassInteractive)
	if err != nil {
		t.Fatalf("dial shared machine: %v", err)
	}
	ownerSession, err := owner.Dial(t.Context(), descriptor, fixture.Machine.EndpointID, peerquic.ClassInteractive)
	if err != nil {
		t.Fatal(err)
	}
	execOnce := func(session *native.Session, descriptor serverIssuedDescriptor, marker string) {
		tunnelAdapter := tunnel.TailnetTerminalTunnel{DialSession: func(context.Context, resolver.ConnectInfo) (*native.Session, error) { return session, nil }}
		auth := resolver.AuthTarget{Token: descriptor.Auth["token"].(string), ResourceID: descriptor.Auth["access_session_id"].(string), ExpiresAt: descriptor.ExpiresAt.Format(time.RFC3339)}
		connection, dialErr := tunnelAdapter.DialExec(t.Context(), resolver.ConnectInfo{Terminal: &resolver.TerminalTarget{EnvironmentID: fixture.EnvironmentID, CWD: root, Auth: auth}}, tunnel.ExecRequest{OperationID: descriptor.OperationID, Argv: []string{"/bin/sh", "-c", "printf " + marker}, CWD: root})
		if dialErr != nil {
			t.Fatalf("exec %s dial: %v", marker, dialErr)
		}
		var stdout strings.Builder
		for event := range connection.Events() {
			if event.Stream == "stdout" {
				stdout.Write(event.Data)
			}
		}
		if code, waitErr := connection.Wait(); waitErr != nil || code != 0 || stdout.String() != marker {
			t.Fatalf("exec %s code=%d err=%v stdout=%q", marker, code, waitErr, stdout.String())
		}
	}
	ownerTunnel := tunnel.TailnetTerminalTunnel{DialSession: func(context.Context, resolver.ConnectInfo) (*native.Session, error) { return ownerSession, nil }}
	var activeOwner tunnel.ExecConn
	if !terminalOnly {
		execOnce(sharedSession, fixture.ExecDescriptor, "shared")
		ownerAuth := resolver.AuthTarget{Token: fixture.OwnerExecDescriptor.Auth["token"].(string), ResourceID: fixture.OwnerExecDescriptor.Auth["access_session_id"].(string), ExpiresAt: fixture.OwnerExecDescriptor.ExpiresAt.Format(time.RFC3339)}
		activeOwner, err = ownerTunnel.DialExec(t.Context(), resolver.ConnectInfo{Terminal: &resolver.TerminalTarget{EnvironmentID: fixture.EnvironmentID, CWD: root, Auth: ownerAuth}}, tunnel.ExecRequest{OperationID: fixture.OwnerExecDescriptor.OperationID, Argv: []string{"/bin/sh", "-c", "while :; do printf owner-alive; sleep 5; done"}, CWD: root})
		if err != nil {
			t.Fatalf("start owner preservation stream: %v", err)
		}
		for event := range activeOwner.Events() {
			if event.State == "started" {
				break
			}
		}
	}
	sharedTunnel := tunnel.TailnetTerminalTunnel{DialSession: func(context.Context, resolver.ConnectInfo) (*native.Session, error) { return sharedSession, nil }}
	terminalTargetFor := func(descriptor serverIssuedTerminalDescriptor, attachment string, scopes []string, restart bool) *resolver.TerminalTarget {
		auth := descriptor.Terminal["auth"].(map[string]any)
		return &resolver.TerminalTarget{EnvironmentID: fixture.EnvironmentID, SessionID: descriptor.Terminal["session_id"].(string), CWD: root, Cols: 80, Rows: 24, RestartIfNotRunning: restart, InputAttachmentID: attachment, Auth: resolver.AuthTarget{Token: auth["token"].(string), ResourceID: auth["access_session_id"].(string), ExpiresAt: descriptor.ExpiresAt.Format(time.RFC3339), Scopes: scopes}}
	}
	ownerSharedSessionID := fixture.TerminalOwnerDescriptor.Terminal["session_id"].(string)
	if _, err = sessions.Create(t.Context(), session.CreateRequest{ID: ownerSharedSessionID, Name: "native-shared", Command: pty.Command{Path: "/bin/sh", Args: []string{"-c", "printf prejoin; while IFS= read -r line; do printf 'input:%s\\n' \"$line\"; done"}, Env: []string{"HOME=" + root, "PATH=/usr/bin:/bin", "TERM=xterm"}, CWD: root, Dimensions: pty.Dimensions{Columns: 80, Rows: 24}}}); err != nil {
		t.Fatal(err)
	}
	ownerSharedTarget := terminalTargetFor(fixture.TerminalOwnerDescriptor, "owner_shared_attachment", []string{"terminal:operate"}, false)
	ownerSharedConnection, err := ownerTunnel.Dial(t.Context(), resolver.ConnectInfo{Terminal: ownerSharedTarget})
	if err != nil {
		t.Fatalf("create owner shared terminal: %v", err)
	}
	defer ownerSharedConnection.Close()
	viewerTarget := terminalTargetFor(fixture.TerminalViewerDescriptor, "viewer_attachment", []string{"terminal:view"}, false)
	viewerAuth := fixture.TerminalViewerDescriptor.Terminal["auth"].(map[string]any)
	if _, verifyErr := verifier.Verify(t.Context(), viewerAuth["token"].(string), auth.Policy{Issuer: fixture.Issuer, Audience: "paperboat-machine", CredentialClass: "terminal_operation", Scopes: []string{"terminal:view"}, EnvironmentID: fixture.EnvironmentID, MachineID: fixture.Machine.EndpointID, SessionID: ownerSharedSessionID, MaxLifetime: 5 * time.Minute}); verifyErr != nil {
		t.Fatalf("viewer credential verification: %v", verifyErr)
	}
	viewerConnection, err := sharedTunnel.Dial(t.Context(), resolver.ConnectInfo{Terminal: viewerTarget})
	if err != nil {
		t.Fatalf("attach viewer terminal: %v", err)
	}
	defer viewerConnection.Close()
	if output := readTerminalUntil(t, viewerConnection, "prejoin"); !strings.Contains(output, "prejoin") {
		t.Fatalf("viewer pre-join replay=%q", output)
	}
	if _, err = viewerConnection.Write([]byte("viewer-denied\n")); err == nil {
		t.Fatal("viewer terminal input accepted")
	}
	if err = revocations.Replace([]string{fixture.TerminalViewerJTI}); err != nil {
		t.Fatal(err)
	}
	revokedRead := make(chan error, 1)
	go func() { buffer := make([]byte, 1); _, readErr := viewerConnection.Read(buffer); revokedRead <- readErr }()
	select {
	case readErr := <-revokedRead:
		if readErr == nil {
			t.Fatal("revoked viewer remained readable")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("revoked viewer stream was not terminated")
	}
	if err = machineAuthority.Apply(t.Context(), fixture.TerminalInteractiveMachineConfiguration); err != nil {
		t.Fatal(err)
	}
	if err = sharedAuthority.Apply(t.Context(), fixture.TerminalInteractiveConfiguration); err != nil {
		t.Fatal(err)
	}
	_ = sharedSession.Close()
	sharedSession, err = shared.Dial(t.Context(), descriptor, fixture.Machine.EndpointID, peerquic.ClassInteractive)
	if err != nil {
		t.Fatalf("redial interactive machine: %v", err)
	}
	sharedTunnel = tunnel.TailnetTerminalTunnel{DialSession: func(context.Context, resolver.ConnectInfo) (*native.Session, error) { return sharedSession, nil }}
	interactiveTarget := terminalTargetFor(fixture.TerminalInteractiveDescriptor, "interactive_attachment_a", []string{"terminal:control"}, false)
	interactiveA, err := sharedTunnel.Dial(t.Context(), resolver.ConnectInfo{Terminal: interactiveTarget})
	if err != nil {
		t.Fatalf("attach interactive terminal A: %v", err)
	}
	defer interactiveA.Close()
	writes := make(chan error, 2)
	go func() { _, writeErr := interactiveA.Write([]byte("alpha\n")); writes <- writeErr }()
	go func() { _, writeErr := ownerSharedConnection.Write([]byte("beta\n")); writes <- writeErr }()
	if first, second := <-writes, <-writes; first != nil || second != nil {
		t.Fatalf("concurrent interactive input errors: %v, %v", first, second)
	}
	ownerOutput := readTerminalUntil(t, ownerSharedConnection, "input:alpha", "input:beta")
	if !strings.Contains(ownerOutput, "alpha") || !strings.Contains(ownerOutput, "beta") {
		t.Fatalf("owner live concurrent output=%q", ownerOutput)
	}
	if terminalOnly {
		succeeded = true
		return
	}
	// These newer production projections revoke the shared session grant while
	// retaining independent machine capabilities and unrelated owner streams.
	if err = machineAuthority.Apply(t.Context(), fixture.MachineShared); err != nil {
		t.Fatalf("advance machine to broad capability projection: %v", err)
	}
	if err = sharedAuthority.Apply(t.Context(), fixture.SharedConfiguration); err != nil {
		t.Fatalf("advance shared client to broad capability projection: %v", err)
	}
	_ = sharedSession.Close()
	sharedSession, err = shared.Dial(t.Context(), descriptor, fixture.Machine.EndpointID, peerquic.ClassInteractive)
	if err != nil {
		t.Fatalf("redial independent machine capabilities: %v", err)
	}
	sharedTunnel = tunnel.TailnetTerminalTunnel{DialSession: func(context.Context, resolver.ConnectInfo) (*native.Session, error) { return sharedSession, nil }}
	terminalTarget := &resolver.TerminalTarget{EnvironmentID: fixture.EnvironmentID, SessionID: fixture.TerminalDescriptor.Terminal["session_id"].(string), CWD: root, Cols: 80, Rows: 24, RestartIfNotRunning: true, InputAttachmentID: "shared_attachment", Auth: resolver.AuthTarget{Token: terminalAuth["token"].(string), ResourceID: terminalAuth["access_session_id"].(string), ExpiresAt: fixture.TerminalDescriptor.ExpiresAt.Format(time.RFC3339)}}
	terminalConnection, err := sharedTunnel.Dial(t.Context(), resolver.ConnectInfo{Terminal: terminalTarget})
	if err != nil {
		t.Fatal(err)
	}
	if output := readTerminalUntil(t, terminalConnection, "ready"); !strings.Contains(output, "ready") {
		t.Fatalf("shared terminal output=%q", output)
	}
	_ = terminalConnection.Close()
	fileSession, err := shared.Dial(t.Context(), descriptor, fixture.Machine.EndpointID, peerquic.ClassTransfer)
	if err != nil {
		t.Fatal(err)
	}
	fileExpiry, _ := time.Parse(time.RFC3339, fixture.FileDescriptor.Auth["expires_at"].(string))
	fileClient, err := clienttransfer.NewNativeClient(fixture.FileDescriptor.Endpoint, clienttransfer.Auth{Token: fixture.FileDescriptor.Auth["token"].(string), ExpiresAt: fileExpiry}, clienttransfer.Binding{SourceMachineID: fixture.FileDescriptor.SourceMachineID, DestinationMachineID: fixture.FileDescriptor.DestinationMachineID, InitiatingUserID: fixture.FileDescriptor.InitiatingUserID}, func(ctx context.Context) (net.Conn, error) {
		header, headerErr := streamauth.New("operation_shared_file", "file_transfer", "stream_shared_file", fixture.FileDescriptor.Auth["token"].(string), fileExpiry, 1<<20)
		if headerErr != nil {
			return nil, headerErr
		}
		return fileSession.OpenAuthorized(ctx, header, fixture.FileDescriptor.Auth["access_session_id"].(string), "file_transfer")
	})
	if err != nil {
		t.Fatal(err)
	}
	if fixture.FileBatchID == "" {
		t.Fatal("server-issued Inbox batch binding is required")
	}
	batch, err := fileClient.SendBatch(t.Context(), fixture.FileBatchID, "", []clienttransfer.Source{{Basename: "shared.txt", Size: int64(len(filePayload)), SHA256: fileDigest, Reader: bytes.NewReader(filePayload)}})
	if err != nil {
		t.Fatal(err)
	}
	if len(batch.Transfers) != 1 || batch.Transfers[0].State != "published" || batch.Transfers[0].ResultCode != "published" || len(batch.Paths) != 1 {
		t.Fatalf("shared file batch=%#v", batch)
	}
	received, readErr := os.ReadFile(filepath.Join(transferRoot, "inbox", "shared.txt"))
	if readErr != nil || string(received) != string(filePayload) {
		t.Fatalf("shared file=%q err=%v", received, readErr)
	}
	if sshAvailable {
		sshSession, sshDialErr := shared.Dial(t.Context(), descriptor, fixture.Machine.EndpointID, peerquic.ClassInteractive)
		if sshDialErr != nil {
			t.Fatalf("dial managed SSH native session: %v", sshDialErr)
		}
		sshAdapter := tunnel.TailnetTerminalTunnel{DialSession: func(context.Context, resolver.ConnectInfo) (*native.Session, error) { return sshSession, nil }}
		sshInfo := resolver.ConnectInfo{Terminal: &resolver.TerminalTarget{Auth: resolver.AuthTarget{Token: fixture.SSHDescriptor.Auth["token"].(string), ResourceID: fixture.SSHDescriptor.Auth["access_session_id"].(string), ExpiresAt: fixture.SSHDescriptor.ExpiresAt.Format(time.RFC3339)}}}
		probe, probeErr := sshAdapter.DialSSH(t.Context(), sshInfo, fixture.SSHDescriptor.OperationID)
		if probeErr != nil {
			t.Fatalf("authorize server-issued managed SSH stream: %v", probeErr)
		}
		_ = probe.Close()
		proxyAddress, stopProxy := startNativeSSHProxy(t, t.Context(), sshAdapter, sshInfo, fixture.SSHDescriptor.OperationID)
		defer stopProxy()
		proxyHost, proxyPort, splitErr := net.SplitHostPort(proxyAddress)
		if splitErr != nil {
			t.Fatal(splitErr)
		}
		proxyCommand := filepath.Join(t.TempDir(), "proxy")
		if err = os.WriteFile(proxyCommand, []byte("#!/bin/sh\nexec nc "+proxyHost+" "+proxyPort+"\n"), 0700); err != nil {
			t.Fatal(err)
		}
		current, _ := user.Current()
		common := []string{"-F", "/dev/null", "-o", "StrictHostKeyChecking=yes", "-o", "UserKnownHostsFile=" + sshKnownHosts, "-o", "BatchMode=yes", "-o", "IdentitiesOnly=yes", "-i", sshIdentity, "-o", "ProxyCommand=" + proxyCommand}
		if output := runOpenSSH(t, t.Context(), "ssh", nil, append(common, current.Username+"@paperboat", "printf shared-managed-ssh")...); string(output) != "shared-managed-ssh" {
			t.Fatalf("shared managed SSH output=%q", output)
		}
		target := current.Username + "@paperboat"
		upload := filepath.Join(t.TempDir(), "shared-upload")
		if err = os.WriteFile(upload, []byte("shared-managed-file"), 0o600); err != nil {
			t.Fatal(err)
		}
		runOpenSSH(t, t.Context(), "scp", nil, append(append([]string{}, common...), upload, target+":"+sshRemoteFile)...)
		download := filepath.Join(t.TempDir(), "shared-download")
		runOpenSSH(t, t.Context(), "sftp", []byte("get "+sshRemoteFile+" "+download+"\n"), append(append([]string{}, common...), "-b", "-", target)...)
		if content, readErr := os.ReadFile(download); readErr != nil || string(content) != "shared-managed-file" {
			t.Fatalf("shared SFTP download=%q error=%v", content, readErr)
		}
		sshWrapper := filepath.Join(t.TempDir(), "ssh-wrapper")
		wrapper := "#!/bin/sh\nexec ssh -F /dev/null -o StrictHostKeyChecking=yes -o UserKnownHostsFile=" + sshKnownHosts + " -o BatchMode=yes -o IdentitiesOnly=yes -i " + sshIdentity + " -o ProxyCommand=" + proxyCommand + " \"$@\"\n"
		if err = os.WriteFile(sshWrapper, []byte(wrapper), 0o700); err != nil {
			t.Fatal(err)
		}
		rsyncDownload := filepath.Join(t.TempDir(), "shared-rsync-download")
		runOpenSSH(t, t.Context(), "rsync", nil, "-e", sshWrapper, target+":"+sshRemoteFile, rsyncDownload)
		if content, readErr := os.ReadFile(rsyncDownload); readErr != nil || string(content) != "shared-managed-file" {
			t.Fatalf("shared rsync download=%q error=%v", content, readErr)
		}
	}
	if err := machineAuthority.Apply(t.Context(), fixture.MachineDeviceDisabled); err != nil {
		t.Fatal(err)
	}
	if err := sharedAuthority.Apply(t.Context(), fixture.SharedDeviceDisabled); err != nil {
		t.Fatal(err)
	}
	sharedSession, err = shared.Dial(t.Context(), descriptor, fixture.Machine.EndpointID, peerquic.ClassInteractive)
	if err != nil {
		t.Fatal(err)
	}
	disabledFileHeader, _ := streamauth.New("operation_disabled_file", "file_transfer", "stream_disabled_file", fixture.FileDescriptor.Auth["token"].(string), fileExpiry, 1024)
	if _, err := sharedSession.OpenAuthorized(t.Context(), disabledFileHeader, fixture.FileDescriptor.Auth["access_session_id"].(string), "file_transfer"); !errors.Is(err, tailnet.ErrAdmission) {
		t.Fatalf("disabled file reconnect error=%v", err)
	}
	execOnce(sharedSession, fixture.AfterDisableExecDescriptor, "shared-after-file-disable")
	terminalHeader, _ := streamauth.New("operation_denied_terminal", "terminal", "stream_denied_terminal", fixture.ExecDescriptor.Auth["token"].(string), fixture.ExecDescriptor.ExpiresAt, 1024)
	if _, err := sharedSession.OpenAuthorized(t.Context(), terminalHeader, fixture.ExecDescriptor.Auth["access_session_id"].(string), "terminal"); !errors.Is(err, tailnet.ErrAdmission) {
		t.Fatalf("ungranted terminal error=%v", err)
	}
	sharedAuth := resolver.AuthTarget{Token: fixture.ActiveExecDescriptor.Auth["token"].(string), ResourceID: fixture.ActiveExecDescriptor.Auth["access_session_id"].(string), ExpiresAt: fixture.ActiveExecDescriptor.ExpiresAt.Format(time.RFC3339)}
	activeShared, err := sharedTunnel.DialExec(t.Context(), resolver.ConnectInfo{Terminal: &resolver.TerminalTarget{EnvironmentID: fixture.EnvironmentID, CWD: root, Auth: sharedAuth}}, tunnel.ExecRequest{OperationID: fixture.ActiveExecDescriptor.OperationID, Argv: []string{"/bin/sh", "-c", "sleep 30"}, CWD: root})
	if err != nil {
		t.Fatal(err)
	}
	for event := range activeShared.Events() {
		if event.State == "started" {
			break
		}
	}
	if err := machineAuthority.Apply(t.Context(), fixture.MachineRevoked); err != nil {
		t.Fatal(err)
	}
	if err := sharedAuthority.Apply(t.Context(), fixture.SharedRevoked); err != nil {
		t.Fatal(err)
	}
	sharedEnded := make(chan error, 1)
	go func() {
		for range activeShared.Events() {
		}
		_, waitErr := activeShared.Wait()
		sharedEnded <- waitErr
	}()
	select {
	case <-sharedEnded:
	case <-time.After(5 * time.Second):
		t.Fatal("revoked shared exec remained active")
	}
	select {
	case _, open := <-activeOwner.Events():
		if !open {
			t.Fatal("unrelated active owner exec closed with shared revocation")
		}
	case <-time.After(6 * time.Second):
		t.Fatal("unrelated active owner exec stopped producing output")
	}
	_ = activeOwner.Close()
	if _, err := shared.Dial(t.Context(), descriptor, fixture.Machine.EndpointID, peerquic.ClassInteractive); !errors.Is(err, tailnet.ErrAdmission) {
		t.Fatalf("revoked reconnect error=%v", err)
	}
	if err := machineAuthority.Apply(t.Context(), fixture.FinalMachineConfiguration); err != nil {
		t.Fatalf("apply machine config after issuer departure: %v", err)
	}
	if err := sharedAuthority.Apply(t.Context(), fixture.FinalSharedConfiguration); err != nil {
		t.Fatalf("apply shared config after issuer departure: %v", err)
	}
	if err := ownerAuthority.Apply(t.Context(), fixture.RemovedOwnerConfiguration); err != nil {
		t.Fatalf("apply removed issuer config: %v", err)
	}
	finalSharedSession, err := shared.Dial(t.Context(), descriptor, fixture.Machine.EndpointID, peerquic.ClassInteractive)
	if err != nil {
		t.Fatalf("shared reconnect after issuer departure: %v", err)
	}
	execOnce(finalSharedSession, fixture.FinalSharedExecDescriptor, "shared-after-issuer-left")
	if _, err := owner.Dial(t.Context(), descriptor, fixture.Machine.EndpointID, peerquic.ClassInteractive); !errors.Is(err, tailnet.ErrAdmission) {
		t.Fatalf("removed issuer reconnect error=%v", err)
	}
	succeeded = true
}

func startServerIssuedTestSSHD(t *testing.T, fixture serverIssuedSharedFixture) (uint16, string, string, string) {
	t.Helper()
	root := t.TempDir()
	current, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	hostPrivate, hostErr := base64.RawURLEncoding.DecodeString(fixture.SSHHostPrivateKey)
	clientPrivate, clientErr := base64.RawURLEncoding.DecodeString(fixture.SSHClientPrivateKey)
	if hostErr != nil || clientErr != nil || fixture.SSHHostPublicKey == "" || fixture.SSHClientPublicKey == "" {
		t.Fatal("invalid server-issued SSH key fixture")
	}
	defer clear(hostPrivate)
	defer clear(clientPrivate)
	hostKey := filepath.Join(root, "host_ed25519")
	identity := filepath.Join(root, "id_ed25519")
	authorized := filepath.Join(root, "authorized_keys")
	knownHosts := filepath.Join(root, "known_hosts")
	for path, value := range map[string][]byte{hostKey: hostPrivate, identity: clientPrivate, authorized: []byte(fixture.SSHClientPublicKey + "\n"), knownHosts: []byte("paperboat " + fixture.SSHHostPublicKey + "\n")} {
		if err = os.WriteFile(path, value, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	probe, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := uint16(probe.Addr().(*net.TCPAddr).Port)
	_ = probe.Close()
	configPath := filepath.Join(root, "sshd_config")
	configText := fmt.Sprintf("ListenAddress 127.0.0.1\nPort %d\nHostKey %s\nPidFile %s\nAuthorizedKeysFile %s\nStrictModes no\nPasswordAuthentication no\nKbdInteractiveAuthentication no\nUsePAM no\nAllowUsers %s\nSubsystem sftp internal-sftp\nLogLevel ERROR\n", port, hostKey, filepath.Join(root, "sshd.pid"), authorized, current.Username)
	if err = os.WriteFile(configPath, []byte(configText), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	sshdPath, err := exec.LookPath("sshd")
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	command := exec.CommandContext(ctx, sshdPath, "-D", "-e", "-f", configPath)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err = command.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); _ = command.Wait() })
	deadline := time.Now().Add(3 * time.Second)
	for {
		connection, dialErr := net.DialTimeout("tcp4", fmt.Sprintf("127.0.0.1:%d", port), 50*time.Millisecond)
		if dialErr == nil {
			_ = connection.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("server-issued sshd readiness: %v: %s", dialErr, stderr.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	return port, identity, knownHosts, filepath.Join(root, "remote-file")
}

func mustConnectionLimiter(t *testing.T, maximum int) *hostserver.ConnectionLimiter {
	t.Helper()
	limiter, err := hostserver.NewConnectionLimiter(maximum)
	if err != nil {
		t.Fatal(err)
	}
	return limiter
}

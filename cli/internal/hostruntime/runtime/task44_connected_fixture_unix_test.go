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
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	clienttransfer "github.com/pinksaucepasta/paperboat/internal/filetransfer"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/auth"
	hostconfig "github.com/pinksaucepasta/paperboat/internal/hostruntime/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/configsync"
	hosttransfer "github.com/pinksaucepasta/paperboat/internal/hostruntime/filetransfer"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/health"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/operation"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/pty"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/server"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/session"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/store"
)

type task44RuntimeKeys struct {
	id  string
	key ed25519.PublicKey
}

func (k task44RuntimeKeys) Lookup(_ context.Context, id string) (ed25519.PublicKey, bool, error) {
	return k.key, id == k.id, nil
}
func (task44RuntimeKeys) Refresh(context.Context) error { return nil }

type task44ConfigTransport struct {
	base http.RoundTripper
	t    *testing.T
}

func (tr task44ConfigTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	response, err := tr.base.RoundTrip(req)
	if req.URL.Hostname() == "127.0.0.1" && (err != nil || response.StatusCode >= 400) {
		status := 0
		if response != nil {
			status = response.StatusCode
		}
		tr.t.Logf("config control request %s status=%d transport_error=%t", req.URL.Path, status, err != nil)
	}
	return response, err
}

type task44RuntimeClock struct{}

func (task44RuntimeClock) Now() time.Time { return time.Now().UTC() }

type task44RuntimePolicy struct{ policy auth.Policy }

func (p task44RuntimePolicy) Policy(frame protocol.Frame) (auth.Policy, error) {
	if p.policy.CredentialClass == "file_transfer" {
		if frame.Capability != "file-transfer.v1" {
			return auth.Policy{}, server.ErrCredentialPolicy
		}
		return p.policy, nil
	}
	if frame.Capability != "terminal.v1" && frame.Capability != "health.v1" {
		return auth.Policy{}, server.ErrCredentialPolicy
	}
	return p.policy, nil
}

// This opt-in consumer uses production signed credentials, dispatcher and join
// observations. Its TLS helper endpoint permits the producer to inspect the same
// actual PTY before granting access. Revocation checks exercise reconnect, not a
// synthetic replacement for the native network projection refresh loop.
func TestTask44ConnectedRuntime(t *testing.T) {
	dir := os.Getenv("PAPERBOAT_TASK44_DIR")
	if dir == "" {
		t.Skip("Task44 connected runtime is opt-in")
	}
	info, err := os.Stat(dir)
	if err != nil || !filepath.IsAbs(dir) || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		t.Fatal("absolute private Task44 directory required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	read := func(name string, value any) error {
		path := filepath.Join(dir, name)
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 65536 {
			return errors.New("invalid private fixture file")
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		defer clear(raw)
		return json.Unmarshal(raw, value)
	}
	write := func(name string, value any) {
		t.Helper()
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal("fixture encoding failed")
		}
		defer clear(raw)
		temporary := filepath.Join(dir, name+".tmp")
		if err = os.WriteFile(temporary, raw, 0600); err != nil {
			t.Fatal("fixture write failed")
		}
		if err = os.Rename(temporary, filepath.Join(dir, name)); err != nil {
			t.Fatal("fixture publication failed")
		}
	}
	defer func() {
		for _, name := range []string{"runtime-ready.json", "runtime-ready.json.tmp", "result.json", "result.json.tmp"} {
			_ = os.Remove(filepath.Join(dir, name))
		}
	}()
	var in struct {
		task43JoinFixture
		Issuer     string `json:"issuer"`
		KeyID      string `json:"key_id"`
		PublicKey  string `json:"public_key"`
		OwnerToken string `json:"owner_token"`
	}
	if err = read("runtime.json", &in); err != nil {
		t.Fatal("runtime input unavailable or invalid")
	}
	proofKey, err := base64.RawURLEncoding.DecodeString(in.PrivateKey)
	if err != nil || len(proofKey) != ed25519.PrivateKeySize {
		t.Fatal("invalid helper proof key")
	}
	defer clear(proofKey)
	publicKey, err := base64.RawURLEncoding.DecodeString(in.PublicKey)
	if err != nil || len(publicKey) != ed25519.PublicKeySize {
		t.Fatal("invalid credential public key")
	}
	credentials := &task43JoinCredentials{fixture: in.task43JoinFixture, key: ed25519.PrivateKey(proofKey)}
	sender := &runtimeObservationSender{endpoint: in.Endpoint, tokens: credentials, proofs: credentials, operationID: func() (string, error) {
		var id [16]byte
		if _, err := rand.Read(id[:]); err != nil {
			return "", err
		}
		return "task44_join_" + hex.EncodeToString(id[:]), nil
	}, environmentID: in.EnvironmentID, machineID: in.MachineID, reporterVersion: "task44-test", client: &http.Client{Timeout: 10 * time.Second, CheckRedirect: rejectRuntimePolicyRedirect}}
	revocations := auth.NewRevocationCache()
	verifier := auth.Verifier{Keys: task44RuntimeKeys{in.KeyID, ed25519.PublicKey(publicKey)}, Clock: task44RuntimeClock{}, Revocations: revocations}
	policy := task44RuntimePolicy{auth.Policy{Issuer: in.Issuer, Audience: "paperboat-machine", CredentialClass: "terminal_operation", AnyScopes: [][]string{{"terminal:operate"}, {"terminal:view"}, {"terminal:control"}}, EnvironmentID: in.EnvironmentID, MachineID: in.MachineID, SessionID: in.TerminalSessionID, MaxLifetime: 5 * time.Minute}}
	authorize := func(token string) (server.Authorization, error) {
		authorizer := &server.CredentialAuthorizer{Token: token, Verifier: verifier, Resolver: policy, Revocations: revocations}
		defer authorizer.CloseAuthorization()
		return authorizer.Authorize(ctx, protocol.Frame{Capability: "terminal.v1"})
	}
	owner, err := authorize(in.OwnerToken)
	if err != nil || owner.TerminalRole != server.TerminalRoleOwner {
		t.Fatal("signed owner authorization failed")
	}
	root := t.TempDir()
	adapter, err := pty.NewAdapter(root)
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := session.NewManager(session.ManagerConfig{Launch: func(command pty.Command) (session.PTYProcess, error) { return adapter.Start(command) }, MaxSessions: 1, MaxAttachments: 8})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		clean, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		if err := sessions.Shutdown(clean); err != nil {
			t.Error("PTY cleanup failed")
		}
	}()
	snapshot, err := sessions.Create(ctx, session.CreateRequest{ID: in.TerminalSessionID, Name: "task44-connected", Command: pty.Command{Path: "/bin/sh", Args: []string{"-c", "printf 'task44-replay-marker\\n'; while IFS= read -r line; do printf 'task44-owner-alive\\n'; done"}, CWD: root, Env: []string{"PATH=/usr/bin:/bin", "TERM=xterm"}, Dimensions: pty.Dimensions{Columns: 80, Rows: 24}}})
	if err != nil {
		t.Fatal(err)
	}
	dispatcher, err := server.NewDispatcher(server.DispatcherConfig{Sessions: sessions, Health: health.New("task44", []string{"terminal.v1", "health.v1"}, nil), SessionLauncher: testSessionLauncher{sessions: sessions, path: "/bin/sh"}, WorkspaceRoot: root, Random: rand.Reader, RecordTerminalJoin: sender.RecordTerminalJoin})
	if err != nil {
		t.Fatal(err)
	}
	journal, err := operation.NewJournal(128)
	if err != nil {
		t.Fatal(err)
	}
	runtimeServer, err := server.New(server.Config{Negotiator: protocol.Negotiator{Profile: hostconfig.BYOD, Available: map[string]bool{"terminal.v1": true, "health.v1": true}}, Journal: journal, Handler: dispatcher, MaxConcurrent: 8, HeartbeatInterval: time.Hour, MutationDeadline: 15 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		clean, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		if err := runtimeServer.Shutdown(clean); err != nil {
			t.Error("runtime cleanup failed")
		}
	}()
	handler, err := server.NewWebSocketHandler(server.WebSocketHandlerConfig{Server: runtimeServer, MaxConnections: 8, Authorizer: func(token string) (server.Authorizer, error) {
		return &server.CredentialAuthorizer{Token: token, Verifier: verifier, Resolver: policy, Revocations: revocations}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/v1/runtime", handler)
	tlsServer := httptest.NewTLSServer(mux)
	defer tlsServer.Close()
	type attachment struct {
		authorization server.Authorization
		id            string
		stream        server.OutputStream
	}
	attachments := make([]attachment, 0, 8)
	defer func() {
		for _, a := range attachments {
			_ = a.stream.Close()
		}
	}()
	attach := func(authorization server.Authorization, id string) (server.OutputStream, string) {
		payload, _ := json.Marshal(map[string]any{"action": "attach", "session_id": in.TerminalSessionID, "attachment_id": id})
		outcome := dispatcher.Handle(ctx, authorization, "terminal.v1", payload)
		if outcome.ErrorCode != "" {
			if _, opened, err := dispatcher.OpenStream(ctx, authorization, "terminal.v1", payload, outcome, false); err != nil || opened {
				t.Fatal("denied attachment opened output")
			}
			return nil, outcome.ErrorCode
		}
		stream, opened, err := dispatcher.OpenStream(ctx, authorization, "terminal.v1", payload, outcome, false)
		if err != nil || !opened {
			t.Fatal("authorized attachment has no output stream")
		}
		return stream, ""
	}
	readMarker := func(stream server.OutputStream, marker string) {
		t.Helper()
		readCtx, done := context.WithTimeout(ctx, 5*time.Second)
		defer done()
		var output []byte
		defer func() { clear(output) }()
		for !bytes.Contains(output, []byte(marker)) {
			frame, err := stream.Next(readCtx)
			if err != nil {
				t.Fatal("expected bounded PTY output unavailable")
			}
			output = append(output, frame.Data...)
			if frame.Release != nil {
				frame.Release()
			}
			if len(output) > 64*1024 {
				t.Fatal("terminal replay exceeded bound")
			}
		}
	}
	ownerStream, code := attach(owner, "att_task44_owner")
	if code != "" {
		t.Fatal("owner attach failed")
	}
	attachments = append(attachments, attachment{owner, "att_task44_owner", ownerStream})
	readMarker(ownerStream, "task44-replay-marker")
	write("runtime-ready.json", map[string]string{"public_host": tlsServer.Listener.Addr().String(), "cert_pem": string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: tlsServer.Certificate().Raw}))})
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	var configWorker *configsync.Supervisor
	configRoot := t.TempDir()
	configHome := filepath.Join(configRoot, "home")
	if err := os.Mkdir(configHome, 0700); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if configWorker != nil {
			clean, done := context.WithTimeout(context.Background(), 30*time.Second)
			defer done()
			if err := configWorker.Shutdown(clean); err != nil {
				t.Error("config worker cleanup failed")
			}
		}
	}()
	lastID := ""
	var viewer *attachment
	for {
		select {
		case <-ctx.Done():
			t.Fatal("Task44 runtime exceeded five minutes")
		case <-ticker.C:
			var action struct {
				ID                  string `json:"id"`
				Token               string `json:"token"`
				Action              string `json:"action"`
				BatchID             string `json:"batch_id"`
				SourceMachineID     string `json:"source_machine_id"`
				InitiatingUserID    string `json:"initiating_user_id"`
				RequestID           string `json:"request_id"`
				OwnDevice           bool   `json:"own_device"`
				ConfigEndpoint      string `json:"config_endpoint"`
				ConfigCA            string `json:"config_ca_pem"`
				ConfigEnvironmentID string `json:"config_environment_id"`
				ConfigMachineID     string `json:"config_machine_id"`
				ConfigIdentityToken string `json:"config_identity_token"`
				ConfigPrivateKey    string `json:"config_proof_private_key_base64"`
				ConfigHelperID      string `json:"config_proof_helper_id"`
				ConfigGeneration    string `json:"config_installation_generation"`
				ConfigPath          string `json:"config_home_relative_path"`
				ChezmoiBinary       string `json:"chezmoi_binary"`
				Content             string `json:"content"`
				ExpectedError       string `json:"expected_error"`
			}
			if err := read("action.json", &action); errors.Is(err, os.ErrNotExist) {
				continue
			} else if err != nil {
				t.Fatal("invalid action file")
			}
			if action.ID == "" || action.ID == lastID {
				continue
			}
			lastID = action.ID
			if action.Action == "stop" {
				write("result.json", map[string]any{"id": action.ID, "ok": true, "error_code": ""})
				return
			}
			if strings.HasPrefix(action.Action, "config_") {
				code := ""
				switch action.Action {
				case "config_start":
					if configWorker != nil {
						code = "config_already_started"
						break
					}
					endpoint, err := url.Parse(action.ConfigEndpoint)
					roots, rootErr := x509.SystemCertPool()
					if rootErr != nil {
						t.Fatal("system trust roots unavailable")
					}
					if err != nil || endpoint.Scheme != "https" || endpoint.Hostname() != "127.0.0.1" || !roots.AppendCertsFromPEM([]byte(action.ConfigCA)) {
						code = "config_endpoint_invalid"
						break
					}
					key, err := base64.RawURLEncoding.DecodeString(action.ConfigPrivateKey)
					generation, genErr := strconv.ParseInt(action.ConfigGeneration, 10, 64)
					if err != nil || len(key) != ed25519.PrivateKeySize || genErr != nil || generation < 1 {
						code = "config_identity_invalid"
						break
					}
					defer clear(key)
					identity := &task43JoinCredentials{fixture: task43JoinFixture{EnvironmentID: action.ConfigEnvironmentID, MachineID: action.ConfigMachineID, IdentityToken: action.ConfigIdentityToken, InstallationGeneration: generation, ProofHelperID: action.ConfigHelperID}, key: ed25519.PrivateKey(key)}
					transport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}}
					defer transport.CloseIdleConnections()
					binary := action.ChezmoiBinary
					if binary == "" {
						binary = "/usr/local/bin/chezmoi"
					}
					configWorker, err = newProductionConfigSync(productionConfigSyncConfig{ControlURL: endpoint.String(), ControlHost: endpoint.Hostname(), RepositoryHosts: []string{"github.com"}, HomeRoot: configHome, StateRoot: filepath.Join(configRoot, "state"), ChezmoiBinary: binary, Identities: identity, Proofs: identity, OperationID: randomProductionOperationID, Transport: task44ConfigTransport{base: transport, t: t}})
					if err != nil {
						code = "config_construction_failed"
						break
					}
					if err := configWorker.Start(ctx); err != nil {
						code = "config_start_failed"
					}
				case "config_stop":
					if configWorker != nil {
						clean, done := context.WithTimeout(ctx, 30*time.Second)
						err := configWorker.Shutdown(clean)
						if action.ExpectedError == "review_required" {
							if !errors.Is(err, configsync.ErrReviewRequired) {
								code = "config_expected_review_stop_missing"
							}
						} else if action.ExpectedError != "" || err != nil {
							code = "config_stop_failed"
						}
						done()
						configWorker = nil
					}
				case "config_expect", "config_edit", "config_expect_absent":
					if action.ConfigPath != "task44-config.txt" || len(action.Content) > 1024 {
						code = "config_file_invalid"
						break
					}
					path := filepath.Join(configHome, action.ConfigPath)
					if action.Action == "config_expect_absent" {
						if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
							code = "config_applied_without_review"
						}
					} else if action.Action == "config_edit" {
						if err := os.WriteFile(path, []byte(action.Content), 0600); err != nil {
							code = "config_edit_failed"
						}
					} else {
						data, err := os.ReadFile(path)
						if err != nil || string(data) != action.Content {
							code = "config_content_mismatch"
						}
						clear(data)
					}
				default:
					code = "unknown_config_action"
				}
				write("result.json", map[string]any{"id": action.ID, "ok": code == "", "error_code": code})
				continue
			}
			if action.Action == "file" {
				code := task44TransferFile(t, ctx, verifier, revocations, in.Issuer, in.EnvironmentID, in.MachineID, owner.AccountID, action.Token, action.BatchID, action.SourceMachineID, action.InitiatingUserID, action.RequestID, action.OwnDevice)
				write("result.json", map[string]any{"id": action.ID, "ok": code == "", "error_code": code})
				continue
			}
			authorization, err := authorize(action.Token)
			if err != nil {
				write("result.json", map[string]any{"id": action.ID, "ok": false, "error_code": "credential_rejected"})
				continue
			}
			code := ""
			switch action.Action {
			case "attach":
				if len(attachments) >= 8 {
					code = "attachment_limit"
					break
				}
				id := "att_task44_" + action.ID
				stream, failure := attach(authorization, id)
				code = failure
				if code == "" {
					attachments = append(attachments, attachment{authorization, id, stream})
					readMarker(stream, "task44-replay-marker")
					if authorization.TerminalRole == server.TerminalRoleViewer {
						viewer = &attachments[len(attachments)-1]
					}
				}
			case "viewer_input":
				if viewer == nil || authorization.TerminalRole != server.TerminalRoleViewer || authorization.AccountID != viewer.authorization.AccountID || authorization.ClientID != viewer.authorization.ClientID {
					code = "viewer_missing"
					break
				}
				if _, err := dispatcher.HandleTerminalInput(ctx, authorization, in.TerminalSessionID, viewer.id, snapshot.Generation, 1, []byte("must-not-execute\n")); !errors.Is(err, session.ErrInvalidInput) {
					code = "viewer_input_not_denied"
				}
			case "revoke_check":
				before, err := sessions.Snapshot(in.TerminalSessionID)
				if err != nil {
					t.Fatal("runtime snapshot unavailable")
				}
				stream, rejected := attach(authorization, "att_task44_revoked_"+action.ID)
				if stream != nil {
					_ = stream.Close()
				}
				if rejected != "unavailable" {
					code = "revoked_attach_not_denied"
					break
				}
				after, err := sessions.Snapshot(in.TerminalSessionID)
				if err != nil || len(after.Participants) != len(before.Participants) {
					code = "revoked_attach_leaked_participant"
					break
				}
				if _, err := dispatcher.HandleTerminalInput(ctx, owner, in.TerminalSessionID, "att_task44_owner", snapshot.Generation, 1, []byte("owner-still-usable\n")); err != nil {
					code = "owner_input_failed"
					break
				}
				readMarker(ownerStream, "task44-owner-alive")
			default:
				code = "unknown_action"
			}
			write("result.json", map[string]any{"id": action.ID, "ok": code == "", "error_code": code})
		}
	}
}

// The in-memory stream replaces only transport plumbing. Every HTTP operation
// verifies the actual server-signed file credential and native publication checks
// the approved manifest, chunk digest and completed file digest.
func task44TransferFile(t *testing.T, parent context.Context, verifier auth.Verifier, revocations *auth.RevocationCache, issuer, environmentID, machineID, ownerAccount, token, batchID, sourceID, actorID, requestID string, ownDevice bool) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(parent, 20*time.Second)
	var workers sync.WaitGroup
	defer cancel()
	root := t.TempDir()
	defer func() {
		if err := os.RemoveAll(root); err != nil {
			t.Error("file fixture cleanup failed")
		}
	}()
	durable, err := store.Open(ctx, store.Config{Root: filepath.Join(root, "state")})
	if err != nil {
		return "file_store_failed"
	}
	defer durable.Close()
	defer func() { cancel(); workers.Wait() }()
	policy := hosttransfer.Policy{Revision: "task44", MaxFileBytes: 1024, MaxBatchFiles: 1, MaxBatchBytes: 1024, MaxConcurrentTransfers: 1, RetentionSeconds: 60, DeliveryTimeoutSeconds: 20, MaxPendingSpoolBytes: 1024}
	if !policy.Valid() {
		t.Fatal("invalid bounded file fixture policy")
	}
	service, err := hosttransfer.New(hosttransfer.Config{Root: filepath.Join(root, "spool"), PublishRoot: filepath.Join(root, "inbox"), LocalMachineID: machineID, Store: durable, Policy: hosttransfer.NewPolicyStore(policy)})
	if err != nil {
		return "file_service_failed"
	}
	journal, err := operation.NewJournal(32)
	if err != nil {
		return "file_journal_failed"
	}
	resolver := task44RuntimePolicy{auth.Policy{Issuer: issuer, Audience: "paperboat-machine", CredentialClass: "file_transfer", Scopes: []string{"file:transfer"}, EnvironmentID: environmentID, MachineID: machineID, SourceMachineID: sourceID, UserID: actorID, MaxLifetime: 5 * time.Minute}}
	authorizer := func(token string) (server.Authorizer, error) {
		return &server.CredentialAuthorizer{Token: token, Verifier: verifier, Resolver: resolver, Revocations: revocations}, nil
	}
	initial, _ := authorizer(token)
	authorization, err := initial.Authorize(ctx, protocol.Frame{Capability: "file-transfer.v1"})
	if closer, ok := initial.(server.AuthorizationCloser); ok {
		closer.CloseAuthorization()
	}
	if err != nil {
		return "file_credential_rejected"
	}
	if ownDevice {
		if actorID != ownerAccount || requestID != "" || authorization.RequestID != "" || authorization.RequestHash != "" || authorization.IdempotencyKey != "" {
			return "own_device_binding_invalid"
		}
	} else if requestID == "" || authorization.RequestID != requestID || authorization.IdempotencyKey != batchID || authorization.RequestHash == "" {
		return "file_approval_binding_invalid"
	}
	handler, err := server.NewNativeFileTransferHandler(server.FileTransferHandlerConfig{Service: service, Journal: journal, Authorizer: authorizer, AuthorizeCreate: func(a server.Authorization, r server.CreateFileTransferRequest) bool {
		identity := a.MachineID == machineID && a.UserID == actorID && a.SourceMachineID == sourceID && r.SourceMachineID == a.SourceMachineID && r.DestinationMachineID == machineID && r.InitiatingUserID == a.UserID && r.SessionID == ""
		if !identity {
			return false
		}
		if ownDevice {
			return a.UserID == ownerAccount && a.RequestID == "" && a.RequestHash == "" && a.IdempotencyKey == ""
		}
		return a.RequestID == requestID && a.IdempotencyKey == r.BatchID && a.RequestHash == server.FileTransferManifestDigest(r.Files)
	}})
	if err != nil {
		return "file_handler_failed"
	}
	var opens atomic.Int32
	client, err := clienttransfer.NewNativeClient("https://task44.invalid/v1/file-transfers", clienttransfer.Auth{Token: token, ExpiresAt: authorization.ExpiresAt}, clienttransfer.Binding{SourceMachineID: sourceID, DestinationMachineID: machineID, InitiatingUserID: actorID}, func(context.Context) (net.Conn, error) {
		if opens.Add(1) > 32 {
			return nil, errors.New("file fixture connection bound exceeded")
		}
		client, host := net.Pipe()
		workers.Add(1)
		go func() { defer workers.Done(); defer host.Close(); _ = server.ServeHTTPConnection(ctx, host, handler) }()
		return client, nil
	})
	if err != nil {
		return "file_client_failed"
	}
	defer client.Close()
	payload := []byte("task44-approved-file")
	digest := sha256.Sum256(payload)
	result, err := client.SendBatch(ctx, batchID, "", []clienttransfer.Source{{Basename: "task44.txt", Size: int64(len(payload)), SHA256: digest, Reader: bytes.NewReader(payload)}})
	if err != nil {
		return "file_transfer_failed"
	}
	if len(result.Transfers) != 1 || result.Transfers[0].State != "published" || result.Transfers[0].ResultCode != "published" || len(result.Paths) != 1 {
		return "file_publication_failed"
	}
	received, err := os.ReadFile(filepath.Join(root, "inbox", "task44.txt"))
	defer clear(received)
	if err != nil || !bytes.Equal(received, payload) || sha256.Sum256(received) != digest {
		return "file_integrity_failed"
	}
	return ""
}

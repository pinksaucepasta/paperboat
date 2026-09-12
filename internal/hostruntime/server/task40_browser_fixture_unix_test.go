//go:build darwin || linux

package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/auth"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/health"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/operation"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/pty"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/session"
)

type task40BrowserKeys struct {
	id  string
	key ed25519.PublicKey
}

func (k task40BrowserKeys) Lookup(_ context.Context, id string) (ed25519.PublicKey, bool, error) {
	return k.key, id == k.id, nil
}
func (task40BrowserKeys) Refresh(context.Context) error { return nil }

type task40BrowserClock struct{}

func (task40BrowserClock) Now() time.Time { return time.Now().UTC() }

// This opt-in held fixture exposes the real PTY/dispatcher/WebSocket boundary.
// The control-plane fixture supplies an actual signed owner credential; no
// participant metadata or successful authorization is manufactured here.
func TestTask40HoldBrowserRuntime(t *testing.T) {
	input := os.Getenv("PAPERBOAT_TASK40_BROWSER_RUNTIME")
	if input == "" {
		t.Skip("private Task40 browser runtime fixture is opt-in")
	}
	if !filepath.IsAbs(input) {
		t.Fatal("absolute protected fixture path required")
	}
	var in struct {
		Issuer        string `json:"issuer"`
		KeyID         string `json:"key_id"`
		PublicKey     string `json:"public_key"`
		OwnerToken    string `json:"owner_token"`
		EnvironmentID string `json:"environment_id"`
		MachineID     string `json:"machine_id"`
		SessionID     string `json:"session_id"`
		AccountID     string `json:"account_id"`
		ListenAddress string `json:"listen_address"`
	}
	encoded, err := os.ReadFile(input)
	if err != nil || json.Unmarshal(encoded, &in) != nil {
		t.Fatal("runtime fixture input unavailable or invalid")
	}
	if in.ListenAddress != "100.95.70.63:15442" && in.ListenAddress != "100.95.70.63:15443" && in.ListenAddress != "127.0.0.1:0" {
		t.Fatal("runtime must bind an approved private Oracle address or ephemeral loopback")
	}
	key, err := base64.RawURLEncoding.DecodeString(in.PublicKey)
	if err != nil || len(key) != ed25519.PublicKeySize {
		t.Fatal("invalid authority public key")
	}
	policy := auth.Policy{Issuer: in.Issuer, Audience: "paperboat-machine", CredentialClass: "terminal_operation", Scopes: []string{"terminal:operate"}, EnvironmentID: in.EnvironmentID, MachineID: in.MachineID, SessionID: in.SessionID, UserID: in.AccountID, MaxLifetime: 5 * time.Minute}
	revocations := auth.NewRevocationCache()
	verifier := auth.Verifier{Keys: task40BrowserKeys{in.KeyID, ed25519.PublicKey(key)}, Clock: task40BrowserClock{}, Revocations: revocations}
	claims, err := verifier.Verify(context.Background(), in.OwnerToken, policy)
	if err != nil || claims.CLIClientSessionID == "" {
		t.Fatal("actual signed owner credential failed verification")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	root := t.TempDir()
	adapter, err := pty.NewAdapter(root)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := session.NewManager(session.ManagerConfig{Launch: func(command pty.Command) (session.PTYProcess, error) { return adapter.Start(command) }, MaxSessions: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		if err := manager.Shutdown(cleanup); err != nil {
			t.Errorf("PTY cleanup: %v", err)
		}
	}()
	_, err = manager.Create(ctx, session.CreateRequest{ID: in.SessionID, Name: "task40-browser", Command: pty.Command{Path: "/bin/sh", Args: []string{"-c", "printf 'task40 browser runtime ready\\n'; while IFS= read -r line; do :; done"}, Env: []string{"PATH=/usr/bin:/bin", "TERM=xterm"}, CWD: root, Dimensions: pty.Dimensions{Columns: 80, Rows: 24}}})
	if err != nil {
		t.Fatal(err)
	}
	// This standalone browser fixture models recording only; Task43 has the connected audit gate.
	dispatcher, err := NewDispatcher(DispatcherConfig{RecordTerminalJoin: func(context.Context, TerminalJoin) error { return nil }, Sessions: manager, Health: health.New("task40-browser", []string{"terminal.v1", "health.v1"}, nil), SessionLauncher: testSessionLauncher{sessions: manager}, WorkspaceRoot: root, Random: rand.Reader})
	if err != nil {
		t.Fatal(err)
	}
	journal, err := operation.NewJournal(128)
	if err != nil {
		t.Fatal(err)
	}
	runtimeServer, err := New(Config{Negotiator: protocol.Negotiator{Profile: config.BYOD, Available: map[string]bool{"terminal.v1": true, "health.v1": true}}, Journal: journal, Handler: dispatcher, MaxConcurrent: 8, HeartbeatInterval: time.Hour, MutationDeadline: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		_ = runtimeServer.Shutdown(cleanup)
	}()
	handler, err := NewWebSocketHandler(WebSocketHandlerConfig{Server: runtimeServer, MaxConnections: 8, OriginPatterns: []string{"100.95.70.63:3103"}, Authorizer: func(token string) (Authorizer, error) {
		return &CredentialAuthorizer{Token: token, Verifier: verifier, Revocations: revocations, Resolver: resolverFunc(func(frame protocol.Frame) (auth.Policy, error) {
			if frame.Capability != "terminal.v1" && frame.Capability != "health.v1" {
				return auth.Policy{}, ErrCredentialPolicy
			}
			return policy, nil
		})}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		t.Fatal(err)
	}
	certificateHost, _, err := net.SplitHostPort(in.ListenAddress)
	if err != nil {
		t.Fatal("invalid runtime listener address")
	}
	template := &x509.Certificate{SerialNumber: serial, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), IPAddresses: []net.IP{net.ParseIP(certificateHost)}, BasicConstraintsValid: true, IsCA: true, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &private.PublicKey, private)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	listener, err := net.Listen("tcp", in.ListenAddress)
	if err != nil {
		t.Fatal(err)
	}
	listenAddress := listener.Addr().String()
	mux := http.NewServeMux()
	mux.Handle("/v1/runtime", handler)
	httpServer := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	defer func() {
		cleanup, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		_ = httpServer.Shutdown(cleanup)
	}()
	served := make(chan error, 1)
	go func() {
		served <- httpServer.Serve(tls.NewListener(listener, &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: private}}}))
	}()
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(certPEM)
	transport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: pool}}
	defer transport.CloseIdleConnections()
	connection, _, err := websocket.Dial(ctx, "wss://"+listenAddress+"/v1/runtime", &websocket.DialOptions{HTTPClient: &http.Client{Transport: transport, Timeout: 10 * time.Second}, HTTPHeader: http.Header{"Authorization": []string{"Bearer " + in.OwnerToken}}, Subprotocols: []string{DefaultWebSocketSubprotocol}})
	if err != nil {
		t.Fatal("authenticated owner websocket could not connect:", err)
	}
	defer connection.CloseNow()
	roundTrip := func(frame protocol.Frame, want string) protocol.Frame {
		wire, err := json.Marshal(frame)
		if err != nil {
			t.Fatal(err)
		}
		requestCtx, done := context.WithTimeout(ctx, 10*time.Second)
		defer done()
		if err := connection.Write(requestCtx, websocket.MessageText, wire); err != nil {
			t.Fatal(err)
		}
		kind, data, err := connection.Read(requestCtx)
		var response protocol.Frame
		if err != nil || kind != websocket.MessageText || json.Unmarshal(data, &response) != nil || response.Type != want {
			t.Fatalf("owner protocol %s failed (type=%s): %v", want, response.Type, err)
		}
		return response
	}
	roundTrip(protocol.Frame{Type: "hello", RequestID: "task40_hello", Version: "1.0", Payload: json.RawMessage(`{"min_version":"1.0","max_version":"1.0","capabilities":["terminal.v1","health.v1"]}`)}, "welcome")
	payload, _ := json.Marshal(map[string]any{"action": "attach", "session_id": in.SessionID})
	roundTrip(protocol.Frame{Type: "request", RequestID: "task40_attach", OperationID: "task40_browser_owner_attach", Version: "1.0", Capability: "terminal.v1", DeadlineMS: 10000, Payload: payload}, "response")
	snapshot, err := manager.Snapshot(in.SessionID)
	if err != nil || len(snapshot.Participants) != 1 || snapshot.Participants[0].AccountID != in.AccountID || snapshot.Participants[0].Role != "owner" {
		t.Fatal("real authenticated owner attachment missing from runtime snapshot")
	}
	drain := make(chan error, 1)
	go func() {
		for {
			if _, _, err := connection.Read(ctx); err != nil {
				drain <- err
				return
			}
		}
	}()
	ready := filepath.Join(filepath.Dir(input), "runtime-ready.json")
	encoded, _ = json.Marshal(map[string]string{"public_host": listenAddress, "cert_pem": string(certPEM), "attachment_id": snapshot.Participants[0].AttachmentID})
	if err := os.WriteFile(ready, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(ready)
	t.Log("Task40 actual PTY and authenticated owner WebSocket are ready")
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	timeout := time.NewTimer(30 * time.Minute)
	defer timeout.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timeout.C:
			t.Fatal("browser runtime hold exceeded 30 minutes")
		case err := <-served:
			if !errors.Is(err, http.ErrServerClosed) {
				t.Fatal(err)
			}
			return
		case err := <-drain:
			if ctx.Err() == nil {
				t.Fatal("owner attachment disconnected:", err)
			}
			return
		case <-ticker.C:
			if _, err := os.Stat(filepath.Join(filepath.Dir(input), "runtime-stop")); err == nil {
				return
			}
		}
	}
}

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/identity"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"github.com/pinksaucepasta/paperboat/internal/tunnel"
)

type execRecoveryBroker func(context.Context, localapi.Peer, localapi.PeerStreamRequest) (net.Conn, error)

func (f execRecoveryBroker) OpenPeerStream(ctx context.Context, p localapi.Peer, r localapi.PeerStreamRequest) (net.Conn, error) {
	return f(ctx, p, r)
}

type execRecoveryHandshakeEOF struct{}

func (execRecoveryHandshakeEOF) Error() string            { return "fixture handshake lost" }
func (execRecoveryHandshakeEOF) Unwrap() error            { return io.EOF }
func (execRecoveryHandshakeEOF) TransportRetryable() bool { return true }

type execRecoveryRemote struct {
	execInputTestConn
	events chan tunnel.ExecEvent
}

func (r *execRecoveryRemote) Events() <-chan tunnel.ExecEvent { return r.events }

// Signal actual CLI output before deliberately losing the initial attachment.
// Queuing an event on the remote does not establish that the client received it.
type execRecoveryOutput struct {
	bytes.Buffer
	observed chan struct{}
	once     sync.Once
}

func (w *execRecoveryOutput) Write(p []byte) (int, error) {
	n, err := w.Buffer.Write(p)
	if bytes.Contains(w.Buffer.Bytes(), []byte("before")) {
		w.once.Do(func() { close(w.observed) })
	}
	return n, err
}

func TestExecRecoveryKeepsItsBudgetAfterTransientReplacementHandshake(t *testing.T) {
	for _, transient := range []bool{true, false} {
		t.Run(fmt.Sprintf("transient=%t", transient), func(t *testing.T) { runExecRecoveryHandshake(t, transient) })
	}
}

func runExecRecoveryHandshake(t *testing.T, transient bool) {
	_, configPath := localDaemonReadinessTestConfig(t, true)
	var opens, descriptors atomic.Int32
	machine := api.UserMachine{ID: "machine_target", Alias: "target", EnvironmentID: "environment_target", State: "online", Online: true, InstallationGeneration: 1, Platform: runtime.GOOS, WorkspaceRoot: filepath.Dir(configPath)}
	backend := execRecoveryBackend(t, machine, &descriptors)
	defer backend.Close()
	writeTestProfile(t, filepath.Dir(configPath), configPath, backend.URL)
	state, err := identity.Open(identity.Config{StateRoot: os.Getenv("PAPERBOAT_RUNTIME_STATE_ROOT")})
	if err != nil {
		t.Fatal(err)
	}
	registration, err := state.Registration()
	if err != nil {
		t.Fatal(err)
	}
	registration.ServerURL = backend.URL
	if err = state.SaveRegistration(registration); err != nil {
		t.Fatal(err)
	}
	paths, err := currentLocalDaemonPaths()
	if err != nil {
		t.Fatal(err)
	}
	snapshot := localapi.Snapshot{Schema: localapi.SnapshotSchemaV1, Generation: 1, ObservedAt: time.Now(), DaemonState: "ready", DaemonVersion: "dev"}
	store, err := localapi.NewSnapshotStore(&snapshot)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := commandLocalAPIServerConfig(paths.SocketPath, store)
	if err != nil {
		t.Fatal(err)
	}
	output := &execRecoveryOutput{observed: make(chan struct{})}
	var operation atomic.Value
	bridges := make(chan struct{}, 3)
	var started atomic.Int32
	defer func() {
		for n := int32(0); n < started.Load(); n++ {
			select {
			case <-bridges:
			case <-time.After(time.Second):
				t.Error("exec bridge cleanup did not finish")
			}
		}
	}()
	fixtureCtx, stopFixture := context.WithCancel(t.Context())
	defer stopFixture()
	cfg.PeerStreams = execRecoveryBroker(func(ctx context.Context, _ localapi.Peer, r localapi.PeerStreamRequest) (net.Conn, error) {
		n := opens.Add(1)
		var request tunnel.ExecRequest
		if json.Unmarshal(r.Payload, &request) != nil || r.Consumer != "exec" {
			return nil, errors.New("invalid exec fixture request")
		}
		if n == 1 {
			operation.Store(request.OperationID)
			if request.FromSequence != 0 {
				t.Error("initial request was not a start")
			}
		} else {
			if request.OperationID != operation.Load().(string) || request.FromSequence != 2 || r.Credential != fmt.Sprintf("renewed_%d", n-1) {
				t.Errorf("recovery binding: operation=%t cursor=%d descriptor=%t", request.OperationID == operation.Load().(string), request.FromSequence, r.Credential == fmt.Sprintf("renewed_%d", n-1))
			}
		}
		if n == 2 {
			if transient {
				return nil, execRecoveryHandshakeEOF{}
			}
			return nil, api.ErrUnauthenticated
		}
		if n > 3 {
			t.Error("recovery exceeded existing budget")
			return nil, errors.New("excess recovery")
		}
		client, server := net.Pipe()
		remote := &execRecoveryRemote{events: make(chan tunnel.ExecEvent, 3)}
		if n == 1 {
			remote.events <- tunnel.ExecEvent{OperationID: request.OperationID, State: "started"}
			remote.events <- tunnel.ExecEvent{OperationID: request.OperationID, EventSequence: 1, Stream: "stdout", Data: []byte("before")}
		} else {
			remote.events <- tunnel.ExecEvent{OperationID: request.OperationID, EventSequence: 2, Stream: "stdout", Data: []byte("after")}
			remote.events <- tunnel.ExecEvent{OperationID: request.OperationID, EventSequence: 3, State: "exited", Result: &tunnel.ExecResult{Code: 0}}
		}
		if n == 1 {
			go func() {
				select {
				case <-output.observed:
				case <-fixtureCtx.Done():
				}
				close(remote.events)
			}()
		} else {
			close(remote.events)
		}
		started.Add(1)
		go func() {
			defer func() { bridges <- struct{}{} }()
			_ = tunnel.ServeLocalPeerConn(t.Context(), server, remote)
		}()
		return client, nil
	})
	server, err := localapi.NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx) }()
	defer stopLocalDaemonReadinessServer(t, cancel, done)
	waitForCommandSocket(t, paths.SocketPath)
	command := newRootCommand()
	var stderr bytes.Buffer
	out := output
	command.SetOut(out)
	command.SetErr(&stderr)
	command.SetArgs([]string{"--config", configPath, "--no-customization", "exec", "target", "--", "fixture-command"})
	commandCtx, finish := context.WithTimeout(t.Context(), 5*time.Second)
	defer finish()
	err = command.ExecuteContext(commandCtx)
	if transient {
		if err != nil || opens.Load() != 3 || descriptors.Load() != 2 || out.String() != "beforeafter" {
			t.Fatalf("replacement handshake consumed recovery: opens=%d descriptors=%d output=%q err=%v", opens.Load(), descriptors.Load(), out.String(), err)
		}
	} else if err == nil || opens.Load() != 2 || descriptors.Load() != 1 || out.String() != "before" {
		t.Fatalf("authoritative rejection was retried: opens=%d descriptors=%d output=%q err=%v", opens.Load(), descriptors.Load(), out.String(), err)
	}
}

func execRecoveryBackend(t *testing.T, machine api.UserMachine, descriptors *atomic.Int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/machines":
			writeAPIData(t, w, api.UserMachinePage{Items: []api.UserMachine{machine}, Pagination: api.Pagination{Limit: 200, Total: 1}})
		case "/v1/machines/machine_target/exec-descriptor":
			var body struct {
				OperationID string `json:"operation_id"`
			}
			if json.NewDecoder(r.Body).Decode(&body) != nil {
				t.Error("invalid descriptor request")
				w.WriteHeader(400)
				return
			}
			n := descriptors.Add(1)
			expires := time.Now().Add(time.Minute)
			writeAPIData(t, w, api.ExecDescriptor{OperationID: body.OperationID, Environment: &api.Environment{ID: machine.EnvironmentID, Kind: "machine", ResourceID: machine.ID, State: "ready", Root: machine.WorkspaceRoot}, Endpoints: api.TerminalEndpoints{QUIC: "quic://peer.example.test:443", WSS: "wss://peer.example.test/v1/runtime"}, Auth: api.AuthMaterial{Method: "bearer", Token: fmt.Sprintf("renewed_%d", n), ExpiresAt: expires, Scopes: []string{"exec:operate"}, AccessSessionID: fmt.Sprintf("access_%d", n)}, ExpiresAt: expires})
		default:
			t.Errorf("unexpected API path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
}

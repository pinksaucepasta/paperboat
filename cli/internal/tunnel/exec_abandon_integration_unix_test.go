//go:build darwin || linux

package tunnel

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/auth"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/execprocess"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/health"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/operation"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/pty"
	hostruntime "github.com/pinksaucepasta/paperboat/internal/hostruntime/runtime"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/server"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/session"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workloadbridge"
	"github.com/pinksaucepasta/paperboat/internal/resolver"
	"github.com/pinksaucepasta/paperboat/internal/shorttemp"
)

// dadape: a killed `pb exec <target> -- cat` client left cat running in its
// session owner, and the owner could never retire. The real client -> hostd ->
// owner bridge path: once the client is gone past the linger, the owner ends
// the process and retirement succeeds.
func TestKilledExecClientEndsOwnerProcessAndOwnerRetires(t *testing.T) {
	root := shorttemp.Dir(t)
	const linger = 300 * time.Millisecond
	owned, err := execprocess.New(execprocess.Config{WorkspaceRoot: root, BaseEnvironment: []string{"PATH=/usr/bin:/bin"}, MaximumActive: 2, ReplayBytes: 64 << 10, DetachedLinger: linger, CancelGrace: 200 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = owned.Shutdown(ctx)
	})
	ownerSessions, err := session.NewManager(session.ManagerConfig{Launch: func(pty.Command) (session.PTYProcess, error) { return nil, errors.New("terminal unavailable") }, MaxSessions: 1})
	if err != nil {
		t.Fatal(err)
	}
	token := bytes.Repeat([]byte{7}, 32)
	bridge := &workloadbridge.Server{
		Endpoint: filepath.Join(shorttemp.Dir(t), "owner", "owner.sock"), Token: token, Sessions: ownerSessions, SessionOnly: true,
		Executions: &execprocess.RemoteServer{Manager: owned, MaximumReaders: 8}, MaximumConcurrent: 16, Ready: make(chan error, 1),
		Fence: func() hostdproto.Status {
			return hostdproto.Status{State: hostdproto.StateActive, WorkerID: "owner", Epoch: 1, APIVersion: 1}
		},
		AcquireActive: func(string, uint64) (func(), error) { return func() {}, nil },
		// The production owner's quiescence gate (runtime/session_owner.go).
		Retire: func(context.Context) error {
			if len(owned.ActiveSnapshots()) != 0 {
				return session.ErrUpdateBusy
			}
			return nil
		},
	}
	bridgeCtx, stopBridge := context.WithCancel(context.Background())
	bridgeDone := make(chan error, 1)
	go func() { bridgeDone <- bridge.Run(bridgeCtx) }()
	if err := <-bridge.Ready; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stopBridge(); <-bridgeDone; _ = ownerSessions.Shutdown(context.Background()) })
	ownerClient, err := workloadbridge.NewClient(bridge.Endpoint, token, "owner", 1, "")
	if err != nil {
		t.Fatal(err)
	}
	defer ownerClient.Close()
	executions, err := ownerClient.Executions()
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Truncate(time.Second)
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	const operationID = "operation_exec_abandoned"
	claims := auth.Claims{
		Issuer: "https://paperboat.test", Audience: "paperboat-machine", Subject: "user_abandon", JTI: "jti_exec_abandon",
		IssuedAt: now.Add(-time.Second).Unix(), ExpiresAt: now.Add(2 * time.Minute).Unix(), Scope: []string{"exec:operate"}, CredentialClass: "exec_operation",
		EnvironmentID: "env_abandon", UserID: "user_abandon", CLIClientSessionID: "cli_abandon", HelperID: "helper_abandon", MachineID: "machine_abandon", OperationID: operationID,
	}
	authorizers, err := hostruntime.NewStaticAuthorizer(hostruntime.StaticAuthConfig{Issuer: claims.Issuer, EnvironmentID: claims.EnvironmentID, MachineID: claims.MachineID, HelperID: claims.HelperID, Keys: map[string]ed25519.PublicKey{"key_abandon": public}, Clock: execCanaryClock{now}})
	if err != nil {
		t.Fatal(err)
	}
	authorizer, err := authorizers(signExecCanaryCredential(t, "key_abandon", private, claims))
	if err != nil {
		t.Fatal(err)
	}
	hostSessions, err := session.NewManager(session.ManagerConfig{Launch: func(pty.Command) (session.PTYProcess, error) { return nil, errors.New("terminal unavailable") }, MaxSessions: 1})
	if err != nil {
		t.Fatal(err)
	}
	dispatcher, err := server.NewDispatcher(server.DispatcherConfig{Sessions: hostSessions, Health: health.New("abandon", []string{"terminal.v1", "health.v1", "exec.v1"}, nil), SessionLauncher: execCanaryLauncher{}, WorkspaceRoot: root, Random: bytes.NewReader(bytes.Repeat([]byte{1}, 256)), Exec: executions})
	if err != nil {
		t.Fatal(err)
	}
	journal, err := operation.NewJournal(32)
	if err != nil {
		t.Fatal(err)
	}
	host, err := server.New(server.Config{Negotiator: protocol.Negotiator{Available: map[string]bool{"terminal.v1": true, "health.v1": true, "exec.v1": true}}, Journal: journal, Authorizer: authorizer, Handler: dispatcher, MaxConcurrent: 4, HeartbeatInterval: time.Hour, MutationDeadline: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = host.Shutdown(ctx)
		_ = hostSessions.Shutdown(ctx)
	})
	wire := newExecCanaryWire()
	go func() { _ = host.ServeAuthenticated(wire, authorizer) }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := helperHandshake(ctx, wire); err != nil {
		t.Fatal(err)
	}
	connection := &helperExecConn{message: wire, target: &resolver.TerminalTarget{}, request: ExecRequest{OperationID: operationID, Argv: []string{"/bin/sh", "-c", `echo $$ > pid; exec cat`}, CWD: root}, events: make(chan ExecEvent, 64), done: make(chan struct{}), pending: make(map[string]chan helperFrame)}
	if err := connection.initialize(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Write([]byte("streamed stdin\n")); err != nil {
		t.Fatal(err)
	}
	var pid int
	for pid == 0 {
		if data, err := os.ReadFile(filepath.Join(root, "pid")); err == nil && strings.HasSuffix(string(data), "\n") {
			pid, _ = strconv.Atoi(strings.TrimSpace(string(data)))
		}
		if ctx.Err() != nil {
			t.Fatal("remote cat never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Attached clients are never cut, however long they stay.
	time.Sleep(3 * linger)
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("attached exec ended: %v", err)
	}
	if err := ownerClient.Retire(ctx); !errors.Is(err, session.ErrUpdateBusy) {
		t.Fatalf("owner retired under a running command: %v", err)
	}

	// Kill the client: the transport drops with no cancel, close or EOF.
	_ = wire.Close()
	deadline := time.Now().Add(linger + 3*time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatalf("orphaned cat %d still running %s after its client was killed", pid, linger+3*time.Second)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := ownerClient.Retire(ctx); err != nil {
		t.Fatalf("owner could not retire after its orphaned command ended: %v", err)
	}
}

//go:build darwin || linux

package daemoncmd

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	pbSync "github.com/pinksaucepasta/paperboat/internal/controlsync/proto"
	"github.com/pinksaucepasta/paperboat/internal/daemonrpc"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
)

func readinessSnapshot(generation uint64, state string) localapi.Snapshot {
	return localapi.Snapshot{Schema: localapi.SnapshotSchemaV1, Generation: generation, ObservedAt: time.Now().UTC(), DaemonState: state, DaemonVersion: "test", Machines: []localapi.MachineStatus{}}
}
func startReadinessServer(t *testing.T, initial localapi.Snapshot) (*localapi.SnapshotStore, localapi.Paths) {
	t.Helper()
	directory, err := os.MkdirTemp("/tmp", "pb-ready-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	socket := filepath.Join(directory, "api.sock")
	store, err := localapi.NewSnapshotStore(&initial)
	if err != nil {
		t.Fatal(err)
	}
	server, err := localapi.NewServer(localapi.ServerConfig{SocketPath: socket, OwnerUID: os.Geteuid(), OwnerGID: os.Getegid(), Source: store})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	deadline := time.Now().Add(time.Second)
	for {
		if _, err = os.Lstat(socket); err == nil {
			break
		}
		select {
		case serveErr := <-done:
			t.Fatalf("local API failed: %v", serveErr)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("local API did not listen")
		}
		time.Sleep(time.Millisecond)
	}
	return store, localapi.Paths{SocketPath: socket}
}
func TestWaitDaemonReadyObservesStartingToReadySnapshot(t *testing.T) {
	store, paths := startReadinessServer(t, readinessSnapshot(1, "starting"))
	old := servicePaths
	t.Cleanup(func() { servicePaths = old })
	servicePaths = func() (localapi.Paths, error) { return paths, nil }
	go func() { time.Sleep(30 * time.Millisecond); _, _ = store.Publish(readinessSnapshot(2, "ready")) }()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := waitDaemonReady(ctx); err != nil {
		t.Fatal(err)
	}
}
func TestWaitDaemonReadyReportsLastStateOnCancellation(t *testing.T) {
	_, paths := startReadinessServer(t, readinessSnapshot(1, "degraded"))
	old := servicePaths
	t.Cleanup(func() { servicePaths = old })
	servicePaths = func() (localapi.Paths, error) { return paths, nil }
	ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
	defer cancel()
	err := waitDaemonReady(ctx)
	if err == nil || !strings.Contains(err.Error(), `last state was "degraded"`) || !strings.Contains(err.Error(), "run pb status and retry") {
		t.Fatalf("readiness error=%v", err)
	}
}

func TestWaitDaemonReadyRetainsSocketFailureAndRecovers(t *testing.T) {
	old := servicePaths
	t.Cleanup(func() { servicePaths = old })
	missing := filepath.Join(t.TempDir(), "PRIVATE_READINESS_SOCKET")
	servicePaths = func() (localapi.Paths, error) { return localapi.Paths{SocketPath: missing}, nil }
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	err := waitDaemonReady(ctx)
	var socketFailure *net.OpError
	if !errors.Is(err, context.DeadlineExceeded) || !errors.As(err, &socketFailure) {
		t.Fatal("readiness discarded original socket failure or deadline")
	}
	if strings.Contains(err.Error(), "PRIVATE_READINESS_SOCKET") {
		t.Fatal("readiness error exposed private socket path")
	}
	_, paths := startReadinessServer(t, readinessSnapshot(1, "ready"))
	servicePaths = func() (localapi.Paths, error) { return paths, nil }
	healthy, stop := context.WithTimeout(t.Context(), time.Second)
	defer stop()
	if err := waitDaemonReady(healthy); err != nil {
		t.Fatal("fresh ready daemon did not recover")
	}
}

// The command must fail when its actual IPC response cannot reach stdout.
type resolveOutputFailure struct{}

func (resolveOutputFailure) Write([]byte) (int, error) { return 0, syscall.EIO }

func TestResolveCommandOutputFailureAndRecovery(t *testing.T) {
	directory, err := os.MkdirTemp("", "pb-rpc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	socket := filepath.Join(directory, "rpc.sock")
	t.Setenv("PAPERBOAT_DAEMON_SOCK", socket)
	listener, err := daemonrpc.Listen(socket)
	if err != nil {
		t.Fatal(err)
	}
	backend := daemonrpc.NewLiveDaemonBackend(nil, "test")
	backend.ApplyPeerUpdates([]*pbSync.PeerUpdate{{PeerId: "machine", Alias: "office", Approved: true, Online: true, AssignedIp: "127.100.0.2"}}, 1, nil)
	server := daemonrpc.NewServer(backend)
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	t.Cleanup(func() { server.Stop(); <-done })
	command := ResolveCommand()
	command.SetContext(t.Context())
	command.SetArgs([]string{"office"})
	command.SetOut(resolveOutputFailure{})
	command.SetErr(&bytes.Buffer{})
	if err := command.Execute(); !errors.Is(err, syscall.EIO) {
		t.Fatal("resolve command lost stdout failure")
	}
	var output bytes.Buffer
	command = ResolveCommand()
	command.SetContext(t.Context())
	command.SetArgs([]string{"office"})
	command.SetOut(&output)
	command.SetErr(&bytes.Buffer{})
	if err := command.Execute(); err != nil || !strings.Contains(output.String(), "127.100.0.2") {
		t.Fatal("fresh IPC resolve did not recover", err)
	}
}

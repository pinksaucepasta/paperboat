//go:build darwin || linux

package daemoncmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

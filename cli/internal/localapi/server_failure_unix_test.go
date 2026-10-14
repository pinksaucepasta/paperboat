//go:build darwin || linux

package localapi

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
)

func TestLocalAPIHandlerPanicIsPrivateAndNextRequestRecovers(t *testing.T) {
	socket := filepath.Join(localAPITestDir(t), "panic.sock")
	var panicNext atomic.Bool
	server, err := NewServer(ServerConfig{SocketPath: socket, OwnerUID: os.Geteuid(), OwnerGID: os.Getegid(), Timeout: time.Second,
		Source: snapshotSourceFunc(func(context.Context) (Snapshot, error) {
			if panicNext.Swap(false) {
				panic("PRIVATE token=fixture /private/path")
			}
			return validSnapshot(), nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var faults []errorreport.Fault
	restore := errorreport.InstallFaultObserver(func(_ context.Context, fault errorreport.Fault) {
		mu.Lock()
		defer mu.Unlock()
		faults = append(faults, fault)
	})
	defer restore()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("server shutdown lost cancellation")
			}
		case <-time.After(time.Second):
			t.Error("server survived cleanup")
		}
	})
	waitForSocket(t, socket)
	panicNext.Store(true)
	client, err := NewClient(socket, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer client.http.CloseIdleConnections()
	reference := supportref.New()
	requestCtx := supportref.WithContext(t.Context(), reference)
	if _, err := client.Snapshot(requestCtx); err == nil {
		t.Fatal("panic response was accepted")
	}
	if snapshot, err := client.Snapshot(requestCtx); err != nil || snapshot.Generation != validSnapshot().Generation {
		t.Fatal("local API failed to recover for the next request")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(faults) != 1 || faults[0].Code != "process_panic" || faults[0].SupportReference != reference || faults[0].Stage != "process" {
		t.Fatalf("panic faults=%#v", faults)
	}
}

//go:build darwin || linux

package main

import (
	"net"
	"net/http"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/localapi"
)

func TestRefreshLocalInventoryAsksTheDaemon(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "api.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	var refreshed atomic.Bool
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshed.Store(r.Method == http.MethodPost && r.URL.Path == "/v1/inventory/refresh")
		w.Header().Set("X-Paperboat-Protocol", localapi.ProtocolV1)
		w.WriteHeader(http.StatusNoContent)
	})}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close() })
	old := currentLocalDaemonPaths
	currentLocalDaemonPaths = func() (localapi.Paths, error) { return localapi.Paths{SocketPath: socket}, nil }
	t.Cleanup(func() { currentLocalDaemonPaths = old })
	if err := refreshLocalInventory(t.Context()); err != nil || !refreshed.Load() {
		t.Fatalf("refresh err=%v asked=%v", err, refreshed.Load())
	}
}

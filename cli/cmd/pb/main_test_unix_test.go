//go:build unix

package main

import (
	"net"
	"os"
	"testing"
	"time"

	"github.com/adrg/xdg"
	"github.com/pinksaucepasta/paperboat/cli/internal/localapi"
)

func commandLocalAPIServerConfig(path string, source localapi.SnapshotSource) (localapi.ServerConfig, error) {
	return localapi.ServerConfig{SocketPath: path, OwnerUID: os.Geteuid(), OwnerGID: os.Getegid(), Source: source}, nil
}

func commandRuntimeTestRoot(t *testing.T) string {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "pb-command-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	return root
}

func isolateCommandCredentialLocation(t *testing.T, root string) {
	t.Helper()
	previous := xdg.ConfigHome
	xdg.ConfigHome = root
	t.Cleanup(func() { xdg.ConfigHome = previous })
}

func waitForCommandSocket(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for delay := 5 * time.Millisecond; time.Now().Before(deadline); delay = min(2*delay, 50*time.Millisecond) {
		connection, err := net.DialTimeout("unix", path, 2*time.Second)
		if err == nil {
			_ = connection.Close()
			return
		}
		time.Sleep(delay)
	}
	t.Fatalf("local command socket %s was not ready", path)
}

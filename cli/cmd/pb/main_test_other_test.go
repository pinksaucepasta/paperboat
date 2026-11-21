//go:build !unix && !windows

package main

import (
	"path/filepath"
	"testing"

	"github.com/adrg/xdg"
	"github.com/pinksaucepasta/paperboat/cli/internal/localapi"
)

func commandLocalAPIServerConfig(path string, source localapi.SnapshotSource) (localapi.ServerConfig, error) {
	return localapi.ServerConfig{SocketPath: path, OwnerUID: 0, OwnerGID: 0, Source: source}, nil
}

func commandRuntimeTestRoot(t *testing.T) string { t.Helper(); return t.TempDir() }

func isolateCommandCredentialLocation(t *testing.T, root string) {
	t.Helper()
	previous := xdg.ConfigHome
	xdg.ConfigHome = root
	t.Cleanup(func() { xdg.ConfigHome = previous })
}

func waitForCommandSocket(t *testing.T, _ string) { t.Helper() }

// commandTestSocket names a local API endpoint no daemon serves.
func commandTestSocket(t *testing.T, root string) string {
	return filepath.Join(root, "local-api.sock")
}

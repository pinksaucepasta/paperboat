//go:build darwin || linux

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"github.com/pinksaucepasta/paperboat/internal/localdaemon"
)

type failingMachineSource struct{ err error }

func (s failingMachineSource) ListUserMachines(context.Context) ([]api.UserMachine, error) {
	return nil, s.err
}

// The login's daemon refresh goes through the real chain: the daemon's inventory refresh, the local API handler, the
// client and the message. A signed-out daemon (a revoked machine) says to run pb login --fresh, because signing in
// again cannot restore its enrollment; any other refresh failure keeps the generic advice (pbvm2, 2026-11-09).
func TestLoginDaemonRefreshFailureAfterRevocationPointsToFreshLogin(t *testing.T) {
	for name, c := range map[string]struct {
		err   error
		code  string
		fresh bool
	}{
		"server 401":     {&api.APIError{Status: 401, Code: "unauthorized"}, localapi.AuthenticationRequiredCode, true},
		"no credentials": {config.ErrNoCredentials, localapi.AuthenticationRequiredCode, true},
		"server error":   {&api.APIError{Status: 500, Code: "internal"}, "inventory_unavailable", false},
	} {
		t.Run(name, func(t *testing.T) {
			store, err := localapi.NewSnapshotStore(nil)
			if err != nil {
				t.Fatal(err)
			}
			inventory, err := localdaemon.NewInventory(localdaemon.InventoryConfig{Source: failingMachineSource{c.err}, Store: store, RefreshInterval: time.Second, RequestTimeout: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			socket := filepath.Join(commandRuntimeTestRoot(t), "api.sock") // darwin caps unix socket paths at 104 bytes
			server, err := localapi.NewServer(localapi.ServerConfig{SocketPath: socket, OwnerUID: os.Geteuid(), OwnerGID: os.Getegid(), Source: inventory})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			go func() { done <- server.Run(ctx) }()
			t.Cleanup(func() { cancel(); <-done })
			waitForCommandSocket(t, socket)
			old := currentLocalDaemonPaths
			currentLocalDaemonPaths = func() (localapi.Paths, error) { return localapi.Paths{SocketPath: socket}, nil }
			t.Cleanup(func() { currentLocalDaemonPaths = old })

			err = refreshLocalInventory(ctx)
			var remote *localapi.RemoteError
			if !errors.As(err, &remote) || remote.Code != c.code {
				t.Fatalf("refresh err=%v, want a daemon refusal with code %s", err, c.code)
			}
			message := daemonRefreshFailureMessage(err)
			if fresh := strings.Contains(message, "pb login --fresh"); fresh != c.fresh || (!c.fresh && !strings.Contains(message, "pb login again")) {
				t.Fatalf("message=%q, want --fresh advice %v", message, c.fresh)
			}
		})
	}
}

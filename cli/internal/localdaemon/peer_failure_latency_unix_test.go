//go:build darwin || linux

package localdaemon

import (
	"context"
	"errors"
	"net"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
)

type stalledAuthenticationSource struct {
	*scriptedMachineSource
	stalled atomic.Bool
	release chan struct{}
}

func (s *stalledAuthenticationSource) ListUserMachines(ctx context.Context) ([]api.UserMachine, error) {
	if s.stalled.Load() {
		select {
		case <-s.release:
		case <-ctx.Done():
		}
		return nil, context.DeadlineExceeded
	}
	return s.scriptedMachineSource.ListUserMachines(ctx)
}

type failedNativeBroker struct{ err error }

func (b failedNativeBroker) OpenPeerStream(context.Context, localapi.Peer, localapi.PeerStreamRequest) (net.Conn, error) {
	return nil, b.err
}

func TestNativeFailureReachesLocalClientWithoutControlPlaneRefresh(t *testing.T) {
	for _, failure := range []error{net.ErrClosed, context.Canceled} {
		t.Run(failure.Error(), func(t *testing.T) {
			source := &stalledAuthenticationSource{scriptedMachineSource: &scriptedMachineSource{results: []machineResult{{machines: []api.UserMachine{{ID: "machine_1", Alias: "studio"}}}}}, release: make(chan struct{})}
			defer close(source.release)
			inventory, _ := newTestInventory(t, source, time.Now)
			if err := inventory.Refresh(t.Context()); err != nil {
				t.Fatal(err)
			}
			source.stalled.Store(true)
			paths := daemonTestPaths(t)
			server, err := localapi.NewServer(localapi.ServerConfig{SocketPath: paths.SocketPath, OwnerUID: os.Geteuid(), OwnerGID: os.Getegid(), Source: inventory, PeerStreams: failedNativeBroker{failure}, SignedOut: inventory.SignedOut})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			stopped := make(chan error, 1)
			go func() { stopped <- server.Run(ctx) }()
			defer func() { cancel(); <-stopped }()
			waitForDaemonSocket(t, paths.SocketPath)
			client, err := localapi.NewClient(paths.SocketPath, 2*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			request, err := localapi.NewPeerStreamRequest("machine_1", "environment_1", 1, "terminal", "operation_1", "credential", time.Now().Add(time.Minute), 1024, nil)
			if err != nil {
				t.Fatal(err)
			}
			started := time.Now()
			_, err = client.OpenPeerStream(t.Context(), request)
			var remote *localapi.RemoteError
			if !errors.As(err, &remote) || remote.StatusCode != 503 || time.Since(started) > 500*time.Millisecond {
				t.Fatalf("native failure delayed or changed: type=%T elapsed=%s", err, time.Since(started))
			}
		})
	}
}

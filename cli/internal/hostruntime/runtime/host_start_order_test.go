//go:build darwin || linux

package runtime

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	runtimeconfig "github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/config"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/configapply"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/server"
)

// blockedStart waits in Start the way a component does while it waits out a control-plane request timeout.
type blockedStart struct{ release chan struct{} }

func (s blockedStart) Start(ctx context.Context) error {
	select {
	case <-s.release:
	case <-ctx.Done():
	}
	return nil
}
func (blockedStart) Shutdown(context.Context) error { return nil }

type acceptSignalListener struct {
	*hostListener
	once     sync.Once
	accepted chan struct{}
}

func (l *acceptSignalListener) Accept() (net.Conn, error) {
	l.once.Do(func() { close(l.accepted) })
	return l.hostListener.Accept()
}

// After a restart in a control-plane outage the first exec waited 35 s: the stable daemon starts its components in
// order, and the observation and preview-recovery components wait out 10-20 s request timeouts before the
// control_plane listener opened. The listener must open while they still wait.
func TestControlPlaneServesWhileAControlPlaneComponentStillStarts(t *testing.T) {
	blocked := blockedStart{release: make(chan struct{})}
	listener := &acceptSignalListener{hostListener: &hostListener{closed: make(chan struct{})}, accepted: make(chan struct{})}
	root := t.TempDir()
	host, err := NewHost(context.Background(), HostConfig{
		Runtime:       runtimeconfig.Config{StateRoot: root, Version: "test", Limits: runtimeconfig.DefaultLimits, Resources: runtimeconfig.DefaultResources},
		ListenAddress: "127.0.0.1:0", WorkspaceRoot: root, EnvironmentID: "env_start_order", MachineID: "machine_start_order",
	}, HostDependencies{
		Authorizer:                func(string) (server.Authorizer, error) { return hostAuthorizer{}, nil },
		Listener:                  func() (net.Listener, error) { return listener, nil },
		ConfigApply:               configapply.ConformanceHandler{},
		SessionLauncherFactory:    testSessionLauncherFactory("/bin/sh", []string{"-l"}, []string{"PATH=/usr/bin:/bin"}),
		RuntimeObservationService: blocked,
		PreviewRecovery:           blocked,
	})
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan error, 1)
	go func() { started <- host.Start(context.Background()) }()
	t.Cleanup(func() {
		select {
		case <-blocked.release:
		default:
			close(blocked.release)
		}
		<-started
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = host.Shutdown(ctx)
	})
	select {
	case <-listener.accepted:
	case err := <-started:
		t.Fatalf("host started (%v) before the blocked components were released: the test no longer holds them", err)
	case <-time.After(5 * time.Second):
		t.Fatal("control_plane listener did not open while a control-plane component was still starting")
	}
}

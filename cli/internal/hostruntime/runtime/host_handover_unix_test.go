//go:build darwin || linux

package runtime

import (
	"context"
	"net"
	"testing"
	"time"

	runtimeconfig "github.com/pinksaucepasta/paperboat/internal/hostruntime/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/configapply"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/server"
)

// drainingAttachment is a runtime attachment whose Shutdown keeps its carriers serving until released, as
// the real one does while the edges move to the next worker (up to carrierDrainGrace).
type drainingAttachment struct{ draining, release chan struct{} }

func (drainingAttachment) Start(context.Context) error { return nil }
func (a drainingAttachment) Shutdown(ctx context.Context) error {
	close(a.draining)
	select {
	case <-a.release:
	case <-ctx.Done():
	}
	return nil
}

// A retiring generation stops accepting on the runtime listener it shares with the next one before any
// slow drain: pbvm4 prefix1 (2026-11-05) the old daemon accepted exec streams for 2.7 s after the new one
// was ready, while its runtime attachment drained, and cut them when it closed its listener last.
func TestRetiringHostStopsAcceptingBeforeItsAttachmentDrains(t *testing.T) {
	root := t.TempDir()
	listener := &hostListener{closed: make(chan struct{})}
	attachment := drainingAttachment{draining: make(chan struct{}), release: make(chan struct{})}
	config := runtimeconfig.Config{StateRoot: root, Version: "test", Limits: runtimeconfig.DefaultLimits, Resources: runtimeconfig.DefaultResources}
	host, err := NewHost(context.Background(), HostConfig{Runtime: config, ListenAddress: "127.0.0.1:0", WorkspaceRoot: root, EnvironmentID: "env_test", MachineID: "machine_test"}, HostDependencies{
		Authorizer:  func(string) (server.Authorizer, error) { return hostAuthorizer{}, nil },
		Listener:    func() (net.Listener, error) { return listener, nil },
		ConfigApply: configapply.ConformanceHandler{}, ConfigApplyProof: true,
		SessionLauncherFactory:   testSessionLauncherFactory("/bin/sh", []string{"-l"}, []string{"PATH=/usr/bin:/bin"}),
		RuntimeAttachmentService: attachment,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := host.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	stopped := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		stopped <- host.Shutdown(ctx)
	}()
	select {
	case <-attachment.draining:
	case <-time.After(5 * time.Second):
		t.Fatal("the runtime attachment never started draining")
	}
	select {
	case <-listener.closed:
	case <-time.After(time.Second):
		t.Fatal("the runtime listener still accepts while the attachment drains")
	}
	close(attachment.release)
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
}

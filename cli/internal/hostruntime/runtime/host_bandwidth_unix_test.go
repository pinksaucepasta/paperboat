//go:build darwin || linux

package runtime

import (
	"context"
	"net"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/bandwidth"
	runtimeconfig "github.com/pinksaucepasta/paperboat/internal/hostruntime/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/server"
)

type finalUsageNativeService struct{ recorder *bandwidth.Recorder }

func (s finalUsageNativeService) Start(context.Context) error { return nil }
func (s finalUsageNativeService) Shutdown(context.Context) error {
	local, remote := net.Pipe()
	connection := s.recorder.Wrap(local, bandwidth.Binding{AccessSessionID: "access", StreamID: "final-native-stream", Consumer: "terminal"}, func(string) bandwidth.Path { return bandwidth.Path{Mode: "direct"} })
	read := make(chan error, 1)
	go func() { var p [1]byte; _, err := remote.Read(p[:]); read <- err }()
	_, writeErr := connection.Write([]byte{1})
	readErr := <-read
	_ = connection.Close()
	_ = remote.Close()
	if writeErr != nil {
		return writeErr
	}
	return readErr
}

func TestHostStableShutdownDrainsBandwidthAfterNative(t *testing.T) {
	root := t.TempDir()
	sink := &hostUsageSink{}
	recorder, err := bandwidth.Open(filepath.Join(root, "bandwidth-usage.json"), sink, nil)
	if err != nil {
		t.Fatal(err)
	}
	host, err := NewHost(t.Context(), HostConfig{Runtime: runtimeconfig.Config{StateRoot: root, Version: "test", Limits: runtimeconfig.DefaultLimits, Resources: runtimeconfig.DefaultResources}, ListenAddress: "127.0.0.1:0", WorkspaceRoot: root, MachineID: "machine-test", InboxPath: filepath.Join(root, "Inbox")}, HostDependencies{Bandwidth: recorder, Authorizer: func(string) (server.Authorizer, error) { return hostAuthorizer{}, nil }, SessionLauncherFactory: testSessionLauncherFactory("/bin/sh", []string{"-l"}, []string{"PATH=/usr/bin:/bin"}), NativePeerFactory: func(func(net.Conn) error, http.Handler) (Service, error) {
		return finalUsageNativeService{recorder}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = host.Shutdown(context.Background()) })
	// StartHostd deliberately leaves the separate worker in New state. Shutdown
	// must close stable listeners before draining their final application bytes.
	if err := host.StartHostd(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := host.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.reports) != 1 || sink.reports[0].DownloadBytes != 1 || sink.reports[0].StreamID != "final-native-stream" {
		t.Fatal("stable shutdown lost native application's final bytes")
	}
}

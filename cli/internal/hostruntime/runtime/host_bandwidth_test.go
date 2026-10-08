//go:build darwin || linux || windows

package runtime

import (
	"context"
	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/bandwidth"
	"net"
	"path/filepath"
	"sync"
	"testing"
)

type hostUsageSink struct {
	mu      sync.Mutex
	reports []api.NativeUsageReport
}

func (s *hostUsageSink) ReportNativeUsage(_ context.Context, reports []api.NativeUsageReport, _, _ int64, _ bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reports = append(s.reports, reports...)
	return nil
}
func TestHostBandwidthLifecycleSharesGenerationCounters(t *testing.T) {
	sink := &hostUsageSink{}
	recorder, err := bandwidth.Open(filepath.Join(t.TempDir(), "usage.json"), sink, nil)
	if err != nil {
		t.Fatal(err)
	}
	service := &hostBandwidthService{recorder: recorder}
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
	for _, binding := range []bandwidth.Binding{{AccessSessionID: "native-session", StreamID: "generation-one", Consumer: "terminal"}, {AccessSessionID: "native-session", StreamID: "generation-two", Consumer: "terminal"}, {AccessSessionID: "browser-attachment", StreamID: "browser-attachment", Consumer: "terminal"}} {
		local, remote := net.Pipe()
		metered := recorder.Wrap(local, binding, func(string) bandwidth.Path { return bandwidth.Path{Mode: "direct"} })
		received := make(chan error, 1)
		go func() { var b [1]byte; _, err := remote.Read(b[:]); received <- err }()
		if _, err := metered.Write([]byte{1}); err != nil {
			t.Fatal(err)
		}
		if err := <-received; err != nil {
			t.Fatal(err)
		}
		_ = metered.Close()
		_ = remote.Close()
	}
	if err := service.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := service.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.reports) != 3 {
		t.Fatalf("shared lifecycle final reports=%d, want 3", len(sink.reports))
	}
	for _, report := range sink.reports {
		if report.DownloadBytes != 1 {
			t.Fatalf("generation counter changed: %#v", report)
		}
	}
}

func TestNewHostFailureDrainsOwnedBandwidth(t *testing.T) {
	sink := &hostUsageSink{}
	recorder, err := bandwidth.Open(filepath.Join(t.TempDir(), "usage.json"), sink, nil)
	if err != nil {
		t.Fatal(err)
	}
	local, remote := net.Pipe()
	connection := recorder.Wrap(local, bandwidth.Binding{AccessSessionID: "access", StreamID: "stream", Consumer: "terminal"}, func(string) bandwidth.Path { return bandwidth.Path{Mode: "direct"} })
	read := make(chan error, 1)
	go func() { var p [1]byte; _, err := remote.Read(p[:]); read <- err }()
	if _, err := connection.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	if err := <-read; err != nil {
		t.Fatal(err)
	}
	_ = connection.Close()
	_ = remote.Close()
	if _, err := NewHost(t.Context(), HostConfig{}, HostDependencies{Bandwidth: recorder}); err == nil {
		t.Fatal("invalid composition accepted")
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.reports) != 1 || sink.reports[0].DownloadBytes != 1 {
		t.Fatal("construction failure did not drain owned recorder")
	}
}

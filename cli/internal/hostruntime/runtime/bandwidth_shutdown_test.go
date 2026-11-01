package runtime

import (
	"context"
	"io"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/bandwidth"
)

type shutdownUsageSink struct {
	release chan struct{}
	mu      sync.Mutex
	reports []api.NativeUsageReport
}

func (s *shutdownUsageSink) ReportNativeUsage(ctx context.Context, reports []api.NativeUsageReport, _ int64, _ int64, _ bool) error {
	if s.release != nil {
		select {
		case <-s.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reports = append(s.reports, reports...)
	return nil
}
func TestRuntimeShutdownCheckpointsUsageWithoutWaitingForControlPlane(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	sink := &shutdownUsageSink{release: make(chan struct{})}
	recorder, err := bandwidth.Open(path, sink, nil)
	if err != nil {
		t.Fatal(err)
	}
	left, right := net.Pipe()
	metered := recorder.Wrap(left, bandwidth.Binding{AccessSessionID: "access", StreamID: "stream", Consumer: "terminal"}, func(string) bandwidth.Path { return bandwidth.Path{Mode: "direct"} })
	sent := make(chan error, 1)
	go func() { _, err := metered.Write([]byte("counted")); sent <- err }()
	if _, err := io.ReadFull(right, make([]byte, 7)); err != nil {
		t.Fatal(err)
	}
	if err := <-sent; err != nil {
		t.Fatal(err)
	}
	metered.Close()
	right.Close()
	service := &hostBandwidthService{recorder: recorder}
	done := make(chan error, 1)
	go func() { done <- service.Shutdown(t.Context()) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
		close(sink.release)
	case <-time.After(500 * time.Millisecond):
		close(sink.release)
		<-done
		t.Fatal("runtime shutdown waited for control-plane usage upload")
	}
	// The next carrier must deliver the exact final bytes without marking a clean
	// restart incomplete, even though the old carrier did not wait for the server.
	nextSink := &shutdownUsageSink{}
	next, err := bandwidth.Open(path, nextSink, nil)
	if err != nil {
		t.Fatal(err)
	}
	if next.Snapshot().Incomplete {
		t.Fatal("clean checkpoint was reported incomplete")
	}
	if err := next.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, r := range nextSink.reports {
		total += r.UploadBytes + r.DownloadBytes
	}
	if total != 7 {
		t.Fatalf("checkpoint lost final usage: bytes=%d", total)
	}
}

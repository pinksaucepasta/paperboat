package bandwidth

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
)

type testSink struct {
	mu      sync.Mutex
	fail    bool
	reports map[string]api.NativeUsageReport
}

func (s *testSink) ReportNativeUsage(_ context.Context, reports []api.NativeUsageReport, _, _ int64, _ bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail {
		return errors.New("offline")
	}
	if s.reports == nil {
		s.reports = map[string]api.NativeUsageReport{}
	}
	for _, r := range reports {
		s.reports[key(r)] = r
	}
	return nil
}
func newTestRecorder(t *testing.T, sink *testSink) *Recorder {
	t.Helper()
	r, err := Open(filepath.Join(t.TempDir(), "usage.json"), sink, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close(context.Background()) })
	return r
}

type partialConn struct{ net.Conn }

func (partialConn) Read(p []byte) (int, error)  { copy(p, "abc"); return min(len(p), 3), io.EOF }
func (partialConn) Write(p []byte) (int, error) { return min(len(p), 2), io.ErrShortWrite }

func TestSuccessfulPartialIOAndPathTransition(t *testing.T) {
	r := newTestRecorder(t, &testSink{})
	b := Binding{AccessSessionID: "access", StreamID: "stream", Consumer: "terminal"}
	path := Path{Mode: "direct"}
	conn := r.Wrap(partialConn{}, b, func(string) Path { return path })
	if n, err := conn.Read(make([]byte, 8)); n != 3 || !errors.Is(err, io.EOF) {
		t.Fatalf("partial read %d %v", n, err)
	}
	path = Path{Mode: "relay", NodeID: "selfhost-relay"}
	if n, err := conn.Write([]byte("abcd")); n != 2 || !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("partial write %d %v", n, err)
	}
	var observation int
	conn = r.Wrap(partialConn{}, b, func(string) Path {
		observation++
		if observation == 1 {
			return Path{Mode: "direct"}
		}
		return Path{Mode: "relay", NodeID: "managed-relay"}
	})
	_, _ = conn.Read(make([]byte, 8))
	var directUp, relayDown, unknownUp int64
	for _, value := range r.state.Counters {
		switch value.Report.Mode {
		case "direct":
			directUp += value.Report.UploadBytes
		case "relay":
			relayDown += value.Report.DownloadBytes
		case "unknown":
			unknownUp += value.Report.UploadBytes
		}
	}
	if directUp != 3 || relayDown != 2 || unknownUp != 3 {
		t.Fatalf("accounting direct=%d relay=%d transition=%d", directUp, relayDown, unknownUp)
	}
}

func TestOriginReverseDirections(t *testing.T) {
	r := newTestRecorder(t, &testSink{})
	c := r.Wrap(partialConn{}, Binding{AccessSessionID: "access", StreamID: "stream", Consumer: "private_http", Reverse: true}, func(string) Path { return Path{Mode: "direct"} })
	_, _ = c.Read(make([]byte, 8))
	_, _ = c.Write([]byte("abcd"))
	for _, v := range r.state.Counters {
		if v.Report.UploadBytes != 2 || v.Report.DownloadBytes != 3 {
			t.Fatalf("origin directions %+v", v.Report)
		}
	}
}

func TestOfflineRestartAbsoluteCountersAndBuckets(t *testing.T) {
	sink := &testSink{fail: true}
	path := filepath.Join(t.TempDir(), "usage.json")
	r, err := Open(path, sink, nil)
	if err != nil {
		t.Fatal(err)
	}
	b := Binding{AccessSessionID: "access", StreamID: "stream", Consumer: "terminal"}
	now := time.Now().UTC().Truncate(time.Minute)
	r.add(b, Path{Mode: "direct"}, 100, true, now)
	r.add(b, Path{Mode: "direct"}, 50, false, now.Add(time.Minute))
	if err = r.Close(context.Background()); err == nil {
		t.Fatal("offline persistence failure hidden")
	}
	sink.fail = false
	r, err = Open(path, sink, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close(context.Background())
	if err = r.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.add(b, Path{Mode: "direct"}, 25, true, now)
	if err = r.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = r.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	var up, down int64
	for _, v := range sink.reports {
		up += v.UploadBytes
		down += v.DownloadBytes
	}
	if up != 125 || down != 50 || len(sink.reports) != 2 {
		t.Fatalf("replay/buckets up=%d down=%d buckets=%d", up, down, len(sink.reports))
	}
}

func TestConcurrentBoundedCountersAndExplicitLoss(t *testing.T) {
	r := newTestRecorder(t, &testSink{})
	now := time.Now().UTC()
	b := Binding{AccessSessionID: "access", StreamID: "stream", Consumer: "terminal"}
	var workers sync.WaitGroup
	for range 8 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for range 100 {
				r.add(b, Path{Mode: "direct"}, 10, true, now)
			}
		}()
	}
	workers.Wait()
	for _, v := range r.state.Counters {
		if v.Report.UploadBytes != 8000 {
			t.Fatalf("concurrent bytes %d", v.Report.UploadBytes)
		}
	}
	r.mu.Lock()
	for len(r.state.Counters) < maxCounters {
		r.state.Counters[string(rune(len(r.state.Counters)))] = counter{Dirty: true}
	}
	r.mu.Unlock()
	r.add(Binding{AccessSessionID: "another", StreamID: "other", Consumer: "terminal"}, Path{Mode: "direct"}, 17, true, now)
	if r.Snapshot().UnrecordedBytes != 17 || len(r.state.Counters) != maxCounters {
		t.Fatal("overflow not bounded/visible")
	}
	// Artificial saturation fixture is not a valid report queue.
	r.mu.Lock()
	r.state.Counters = map[string]counter{}
	r.mu.Unlock()
}

func TestUncleanRestartAndCorruptStateMarkIncomplete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	sink := &testSink{}
	r, err := Open(path, sink, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.add(Binding{AccessSessionID: "access", StreamID: "stream", Consumer: "terminal"}, Path{Mode: "direct"}, 10, true, time.Now().UTC())
	if err = r.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Simulate process termination after durable persistence, without clean drain.
	r.cancel()
	<-r.done
	r, err = Open(path, sink, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Snapshot().Incomplete {
		t.Fatal("unclean restart presented complete accounting")
	}
	if err = r.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, []byte("interrupted JSON"), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err = Open(path, sink, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close(context.Background())
	if !r.Snapshot().Incomplete {
		t.Fatal("corrupt state presented complete accounting")
	}
	if body, err := os.ReadFile(path + ".corrupt"); err != nil || string(body) != "interrupted JSON" {
		t.Fatalf("bounded recovery evidence missing: %v", err)
	}
}

type blockingSink struct {
	entered chan struct{}
	release chan struct{}
}

func (s *blockingSink) ReportNativeUsage(ctx context.Context, _ []api.NativeUsageReport, _, _ int64, _ bool) error {
	select {
	case s.entered <- struct{}{}:
	default:
	}
	select {
	case <-s.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestFlushAndCloseRespectDeadlineBehindActiveFlush(t *testing.T) {
	sink := &blockingSink{entered: make(chan struct{}, 1), release: make(chan struct{})}
	r, err := Open(filepath.Join(t.TempDir(), "usage.json"), sink, nil)
	if err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() { finished <- r.Flush(context.Background()) }()
	<-sink.entered
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := r.Flush(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiting flush: %v", err)
	}
	if err := r.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiting close: %v", err)
	}
	r.mu.Lock()
	markedClean := r.state.CleanShutdown
	r.mu.Unlock()
	if markedClean {
		t.Fatal("expired close marked an active recorder clean")
	}
	close(sink.release)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	if err := r.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestFlushPreservesPrivateStorageAndDeliveryCauses(t *testing.T) {
	sink := &testSink{fail: true}
	r := newTestRecorder(t, sink)
	err := r.Flush(context.Background())
	var failure recorderFailure
	if !errors.As(err, &failure) || failure.DiagnosticStage() != "control_request" || err.Error() != "bandwidth reporting failed" {
		t.Fatalf("delivery classification: %v", err)
	}
	originalPath := r.path
	r.path = filepath.Join(t.TempDir(), "missing-private-directory", "usage.json")
	err = r.Flush(context.Background())
	var pathError *os.PathError
	if !errors.As(err, &pathError) || !errors.Is(err, os.ErrNotExist) || !errors.As(err, &failure) || failure.DiagnosticStage() != "diagnostic_storage" || err.Error() != "bandwidth reporting failed" {
		t.Fatalf("storage classification: %v", err)
	}
	r.path = originalPath
	sink.fail = false
	if err := r.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
}

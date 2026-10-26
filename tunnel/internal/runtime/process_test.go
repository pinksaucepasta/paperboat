package runtime

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/signal"
	"syscall"
	"testing"
	"time"
)

func TestBoundedWriterForwardsOnlyConfiguredLimit(t *testing.T) {
	var output bytes.Buffer
	writer := &boundedWriter{destination: &output, limit: 5}
	if written, err := writer.Write([]byte("1234567")); err != nil || written != 7 {
		t.Fatalf("first write = %d, %v", written, err)
	}
	if written, err := writer.Write([]byte("89")); err != nil || written != 2 {
		t.Fatalf("second write = %d, %v", written, err)
	}
	if got := output.String(); got != "12345" {
		t.Fatalf("output = %q", got)
	}
}

func TestProcessGracefulShutdown(t *testing.T) {
	process, err := NewProcess(ProcessSpec{Name: "test", Path: os.Args[0], Args: []string{"-test.run=TestProcessHelper", "--", "graceful"}, Env: append(os.Environ(), "PAPERBOAT_PROCESS_HELPER=1"), MaxOutputBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	if err := process.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := process.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestProcessCompletionIsBroadcastAndRetained(t *testing.T) {
	process, err := NewProcess(ProcessSpec{Name: "test", Path: os.Args[0], Args: []string{"-test.run=TestProcessHelper", "--", "graceful"}, Env: append(os.Environ(), "PAPERBOAT_PROCESS_HELPER=1"), MaxOutputBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	if err := process.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-process.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("child completion was not observed")
	}
	<-process.Done()
	if err := process.Err(); err != nil {
		t.Fatalf("wait error = %v", err)
	}
}

func TestProcessForcedShutdown(t *testing.T) {
	process, err := NewProcess(ProcessSpec{Name: "test", Path: os.Args[0], Args: []string{"-test.run=TestProcessHelper", "--", "ignore"}, Env: append(os.Environ(), "PAPERBOAT_PROCESS_HELPER=1"), MaxOutputBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	if err := process.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cancel()
	if err := process.Shutdown(ctx); err == nil {
		t.Fatal("forced shutdown reported success")
	}
}

func TestProcessHelper(t *testing.T) {
	if os.Getenv("PAPERBOAT_PROCESS_HELPER") != "1" {
		return
	}
	mode := os.Args[len(os.Args)-1]
	if mode == "ignore" {
		signal.Ignore(syscall.SIGTERM)
	}
	if mode == "graceful" || mode == "exit" {
		return
	}
	if mode == "recover" {
		marker := os.Getenv("PAPERBOAT_RECOVERY_MARKER")
		if _, err := os.Stat(marker); err != nil {
			_ = os.WriteFile(marker, []byte("started"), 0600)
			return
		}
	}
	select {}
}

// Code review (process.go:48): a canceled Start spawns no child, and a cancel during the startup
// grace kills and reaps the child instead of reporting it started.
func TestProcessStartHonorsCancellation(t *testing.T) {
	spec := ProcessSpec{Name: "test", Path: os.Args[0], Args: []string{"-test.run=TestProcessHelper", "--", "ignore"}, Env: append(os.Environ(), "PAPERBOAT_PROCESS_HELPER=1"), MaxOutputBytes: 1024, StartupGrace: 10 * time.Second}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	process, err := NewProcess(spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := process.Start(canceled); !errors.Is(err, context.Canceled) || process.cmd != nil {
		t.Fatalf("canceled start = %v, spawned %v", err, process.cmd != nil)
	}
	process, err = NewProcess(spec)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	started := time.Now()
	if err := process.Start(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("start canceled during the grace = %v", err)
	}
	if time.Since(started) > 5*time.Second || process.Running() {
		t.Fatalf("child left running after a canceled start (took %v)", time.Since(started))
	}
	select {
	case <-process.Done():
	default:
		t.Fatal("canceled child not reaped")
	}
}

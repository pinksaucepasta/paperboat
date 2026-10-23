//go:build load && (darwin || linux)

// Load checks for one host's terminal sessions (PLAN 3b, scale matrix): 100 concurrent real PTY sessions with
// output, one shared session with 50 viewers and a stalled viewer, input echo latency under that load, and
// that closing everything returns goroutines and file descriptors to the baseline. Run on an idle machine:
//
//	go test -tags load -run TestLoad -v ./internal/hostruntime/session/
package session

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/pty"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/store"
)

var loadSessions = loadCount("PAPERBOAT_LOAD_SESSIONS", 100)

const (
	loadViewers = 50
	// One shared session prints this many bytes as fast as the PTY allows.
	loadFloodBytes = 32 << 20
)

func loadCount(name string, fallback int) int {
	if value, err := strconv.Atoi(os.Getenv(name)); err == nil && value > 1 {
		return value
	}
	return fallback
}

func loadManager(t *testing.T) (*Manager, string) {
	t.Helper()
	root := t.TempDir()
	adapter, err := pty.NewAdapter(root)
	if err != nil {
		t.Fatal(err)
	}
	state, err := store.Open(context.Background(), store.Config{Root: filepath.Join(root, "state")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = state.Close() })
	// Production values (runtime/session_owner.go), except the session count, which is what this measures.
	manager, err := NewManager(ManagerConfig{
		Launch: func(command pty.Command) (PTYProcess, error) { return adapter.Start(command) },
		Random: rand.Reader, HistoryBytes: config.DefaultResources.HistoryBytes,
		AttachmentBytes: config.DefaultLimits.PendingOutputBytes,
		MaxSessions:     256, MaxAttachments: config.DefaultResources.MaxAttachments,
		MaxInputDecisions:  config.DefaultResources.MaxInputDecisions,
		TerminationTimeout: 10 * time.Second, TerminationGrace: 2 * time.Second,
		Store: state,
	})
	if err != nil {
		t.Fatal(err)
	}
	return manager, root
}

func openFDs() int {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return -1
	}
	return len(entries)
}

func heapMiB() float64 {
	var stats runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&stats)
	return float64(stats.HeapInuse) / (1 << 20)
}

func percentile(values []time.Duration, p float64) time.Duration {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]time.Duration(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return sorted[int(float64(len(sorted)-1)*p)]
}

// drain reads an attachment until want bytes arrived, the context ends, or the attachment fails.
func drain(ctx context.Context, manager *Manager, sessionID, attachmentID string, want int) (int, error) {
	got := 0
	for got < want {
		event, err := manager.WaitNext(ctx, sessionID, attachmentID)
		if err != nil {
			return got, err
		}
		got += len(event.Data)
	}
	return got, nil
}

func TestLoadHundredSessionsFiftyViewers(t *testing.T) {
	baseGoroutines, baseFDs, baseHeap := runtime.NumGoroutine(), openFDs(), heapMiB()
	manager, root := loadManager(t)
	shell := "/bin/sh"
	command := func(script string) pty.Command {
		return pty.Command{Path: shell, Args: []string{"-c", script}, Env: []string{"PATH=/usr/bin:/bin", "TERM=xterm"}, CWD: root, Dimensions: pty.Dimensions{Columns: 120, Rows: 40}}
	}

	// 1. 99 background sessions, each printing ~10 lines/s, plus one interactive echo session.
	start := time.Now()
	ids := make([]string, 0, loadSessions)
	for index := 0; index < loadSessions-1; index++ {
		created, err := manager.Create(context.Background(), CreateRequest{Name: fmt.Sprintf("bg-%03d", index), Command: command("i=0; while :; do i=$((i+1)); echo background line $i; sleep 0.1; done")})
		if err != nil {
			t.Fatalf("create session %d of %d: %v (counts %v)", index+1, loadSessions, err, manager.ResourceCounts())
		}
		ids = append(ids, created.ID)
	}
	echo, err := manager.Create(context.Background(), CreateRequest{Name: "echo", Command: command("stty -echo; while read line; do echo \"got $line\"; done")})
	if err != nil {
		t.Fatalf("create echo session: %v", err)
	}
	ids = append(ids, echo.ID)
	t.Logf("created %d sessions in %v; goroutines %d, fds %d, heap %.1f MiB", len(ids), time.Since(start), runtime.NumGoroutine(), openFDs(), heapMiB())

	// Every session can be attached, including the last one (F3-05: the attach at the cap was refused).
	for index, id := range ids {
		if _, err := manager.Attach(id, "owner", 0); err != nil {
			t.Fatalf("attach session %d of %d: %v", index+1, len(ids), err)
		}
	}

	// 2. Input echo latency on the echo session while the others print.
	var latencies []time.Duration
	for index := 0; index < 50; index++ {
		marker := fmt.Sprintf("m%03d", index)
		key := InputKey{ClientID: "load", AttachmentID: "owner", Generation: echo.Generation, InputSequence: uint64(index + 1)}
		sent := time.Now()
		if _, err := manager.Write(echo.ID, key, []byte(marker+"\n")); err != nil {
			t.Fatalf("input %d: %v", index, err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		var seen strings.Builder
		for !strings.Contains(seen.String(), "got "+marker) {
			event, err := manager.WaitNext(ctx, echo.ID, "owner")
			if err != nil {
				cancel()
				t.Fatalf("echo %d: %v after %v (seen %q)", index, err, time.Since(sent), seen.String())
			}
			seen.Write(event.Data)
		}
		cancel()
		latencies = append(latencies, time.Since(sent))
	}
	t.Logf("echo latency with %d busy sessions: p50 %v p95 %v max %v", len(ids)-1, percentile(latencies, .5), percentile(latencies, .95), percentile(latencies, 1))

	// 3. One shared session floods output to 50 viewers; one more viewer never reads.
	flood, err := manager.Create(context.Background(), CreateRequest{Name: "flood", Command: command(fmt.Sprintf("read go; head -c %d /dev/zero | tr '\\0' 'x'; echo; echo FLOOD-END; read done", loadFloodBytes))})
	if err == nil {
		ids = append(ids, flood.ID)
	} else {
		t.Fatalf("create flood session: %v", err)
	}
	for viewer := 0; viewer < loadViewers; viewer++ {
		if _, err := manager.Attach(flood.ID, fmt.Sprintf("viewer-%02d", viewer), 0); err != nil {
			t.Fatalf("attach viewer %d of %d: %v", viewer+1, loadViewers, err)
		}
	}
	if _, err := manager.Attach(flood.ID, "stalled", 0); err != nil {
		t.Fatalf("attach stalled viewer: %v", err)
	}
	var wg sync.WaitGroup
	var failed atomic.Int32
	results := make([]time.Duration, loadViewers)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	floodStart := time.Now()
	key := InputKey{ClientID: "load", AttachmentID: "viewer-00", Generation: flood.Generation, InputSequence: 1}
	if _, err := manager.Write(flood.ID, key, []byte("go\n")); err != nil {
		t.Fatalf("start flood: %v", err)
	}
	for viewer := 0; viewer < loadViewers; viewer++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := drain(ctx, manager, flood.ID, fmt.Sprintf("viewer-%02d", viewer), loadFloodBytes)
			if err != nil {
				failed.Add(1)
				t.Errorf("viewer %d got %d of %d bytes: %v", viewer, got, loadFloodBytes, err)
				return
			}
			results[viewer] = time.Since(floodStart)
		}()
	}
	wg.Wait()
	elapsed := time.Since(floodStart)
	if failed.Load() == 0 {
		t.Logf("flood %d MiB to %d viewers in %v (%.1f MiB/s per viewer, slowest %v); goroutines %d, heap %.1f MiB",
			loadFloodBytes>>20, loadViewers, elapsed, float64(loadFloodBytes>>20)/elapsed.Seconds(), percentile(results, 1), runtime.NumGoroutine(), heapMiB())
	}
	// The stalled viewer must have been evicted rather than holding the others back or growing memory.
	if _, _, err := manager.Next(flood.ID, "stalled"); !errors.Is(err, ErrAttachmentEvicted) {
		t.Errorf("stalled viewer after %d MiB: err=%v, want ErrAttachmentEvicted", loadFloodBytes>>20, err)
	}

	// 4. Close everything: goroutines, descriptors and heap return near the baseline.
	closeStart := time.Now()
	for _, id := range ids {
		if _, err := manager.Close(context.Background(), id); err != nil {
			t.Errorf("close %s: %v", id, errorCauses(err))
		}
	}
	shutdown, cancelShutdown := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelShutdown()
	if err := manager.Shutdown(shutdown); err != nil {
		t.Fatalf("shutdown: %v", errorCauses(err))
	}
	closed := time.Since(closeStart)
	deadline := time.Now().Add(10 * time.Second)
	for runtime.NumGoroutine() > baseGoroutines+5 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	t.Logf("closed %d sessions in %v; goroutines %d (baseline %d), fds %d (baseline %d), heap %.1f MiB (baseline %.1f)",
		len(ids), closed, runtime.NumGoroutine(), baseGoroutines, openFDs(), baseFDs, heapMiB(), baseHeap)
	if leaked := runtime.NumGoroutine() - baseGoroutines; leaked > 5 {
		buf := make([]byte, 1<<20)
		t.Errorf("%d goroutines leaked after closing every session:\n%s", leaked, buf[:runtime.Stack(buf, true)])
	}
	if leaked := openFDs() - baseFDs; leaked > 5 {
		t.Errorf("%d file descriptors leaked after closing every session", leaked)
	}
}

// errorCauses spells out every leaf under the product's user-facing wrappers.
func errorCauses(err error) string {
	var leaves []string
	var walk func(error)
	walk = func(err error) {
		switch wrapped := err.(type) {
		case interface{ Unwrap() []error }:
			for _, inner := range wrapped.Unwrap() {
				walk(inner)
			}
		case interface{ Unwrap() error }:
			if inner := wrapped.Unwrap(); inner != nil {
				walk(inner)
				return
			}
			leaves = append(leaves, err.Error())
		default:
			leaves = append(leaves, fmt.Sprintf("%v (%T)", err, err))
		}
	}
	walk(err)
	return strings.Join(leaves, "; ")
}

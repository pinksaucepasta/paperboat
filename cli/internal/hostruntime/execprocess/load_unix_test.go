//go:build load && (darwin || linux)

// Load checks for remote exec on one host (PLAN 3b, scale matrix): the 32 concurrent executions the host allows,
// all printing output into the durable store, a clear capacity error for the next one, then 2,000 short executions
// through 32 workers (churn past the 1,024 retained operations), and no goroutine or descriptor leak afterwards.
//
//	go test -tags load -run TestLoad -v ./internal/hostruntime/execprocess/
package execprocess

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/store"
)

func execLoadFDs() int {
	entries, _ := os.ReadDir("/proc/self/fd")
	return len(entries)
}

func execCauses(err error) string {
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

func TestLoadExecConcurrencyAndChurn(t *testing.T) {
	baseGoroutines, baseFDs := runtime.NumGoroutine(), execLoadFDs()
	root := t.TempDir()
	durable, err := store.Open(context.Background(), store.Config{Root: filepath.Join(root, "state")})
	if err != nil {
		t.Fatal(err)
	}
	defer durable.Close()
	resources := config.DefaultResources
	// Production values (runtime/host.go).
	manager, err := New(Config{
		WorkspaceRoot: root, BaseEnvironment: []string{"PATH=/usr/bin:/bin", "LANG=C"},
		MaximumActive: resources.MaxConcurrentOps, MaximumOperations: resources.MaxConcurrentOps * 32,
		ReplayBytes: int(config.DefaultLimits.PendingOutputBytes), CancelGrace: 2 * time.Second, Store: durable,
	})
	if err != nil {
		t.Fatal(err)
	}
	// The production stream path (server/terminal_stream.go): a reader with explicit release, which is what lets
	// the execution drop output the reader has taken.
	drainAll := func(execution ExecutionService) (int, error) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		reader, err := execution.OpenReader(1)
		if err != nil {
			return 0, err
		}
		defer reader.Close()
		got := 0
		for {
			event, release, err := reader.Next(ctx)
			if errors.Is(err, io.EOF) {
				return got, nil
			}
			if err != nil {
				return got, err
			}
			got += len(event.Data)
			release()
		}
	}

	// 1. Every allowed slot busy with output, then one more.
	const outputBytes = 4 << 20
	started := time.Now()
	var running []ExecutionService
	for index := 0; index < resources.MaxConcurrentOps; index++ {
		execution, _, err := manager.Start(context.Background(), Request{OperationID: fmt.Sprintf("operation_busy_%02d", index), Argv: []string{"/bin/sh", "-c", fmt.Sprintf("head -c %d /dev/zero | tr '\\0' x; sleep 2", outputBytes)}, CWD: root})
		if err != nil {
			t.Fatalf("start %d of %d: %v", index+1, resources.MaxConcurrentOps, execCauses(err))
		}
		running = append(running, execution)
	}
	overflowStart := time.Now()
	_, _, err = manager.Start(context.Background(), Request{OperationID: "operation_overflow", Argv: []string{"/bin/true"}, CWD: root})
	t.Logf("%d busy executions started in %v; the next one: %v after %v", len(running), overflowStart.Sub(started), err, time.Since(overflowStart))
	if !errors.Is(err, ErrCapacity) {
		t.Errorf("execution %d: err=%v, want ErrCapacity", resources.MaxConcurrentOps+1, err)
	}
	var wg sync.WaitGroup
	var outputFailures atomic.Int32
	for index, execution := range running {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := drainAll(execution)
			if err != nil || got != outputBytes {
				outputFailures.Add(1)
				t.Errorf("busy execution %d: %d of %d bytes: %v", index, got, outputBytes, execCauses(err))
			}
		}()
	}
	wg.Wait()
	t.Logf("%d × %d MiB output delivered in %v", len(running), outputBytes>>20, time.Since(started))

	// 2. Churn: 2,000 short executions through as many workers as slots.
	const churn = 2000
	var next, failed atomic.Int32
	var mu sync.Mutex
	var latencies []time.Duration
	failures := map[string]int{}
	churnStart := time.Now()
	for worker := 0; worker < resources.MaxConcurrentOps; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				index := next.Add(1)
				if index > churn {
					return
				}
				began := time.Now()
				execution, _, err := manager.Start(context.Background(), Request{OperationID: fmt.Sprintf("operation_churn_%04d", index), Argv: []string{"/bin/echo", "ok"}, CWD: root})
				if err == nil {
					_, err = execution.Wait(context.Background())
				}
				if err == nil {
					_, err = drainAll(execution)
				}
				mu.Lock()
				if err != nil {
					failed.Add(1)
					failures[execCauses(err)]++
				} else {
					latencies = append(latencies, time.Since(began))
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(churnStart)
	slices.Sort(latencies)
	p := func(q float64) time.Duration {
		if len(latencies) == 0 {
			return 0
		}
		return latencies[int(float64(len(latencies)-1)*q)].Round(time.Millisecond)
	}
	t.Logf("churn: %d executions in %v (%.0f/s), p50 %v p99 %v, %d failed %v", churn, elapsed.Round(time.Millisecond), float64(churn)/elapsed.Seconds(), p(.5), p(.99), failed.Load(), failures)
	if failed.Load() > 0 {
		t.Errorf("%d of %d short executions failed: %v", failed.Load(), churn, failures)
	}

	// 3. Shutdown leaves nothing behind.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := manager.Shutdown(ctx); err != nil {
		t.Errorf("shutdown: %v", execCauses(err))
	}
	deadline := time.Now().Add(10 * time.Second)
	for runtime.NumGoroutine() > baseGoroutines+5 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	t.Logf("after shutdown: goroutines %d (baseline %d), fds %d (baseline %d)", runtime.NumGoroutine(), baseGoroutines, execLoadFDs(), baseFDs)
	if leaked := runtime.NumGoroutine() - baseGoroutines; leaked > 5 {
		t.Errorf("%d goroutines leaked", leaked)
	}
	if leaked := execLoadFDs() - baseFDs; leaked > 5 {
		t.Errorf("%d file descriptors leaked", leaked)
	}
}

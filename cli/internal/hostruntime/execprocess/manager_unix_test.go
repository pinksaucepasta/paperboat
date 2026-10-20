//go:build darwin || linux

package execprocess

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/pty"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/store"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
)

func testManager(t *testing.T, root string, configure func(*Config)) *Manager {
	t.Helper()
	config := Config{WorkspaceRoot: root, BaseEnvironment: []string{"PATH=/usr/bin:/bin", "LANG=C"}, MaximumActive: 4, ReplayBytes: 64 << 10, ChunkBytes: 1024, CancelGrace: 20 * time.Millisecond}
	if configure != nil {
		configure(&config)
	}
	manager, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	return manager
}

func collect(t *testing.T, execution ExecutionService) []Event {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var events []Event
	for sequence := uint64(1); ; sequence++ {
		event, err := execution.Next(ctx, sequence)
		if errors.Is(err, io.EOF) {
			return events
		}
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
}

func TestExactArgvCWDEnvironmentAndSeparateStreams(t *testing.T) {
	root := t.TempDir()
	manager := testManager(t, root, nil)
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	request := Request{OperationID: "operation_exact", Argv: []string{"/bin/sh", "-c", `printf '%s|%s' "$PWD" "$VALUE"; printf error >&2; exit 7`}, CWD: root, Environment: map[string]string{"VALUE": "a b"}}
	execution, replay, err := manager.Start(context.Background(), request)
	if err != nil || replay {
		t.Fatalf("replay=%v err=%v", replay, err)
	}
	snapshot, err := execution.Wait(context.Background())
	if err != nil || snapshot.State != StateExited || snapshot.Result == nil || snapshot.Result.Code != 7 {
		t.Fatalf("snapshot=%#v err=%v", snapshot, err)
	}
	events := collect(t, execution)
	var stdout, stderr strings.Builder
	for _, event := range events {
		if event.Stream == "stdout" {
			stdout.Write(event.Data)
		}
		if event.Stream == "stderr" {
			stderr.Write(event.Data)
		}
	}
	if stdout.String() != resolvedRoot+"|a b" || stderr.String() != "error" {
		t.Fatalf("stdout=%q stderr=%q events=%#v", stdout.String(), stderr.String(), events)
	}
}

func TestManagedEnvironmentIsResolvedForEachNewProcess(t *testing.T) {
	root := t.TempDir()
	value := "first"
	manager := testManager(t, root, func(config *Config) {
		config.ManagedEnvironment = func(context.Context) ([]string, error) {
			return []string{"INJECTED=" + value, "OVERRIDE=managed"}, nil
		}
	})
	run := func(operationID string, override map[string]string) string {
		t.Helper()
		execution, _, err := manager.Start(context.Background(), Request{OperationID: operationID, Argv: []string{"/bin/sh", "-c", `printf '%s|%s' "$INJECTED" "$OVERRIDE"`}, CWD: root, Environment: override})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := execution.Wait(context.Background()); err != nil {
			t.Fatal(err)
		}
		var output strings.Builder
		for _, event := range collect(t, execution) {
			if event.Stream == "stdout" {
				output.Write(event.Data)
			}
		}
		return output.String()
	}
	if got := run("operation_managed_first", nil); got != "first|managed" {
		t.Fatalf("first environment=%q", got)
	}
	value = "second"
	if got := run("operation_managed_second", map[string]string{"override": "request"}); got != "second|request" {
		t.Fatalf("second environment=%q", got)
	}
}

func TestRequestRejectsCaseFoldedEnvironmentDuplicates(t *testing.T) {
	root := t.TempDir()
	manager := testManager(t, root, nil)
	_, _, err := manager.Start(context.Background(), Request{OperationID: "operation_duplicate_environment", Argv: []string{"/bin/true"}, CWD: root, Environment: map[string]string{"Token": "one", "TOKEN": "two"}})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("error=%v", err)
	}
}

func TestManagedEnvironmentFailureFailsClosed(t *testing.T) {
	root := t.TempDir()
	manager := testManager(t, root, func(config *Config) {
		config.ManagedEnvironment = func(context.Context) ([]string, error) { return nil, errors.New("secret detail") }
	})
	execution, _, err := manager.Start(context.Background(), Request{OperationID: "operation_managed_unavailable", Argv: []string{"/bin/true"}, CWD: root})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := execution.Wait(context.Background())
	if err != nil || snapshot.State != StateFailed || snapshot.ErrorCode != "environment_unavailable" || strings.Contains(snapshot.ErrorCode, "secret detail") {
		t.Fatalf("snapshot=%#v err=%v", snapshot, err)
	}
}

func TestManagedEnvironmentTimeoutRemainsCancellation(t *testing.T) {
	var faults []errorreport.Fault
	restore := errorreport.InstallFaultObserver(func(_ context.Context, fault errorreport.Fault) {
		faults = append(faults, fault)
	})
	defer restore()
	root := t.TempDir()
	manager := testManager(t, root, func(config *Config) {
		config.ManagedEnvironment = func(ctx context.Context) ([]string, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}
	})
	execution, _, err := manager.Start(context.Background(), Request{OperationID: "operation_environment_timeout", Argv: []string{"/bin/true"}, CWD: root, Timeout: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := execution.Wait(context.Background())
	if err != nil || snapshot.State != StateCanceled || snapshot.ErrorCode != "exec_timeout" {
		t.Fatalf("snapshot=%#v err=%v", snapshot, err)
	}
	if len(faults) != 0 {
		t.Fatalf("timeout emitted failure observations: %+v", faults)
	}
}

func TestWaitStartedCancelsWithoutCancelingAdmittedExecution(t *testing.T) {
	root := t.TempDir()
	entered, release := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	manager := testManager(t, root, func(config *Config) {
		config.ManagedEnvironment = func(ctx context.Context) ([]string, error) {
			close(entered)
			select {
			case <-release:
				return nil, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	execution, _, err := manager.Start(ctx, Request{OperationID: "operation_start_ready", Argv: []string{"/bin/sh", "-c", "cat > received"}, CWD: root})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("managed launch did not start", ctx.Err())
	}
	stopped, stop := context.WithCancel(ctx)
	stop()
	if _, err := execution.WaitStarted(stopped); !errors.Is(err, context.Canceled) {
		t.Fatal("readiness cancellation did not return its cause", err)
	}
	close(release)
	if snapshot, err := execution.WaitStarted(ctx); err != nil || snapshot.State != StateRunning {
		t.Fatal("admitted execution was not ready after launch", snapshot.State, err)
	}
	if _, err := execution.Write([]byte("exact-input")); err != nil {
		t.Fatal(err)
	}
	if err := execution.CloseInput(); err != nil {
		t.Fatal(err)
	}
	if snapshot, err := execution.Wait(ctx); err != nil || snapshot.State != StateExited || snapshot.Result.Code != 0 {
		t.Fatal("readiness cancellation changed the remote outcome", err)
	}
	if received, err := os.ReadFile(filepath.Join(root, "received")); err != nil || string(received) != "exact-input" {
		t.Fatal("ready execution lost its input", err)
	}
}

func TestExecExitObservationKeepsResultAndOriginalReference(t *testing.T) {
	var faults []errorreport.Fault
	restore := errorreport.InstallFaultObserver(func(_ context.Context, fault errorreport.Fault) {
		faults = append(faults, fault)
	})
	defer restore()
	root := t.TempDir()
	reference := supportref.New()
	ctx := supportref.WithContext(context.Background(), reference)
	manager := testManager(t, root, nil)
	execution, _, err := manager.Start(ctx, Request{OperationID: "operation_private_user_label", Argv: []string{"/bin/sh", "-c", "exit 9"}, CWD: root})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := execution.Wait(context.Background())
	if err != nil || snapshot.State != StateExited || snapshot.ErrorCode != "" || snapshot.Result == nil || snapshot.Result.Code != 9 {
		t.Fatalf("snapshot=%#v err=%v", snapshot, err)
	}
	if len(faults) != 1 {
		t.Fatalf("fault observations=%+v, want one bounded exit attempt", faults)
	}
	fault := faults[0]
	if fault.Component != "paperboat-daemon" || fault.Operation != "exec" || fault.Stage != "command" || fault.Code != "exec_operation_failed" || fault.SupportReference != reference || fault.ErrorType != "commandExitFailure" {
		t.Fatalf("fault=%+v", fault)
	}
	if strings.Contains(fault.ErrorType+fault.Operation+fault.Stage+fault.Code, "private_user_label") {
		t.Fatal("operation label entered local fault metadata")
	}
}

func TestExecPersistenceFailureHasSafeTextAndPreservesCause(t *testing.T) {
	var faults []errorreport.Fault
	restore := errorreport.InstallFaultObserver(func(_ context.Context, fault errorreport.Fault) {
		faults = append(faults, fault)
	})
	defer restore()
	root := t.TempDir()
	durable, err := store.Open(context.Background(), store.Config{Root: filepath.Join(root, "state")})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := NewPersistent(context.Background(), Config{WorkspaceRoot: root, BaseEnvironment: []string{"PATH=/usr/bin:/bin"}, MaximumActive: 2, MaximumOperations: 16, ReplayBytes: 64 << 10, Store: durable})
	if err != nil {
		t.Fatal(err)
	}
	reference := supportref.New()
	ctx := supportref.WithContext(context.Background(), reference)
	execution, _, err := manager.Start(ctx, Request{OperationID: "operation_private_storage_label", Argv: []string{"/bin/sh", "-c", "sleep 0.2"}, CWD: root})
	if err != nil {
		t.Fatal(err)
	}
	if err := durable.Close(); err != nil {
		t.Fatal(err)
	}
	snapshot, waitErr := execution.Wait(context.Background())
	if waitErr == nil || snapshot.ErrorCode != "persistence_failed" {
		t.Fatalf("snapshot=%#v wait error=%v", snapshot, waitErr)
	}
	var failure *persistenceFailure
	if !errors.As(waitErr, &failure) || failure.Unwrap() == nil || waitErr.Error() != "exec operation result persistence failed" {
		t.Fatalf("unsafe or unwrapped persistence failure: %T %v", waitErr, waitErr)
	}
	if len(faults) != 1 {
		t.Fatalf("fault observations=%+v, want one persistence failure", faults)
	}
	fault := faults[0]
	if fault.Operation != "exec" || fault.Stage != "lifecycle" || fault.Code != "exec_persistence_failed" || fault.SupportReference != reference {
		t.Fatalf("fault=%+v", fault)
	}
	encoded, err := json.Marshal(fault)
	if err != nil || strings.Contains(string(encoded), "operation_private_storage_label") || strings.Contains(string(encoded), "database is closed") {
		t.Fatalf("raw persistence details leaked: fault=%s err=%v", encoded, err)
	}
}

func TestPipeProcessPreservesSymlinkInvocationName(t *testing.T) {
	root := t.TempDir()
	command := filepath.Join(root, "sh")
	if err := os.Symlink("/bin/sh", command); err != nil {
		t.Fatal(err)
	}
	manager := testManager(t, root, nil)
	execution, _, err := manager.Start(context.Background(), Request{OperationID: "operation_symlink_argv0", Argv: []string{command, "-c", `printf %s "$0"`}, CWD: root})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := execution.Wait(context.Background())
	if err != nil || snapshot.Result == nil || snapshot.Result.Code != 0 {
		t.Fatalf("snapshot=%#v err=%v", snapshot, err)
	}
	var stdout strings.Builder
	for _, event := range collect(t, execution) {
		if event.Stream == "stdout" {
			stdout.Write(event.Data)
		}
	}
	if stdout.String() != command {
		t.Fatalf("stdout=%q, want invocation %q", stdout.String(), command)
	}
}

func TestImmediateExitPublishesAllOutputBeforeTerminalState(t *testing.T) {
	root := t.TempDir()
	manager := testManager(t, root, nil)
	for index := range 100 {
		execution, _, err := manager.Start(context.Background(), Request{OperationID: fmt.Sprintf("operation_drain_%d", index), Argv: []string{"/bin/sh", "-c", `printf out; printf err >&2`}, CWD: root})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := execution.Wait(context.Background()); err != nil {
			t.Fatal(err)
		}
		events := collect(t, execution)
		var stdout, stderr strings.Builder
		for _, event := range events {
			if event.Stream == "stdout" {
				stdout.Write(event.Data)
			}
			if event.Stream == "stderr" {
				stderr.Write(event.Data)
			}
		}
		if stdout.String() != "out" || stderr.String() != "err" {
			t.Fatalf("iteration=%d stdout=%q stderr=%q events=%#v", index, stdout.String(), stderr.String(), events)
		}
	}
}

func TestOperationRetryDoesNotStartSecondProcessAndConflictFails(t *testing.T) {
	root := t.TempDir()
	marker := filepath.Join(root, "marker")
	manager := testManager(t, root, nil)
	request := Request{OperationID: "operation_retry", Argv: []string{"/bin/sh", "-c", `printf x >> "$1"`, "sh", marker}, CWD: root}
	first, replay, err := manager.Start(context.Background(), request)
	if err != nil || replay {
		t.Fatalf("replay=%v err=%v", replay, err)
	}
	second, replay, err := manager.Start(context.Background(), request)
	if err != nil || !replay || first != second {
		t.Fatalf("same=%v replay=%v err=%v", first == second, replay, err)
	}
	if _, err := first.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(marker)
	if err != nil || string(data) != "x" {
		t.Fatalf("marker=%q err=%v", data, err)
	}
	request.Argv = []string{"/bin/echo", "different"}
	if _, _, err := manager.Start(context.Background(), request); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflict err=%v", err)
	}
}

func TestTimeoutTerminatesProcessGroupAndReportsCanceled(t *testing.T) {
	root := t.TempDir()
	manager := testManager(t, root, nil)
	execution, _, err := manager.Start(context.Background(), Request{OperationID: "operation_timeout", Argv: []string{"/bin/sh", "-c", `trap '' TERM; while :; do sleep 1; done`}, CWD: root, Timeout: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	snapshot, err := execution.Wait(ctx)
	if err != nil || snapshot.State != StateCanceled || snapshot.ErrorCode != "exec_timeout" || snapshot.Result == nil {
		t.Fatalf("snapshot=%#v err=%v", snapshot, err)
	}
}

func TestImmediateCancellationCannotLeaveStartingProcessRunning(t *testing.T) {
	root := t.TempDir()
	marker := filepath.Join(root, "late-marker")
	manager := testManager(t, root, nil)
	execution, _, err := manager.Start(context.Background(), Request{OperationID: "operation_cancel_start", Argv: []string{"/bin/sh", "-c", `sleep 0.2; printf late > "$1"`, "sh", marker}, CWD: root})
	if err != nil {
		t.Fatal(err)
	}
	cancelCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	if err := execution.Cancel(cancelCtx); err != nil {
		cancel()
		t.Fatal(err)
	}
	cancel()
	snapshot, err := execution.Wait(context.Background())
	if err != nil || snapshot.State != StateCanceled {
		t.Fatalf("snapshot=%#v err=%v", snapshot, err)
	}
	time.Sleep(300 * time.Millisecond)
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled process published marker: %v", err)
	}
}

func TestPTYMergesOutputAndReportsExit(t *testing.T) {
	root := t.TempDir()
	manager := testManager(t, root, nil)
	execution, _, err := manager.Start(context.Background(), Request{OperationID: "operation_pty", Argv: []string{"/bin/sh", "-c", `printf out; printf err >&2; exit 9`}, CWD: root, PTY: true, Dimensions: pty.Dimensions{Columns: 80, Rows: 24}})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := execution.Wait(context.Background())
	if err != nil || snapshot.Result == nil || snapshot.Result.Code != 9 {
		t.Fatalf("snapshot=%#v err=%v", snapshot, err)
	}
	events := collect(t, execution)
	var output strings.Builder
	for _, event := range events {
		if event.Stream == "pty" {
			output.Write(event.Data)
		}
		if event.Stream == "stdout" || event.Stream == "stderr" {
			t.Fatalf("PTY emitted split stream: %#v", event)
		}
	}
	if !strings.Contains(output.String(), "out") || !strings.Contains(output.String(), "err") {
		t.Fatalf("output=%q", output.String())
	}
}

// Output nobody has read yet is never dropped: a client that attaches after the buffer filled (a slow
// dial, a reconnect) gets every byte, and the process waits meanwhile instead of losing output.
func TestUnreadOutputIsNeverEvictedWhileNoReaderIsAttached(t *testing.T) {
	root := t.TempDir()
	manager := testManager(t, root, func(config *Config) { config.ReplayBytes = 64 << 10; config.ChunkBytes = 1024 })
	execution, _, err := manager.Start(context.Background(), Request{OperationID: "operation_unread", Argv: []string{"/bin/sh", "-c", `head -c 131072 /dev/zero`}, CWD: root})
	if err != nil {
		t.Fatal(err)
	}
	waitCtx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if _, err := execution.Wait(waitCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the process finished although no client had read its output: %v", err)
	}
	reader, err := execution.OpenReader(1)
	if err != nil {
		t.Fatalf("unread output was evicted: %v", err)
	}
	defer reader.Close()
	if got := readAll(t, reader); got != 131072 {
		t.Fatalf("bytes=%d", got)
	}
}

// Output a client has read stays available for reconnect only within the replay bound.
func TestReplayBoundRejectsEvictedSequenceOnceRead(t *testing.T) {
	root := t.TempDir()
	manager := testManager(t, root, func(config *Config) { config.ReplayBytes = 64 << 10; config.ChunkBytes = 1024 })
	execution, _, err := manager.Start(context.Background(), Request{OperationID: "operation_replay", Argv: []string{"/bin/sh", "-c", `head -c 131072 /dev/zero`}, CWD: root})
	if err != nil {
		t.Fatal(err)
	}
	reader, err := execution.OpenReader(1)
	if err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, reader); got != 131072 {
		t.Fatalf("bytes=%d", got)
	}
	_ = reader.Close()
	if _, err := execution.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := execution.Next(context.Background(), 1); !errors.Is(err, ErrReplayUnavailable) {
		t.Fatalf("replay err=%v", err)
	}
}

// A command printing far more than the replay buffer reaches a slow reader whole: the process waits
// for the reader (as a pipe writer does) and nothing is dropped, even across a detach and reattach.
func TestLargeOutputReachesASlowReaderAcrossAReattach(t *testing.T) {
	root := t.TempDir()
	manager := testManager(t, root, func(config *Config) { config.ReplayBytes = 1 << 20; config.ChunkBytes = 32 << 10 })
	const total = 8 << 20
	execution, _, err := manager.Start(context.Background(), Request{OperationID: "operation_large", Argv: []string{"/bin/sh", "-c", `head -c 8388608 /dev/zero | tr '\0' x`}, CWD: root})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond) // the client attaches late: the buffer is already full
	var received int
	next := uint64(1)
	for attach := 0; ; attach++ {
		reader, err := execution.OpenReader(next)
		if err != nil {
			t.Fatalf("attach %d from %d: %v", attach, next, err)
		}
		done := false
		for step := 0; step < 40 && !done; step++ {
			event, release, nextErr := reader.Next(context.Background())
			if errors.Is(nextErr, io.EOF) {
				done = true
				break
			}
			if nextErr != nil {
				t.Fatal(nextErr)
			}
			received += len(event.Data)
			next = event.Sequence + 1
			release()
			time.Sleep(time.Millisecond) // a slow consumer
			if terminalState(event.State) {
				done = true
			}
		}
		_ = reader.Close() // detach; the client reconnects with its own next sequence
		if done {
			break
		}
	}
	if received != total {
		t.Fatalf("received %d of %d bytes", received, total)
	}
}

func readAll(t *testing.T, reader ReaderService) int {
	t.Helper()
	var total int
	for {
		event, release, err := reader.Next(context.Background())
		if errors.Is(err, io.EOF) {
			return total
		}
		if err != nil {
			t.Fatal(err)
		}
		total += len(event.Data)
		release()
		if terminalState(event.State) {
			return total
		}
	}
}

func TestLiveReaderBackpressuresInsteadOfEvictingUnreadOutput(t *testing.T) {
	root := t.TempDir()
	manager := testManager(t, root, func(config *Config) { config.ReplayBytes = 64 << 10; config.ChunkBytes = 1024 })
	execution, _, err := manager.Start(context.Background(), Request{OperationID: "operation_live_backpressure", Argv: []string{"/bin/sh", "-c", `head -c 262144 /dev/zero`}, CWD: root})
	if err != nil {
		t.Fatal(err)
	}
	reader, err := execution.OpenReader(1)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	var bytesRead int
	for {
		event, release, nextErr := reader.Next(context.Background())
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			t.Fatal(nextErr)
		}
		bytesRead += len(event.Data)
		release()
		if terminalState(event.State) {
			break
		}
	}
	if bytesRead != 262144 {
		t.Fatalf("bytes=%d", bytesRead)
	}
}

func TestValidationRejectsEscapedCWDInvalidEnvironmentAndImplicitDimensions(t *testing.T) {
	root := t.TempDir()
	manager := testManager(t, root, nil)
	requests := []Request{
		{OperationID: "operation_bad_cwd", Argv: []string{"/bin/true"}, CWD: filepath.Dir(root)},
		{OperationID: "operation_bad_env", Argv: []string{"/bin/true"}, CWD: root, Environment: map[string]string{"BAD-KEY": "x"}},
		{OperationID: "operation_bad_size", Argv: []string{"/bin/true"}, CWD: root, Dimensions: pty.Dimensions{Columns: 80, Rows: 24}},
	}
	for _, request := range requests {
		if _, _, err := manager.Start(context.Background(), request); !errors.Is(err, ErrInvalid) {
			t.Fatalf("request=%#v err=%v", request, err)
		}
	}
}

func TestPersistentCompletedOperationReplaysWithoutRerun(t *testing.T) {
	root := t.TempDir()
	durable, err := store.Open(context.Background(), store.Config{Root: filepath.Join(root, "state")})
	if err != nil {
		t.Fatal(err)
	}
	defer durable.Close()
	config := Config{WorkspaceRoot: root, BaseEnvironment: []string{"PATH=/usr/bin:/bin"}, MaximumActive: 2, MaximumOperations: 16, ReplayBytes: 64 << 10, Store: durable}
	first, err := NewPersistent(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "marker")
	request := Request{OperationID: "operation_persisted", Argv: []string{"/bin/sh", "-c", `printf x >> "$1"; exit 6`, "sh", marker}, CWD: root}
	execution, replay, err := first.Start(context.Background(), request)
	if err != nil || replay {
		t.Fatalf("replay=%v err=%v", replay, err)
	}
	if snapshot, err := execution.Wait(context.Background()); err != nil || snapshot.Result == nil || snapshot.Result.Code != 6 {
		t.Fatalf("snapshot=%#v err=%v", snapshot, err)
	}
	second, err := NewPersistent(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	recovered, replay, err := second.Start(context.Background(), request)
	if err != nil || !replay {
		t.Fatalf("replay=%v err=%v", replay, err)
	}
	snapshot := recovered.Snapshot()
	if snapshot.State != StateExited || snapshot.Result == nil || snapshot.Result.Code != 6 {
		t.Fatalf("snapshot=%#v", snapshot)
	}
	data, err := os.ReadFile(marker)
	if err != nil || string(data) != "x" {
		t.Fatalf("marker=%q err=%v", data, err)
	}
}

func TestPersistentPendingOperationBecomesRestartFailure(t *testing.T) {
	root := t.TempDir()
	durable, err := store.Open(context.Background(), store.Config{Root: filepath.Join(root, "state")})
	if err != nil {
		t.Fatal(err)
	}
	defer durable.Close()
	config := Config{WorkspaceRoot: root, BaseEnvironment: []string{"PATH=/usr/bin:/bin"}, MaximumActive: 2, MaximumOperations: 16, ReplayBytes: 64 << 10, Store: durable}
	validator := testManager(t, root, nil)
	request := Request{OperationID: "operation_pending", Argv: []string{"/bin/true"}, CWD: root}
	_, hash, err := validator.validate(request)
	if err != nil {
		t.Fatal(err)
	}
	if _, inserted, err := durable.ReserveOperation(context.Background(), persistencePrefix+request.OperationID, hash[:], time.Now().UTC().Add(time.Hour)); err != nil || !inserted {
		t.Fatalf("inserted=%v err=%v", inserted, err)
	}
	manager, err := NewPersistent(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	execution, replay, err := manager.Start(context.Background(), request)
	if err != nil || !replay {
		t.Fatalf("replay=%v err=%v", replay, err)
	}
	snapshot := execution.Snapshot()
	if snapshot.State != StateFailed || snapshot.ErrorCode != "exec_start_uncertain" || snapshot.Result == nil {
		t.Fatalf("snapshot=%#v", snapshot)
	}
	records, err := durable.OperationsWithPrefix(context.Background(), persistencePrefix, time.Now().UTC(), 16)
	if err != nil || len(records) != 1 || records[0].State != "completed" || records[0].ErrorCode != "exec_start_uncertain" {
		t.Fatalf("records=%#v err=%v", records, err)
	}
}

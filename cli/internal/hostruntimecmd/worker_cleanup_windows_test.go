//go:build windows

package hostruntimecmd

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"golang.org/x/sys/windows"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestWindowsWorkerActivationBorrowedPipeCancellationAndRecovery(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	ctx, cancel := context.WithCancelCause(context.Background())
	cause := errors.New("private activation cancellation")
	cancel(cause)
	if err := awaitWindowsWorkerActivation(ctx, reader); !errors.Is(err, context.Canceled) || !errors.Is(err, cause) {
		t.Fatalf("cancellation cause lost: %T", err)
	}
	sent := make(chan error, 1)
	go func() { _, err := io.WriteString(writer, "activate\n"); sent <- err }()
	recoveryCtx, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	if err := awaitWindowsWorkerActivation(recoveryCtx, reader); err != nil {
		t.Fatalf("borrowed pipe recovery: %T", err)
	}
	if err := <-sent; err != nil {
		t.Fatal(err)
	}
}

func TestWindowsWorkerOwnedOutputCancellationAndConcurrentReap(t *testing.T) {
	command := exec.Command(os.Args[0], "-test.run=^TestWindowsWorkerOutputHelper$")
	command.Env = append(os.Environ(), "PAPERBOAT_WORKER_OUTPUT_HELPER=1")
	command.WaitDelay = time.Second
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	control, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = control.Close(); _ = command.Process.Kill() }()
	worker := &windowsRuntimeWorker{command: command, stdout: stdout, lines: bufio.NewReader(stdout), workerID: "owned-output"}
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = worker.Stop(stopCtx)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := worker.Ready(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("owned read cancellation: %T", err)
	}
	if err := control.Close(); err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	results := make(chan error, 2)
	for n := 0; n < 2; n++ {
		group.Add(1)
		go func() {
			defer group.Done()
			stopCtx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			results <- worker.Stop(stopCtx)
		}()
	}
	group.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatalf("concurrent cleanup: %T", err)
		}
	}
	if worker.command.ProcessState == nil || !worker.command.ProcessState.Exited() {
		t.Fatal("worker was not reaped")
	}
}
func TestWindowsWorkerOutputHelper(t *testing.T) {
	if os.Getenv("PAPERBOAT_WORKER_OUTPUT_HELPER") != "1" {
		return
	}
	_, _ = io.WriteString(os.Stdout, "ready ")
	_, _ = io.Copy(io.Discard, os.Stdin)
	os.Exit(0)
}

func TestWindowsRuntimeTokenNativeFailureAndFreshRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hostd.token")
	if _, err := readWindowsHostdToken(path); !errors.Is(err, os.ErrNotExist) || strings.Contains(err.Error(), path) {
		t.Fatalf("missing-token cause/privacy: %T", err)
	}
	// The detailed ACL probe must retain a real Win32 lookup failure.
	if _, err := checkWindowsHostdTokenACLForSID(path, "S-1-5-18"); !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
		t.Fatalf("ACL probe cause: %T", err)
	}
	want := bytes.Repeat([]byte{0x42}, 32)
	if err := os.WriteFile(path, want, 0600); err != nil {
		t.Fatal(err)
	}
	if err := setWindowsHostdTokenACL(path); err != nil {
		t.Fatal(err)
	}
	got, err := readWindowsHostdToken(path)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("protected token recovery: %T", err)
	}
	staged, cleanup, err := stageWindowsElevationExecutable(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(staged); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("temporary executable retained")
	}
	if _, _, err := stageWindowsElevationExecutable(filepath.Join(filepath.Dir(path), "absent")); !errors.Is(err, os.ErrNotExist) || strings.Contains(err.Error(), filepath.Dir(path)) {
		t.Fatalf("staging cause/privacy: %T", err)
	}
}

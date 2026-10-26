//go:build darwin || linux

package pty

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func shellPath(t *testing.T) string {
	t.Helper()
	for _, path := range []string{"/bin/sh", "/usr/bin/sh"} {
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	t.Fatal("no test shell")
	return ""
}
func startShell(t *testing.T, script string) (*Process, string) {
	t.Helper()
	root := t.TempDir()
	adapter, err := NewAdapter(root)
	if err != nil {
		t.Fatal(err)
	}
	process, err := adapter.Start(Command{Path: shellPath(t), Args: []string{"-c", script}, Env: []string{"PATH=/usr/bin:/bin", "TERM=xterm"}, CWD: root, Dimensions: Dimensions{80, 24}})
	if err != nil {
		t.Fatal(err)
	}
	return process, root
}

func TestRealPTYReportsOutputCWDAndExit(t *testing.T) {
	process, root := startShell(t, "pwd; printf hello; exit 7")
	output, readErr := io.ReadAll(process)
	if readErr != nil {
		t.Fatal(readErr)
	}
	result, err := process.Wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer process.CloseIO()
	if result.Code != 7 || !strings.Contains(string(output), root) || !strings.Contains(string(output), "hello") {
		t.Fatalf("result=%#v output=%q", result, output)
	}
}

func TestRealPTYPreservesSymlinkInvocationName(t *testing.T) {
	root := t.TempDir()
	command := filepath.Join(root, "sh")
	if err := os.Symlink(shellPath(t), command); err != nil {
		t.Fatal(err)
	}
	adapter, err := NewAdapter(root)
	if err != nil {
		t.Fatal(err)
	}
	process, err := adapter.Start(Command{Path: command, Args: []string{"-c", `printf %s "$0"`}, Env: []string{"PATH=/usr/bin:/bin", "TERM=xterm"}, CWD: root, Dimensions: Dimensions{80, 24}})
	if err != nil {
		t.Fatal(err)
	}
	output, readErr := io.ReadAll(process)
	if readErr != nil {
		t.Fatal(readErr)
	}
	result, err := process.Wait(context.Background())
	defer process.CloseIO()
	if err != nil || result.Code != 0 || !strings.Contains(string(output), command) {
		t.Fatalf("result=%#v error=%v output=%q, want invocation %q", result, err, output, command)
	}
}

func TestRealPTYInitialSizeAndResize(t *testing.T) {
	process, _ := startShell(t, "stty size; echo resize-ready; read line; stty size")
	reader := bufio.NewReader(process)
	first, err := reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	marker, err := reader.ReadString('\n')
	if err != nil || !strings.Contains(marker, "resize-ready") {
		t.Fatalf("marker=%q err=%v", marker, err)
	}
	if err := process.Resize(Dimensions{100, 30}); err != nil {
		t.Fatal(err)
	}
	if _, err := process.Write([]byte("\n")); err != nil {
		t.Fatal(err)
	}
	output, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	result, err := process.Wait(context.Background())
	if err != nil || result.Code != 0 {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	defer process.CloseIO()
	text := first + string(output)
	if !strings.Contains(text, "24 80") || !strings.Contains(text, "30 100") {
		t.Fatalf("output=%q", text)
	}
}

func TestSignalTargetsPTYProcessGroup(t *testing.T) {
	process, _ := startShell(t, "trap 'exit 42' TERM; echo ready; while :; do sleep 1; done")
	buffer := make([]byte, 64)
	if _, err := process.Read(buffer); err != nil {
		t.Fatal(err)
	}
	if err := process.Signal(Terminate); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	result, err := process.Wait(ctx)
	defer process.CloseIO()
	if err != nil || result.Code != 42 {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}

func TestAdapterRejectsEscapedCWDAndInvalidEnvironment(t *testing.T) {
	root := t.TempDir()
	adapter, err := NewAdapter(root)
	if err != nil {
		t.Fatal(err)
	}
	command := Command{Path: shellPath(t), Args: []string{"-c", "exit"}, Env: []string{"PATH=/bin"}, CWD: filepath.Dir(root), Dimensions: Dimensions{80, 24}}
	if _, err := adapter.Start(command); !errors.Is(err, ErrInvalidCWD) {
		t.Fatalf("cwd err=%v", err)
	}
	command.CWD = root
	command.Env = []string{"A=1", "A=2"}
	if _, err := adapter.Start(command); !errors.Is(err, ErrInvalidCommand) {
		t.Fatalf("env err=%v", err)
	}
}

func TestProcessPolicyRejectsUnboundedOrNULStaticValues(t *testing.T) {
	tooManyArguments := make([]string, 65)
	tooManyEnvironment := make([]string, maximumProcessEnvironmentEntries+1)
	for index := range tooManyEnvironment {
		tooManyEnvironment[index] = fmt.Sprintf("KEY_%d=value", index)
	}
	for _, test := range []struct {
		arguments   []string
		environment []string
	}{
		{arguments: []string{"bad\x00argument"}},
		{arguments: tooManyArguments},
		{environment: []string{"BAD=bad\x00value"}},
		{environment: []string{"Token=one", "TOKEN=two"}},
		{environment: tooManyEnvironment},
	} {
		if _, err := ValidateProcessPolicy("/bin/sh", test.arguments, test.environment); !errors.Is(err, ErrInvalidCommand) {
			t.Fatalf("arguments=%d environment=%d err=%v", len(test.arguments), len(test.environment), err)
		}
	}
	if _, err := ValidateProcessPolicy("/bin/sh", nil, []string{"LONG=" + strings.Repeat("x", 32_767)}); err != nil {
		t.Fatalf("Managed environment maximum value rejected: %v", err)
	}
}

func TestTerminateEscalatesWithinBound(t *testing.T) {
	process, _ := startShell(t, "trap '' TERM; while :; do sleep 1; done")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	result, err := process.Terminate(ctx, 10*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if result.Signal == "" && result.Code == 0 {
		t.Fatalf("result=%#v", result)
	}
}

func TestTerminateRetainsOwnedCleanupFailureAndRecovers(t *testing.T) {
	process, _ := startShell(t, "exit 0")
	defer process.CloseIO()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	before, err := process.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := process.file.Close(); err != nil {
		t.Fatal(err)
	}
	result, err := process.Terminate(ctx, 0)
	if !errors.Is(err, os.ErrClosed) {
		t.Fatal("completed termination discarded owned cleanup failure")
	}
	if result != before || result.ExitedAt.IsZero() {
		t.Fatal("cleanup failure altered reaped result")
	}
	var readers sync.WaitGroup
	failed := make(chan bool, 8)
	for range 8 {
		readers.Add(1)
		go func() { defer readers.Done(); failed <- errors.Is(process.CloseIO(), os.ErrClosed) }()
	}
	readers.Wait()
	close(failed)
	for retained := range failed {
		if !retained {
			t.Fatal("idempotent close erased first cleanup failure")
		}
	}
	fresh, _ := startShell(t, "exit 0")
	defer fresh.CloseIO()
	if _, err := fresh.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if result, err := fresh.Terminate(ctx, 0); err != nil || result.ExitedAt.IsZero() {
		t.Fatal("fresh healthy cleanup did not recover")
	}
}

// A paste into a program that never reads stdin fills the tty queue and blocks; Ctrl-C must still stop the program
// at once, and the paste left in the queue must never reach the shell as commands (F3-11).
func TestInterruptOvertakesAPasteTheProgramNeverReads(t *testing.T) {
	// The shell survives SIGINT (a handler, so sleep keeps the default) and then reads what is left in the queue.
	process, root := startShell(t, `trap : INT; sleep 30; dd iflag=nonblock bs=4096 count=1 of=got 2>/dev/null; echo done > ran`)
	paste := bytes.Repeat([]byte("echo pasted > pasted\n"), 256<<10/21)
	written := make(chan error, 1)
	go func() { _, err := process.Write(paste); written <- err }()
	time.Sleep(300 * time.Millisecond)
	start := time.Now()
	handled, err := process.Interrupt(0x03)
	if err != nil || !handled {
		t.Fatalf("interrupt: handled=%v err=%v", handled, err)
	}
	select {
	case err := <-written:
		if !errors.Is(err, ErrInterrupted) {
			t.Fatalf("paste write: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("paste write still blocked after the interrupt")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := process.Wait(ctx)
	if err != nil || time.Since(start) > 2*time.Second {
		t.Fatalf("shell did not stop after interrupt: %+v %v", result, err)
	}
	if _, err := os.Stat(filepath.Join(root, "ran")); err != nil {
		t.Fatalf("the shell did not continue after the interrupt: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(root, "got")); len(got) != 0 {
		t.Fatalf("%d bytes of the paste were still queued after the interrupt", len(got))
	}
}

// In raw mode the interrupt key is ordinary data for the program; Interrupt must leave it alone.
func TestInterruptLeavesRawModeInputAlone(t *testing.T) {
	process, _ := startShell(t, `stty raw -isig; sleep 30`)
	defer process.Terminate(context.Background(), time.Second)
	time.Sleep(300 * time.Millisecond)
	if handled, err := process.Interrupt(0x03); handled || err != nil {
		t.Fatalf("raw mode interrupt: handled=%v err=%v", handled, err)
	}
}

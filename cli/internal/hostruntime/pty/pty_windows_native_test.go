//go:build windows && paperboat_native_e2e

package pty

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

const stillActive = 259

type nativeOutput struct {
	mu   sync.Mutex
	body bytes.Buffer
	done chan struct{}
}

func collectNativeOutput(process *Process) *nativeOutput {
	output := &nativeOutput{done: make(chan struct{})}
	go func() {
		defer close(output.done)
		buffer := make([]byte, 4096)
		for {
			count, err := process.Read(buffer)
			if count > 0 {
				output.mu.Lock()
				_, _ = output.body.Write(buffer[:count])
				output.mu.Unlock()
			}
			if err != nil {
				if err != io.EOF && !strings.Contains(strings.ToLower(err.Error()), "closed") {
					output.mu.Lock()
					_, _ = fmt.Fprintf(&output.body, "\n[reader error: %v]", err)
					output.mu.Unlock()
				}
				return
			}
		}
	}()
	return output
}

func (o *nativeOutput) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.body.String()
}

func (o *nativeOutput) waitFor(t *testing.T, text string) {
	t.Helper()
	o.waitForWithin(t, text, 10*time.Second)
}

func (o *nativeOutput) waitForWithin(t *testing.T, text string, limit time.Duration) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if strings.Contains(o.String(), text) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	tail := o.String()
	if len(tail) > 400 {
		tail = tail[len(tail)-400:]
	}
	t.Fatalf("timed out waiting for %q in ConPTY output ending %q", text, tail)
}

var (
	consoleCSI = regexp.MustCompile("\x1b\\[[0-9;?]*[A-Za-z]")
	consoleOSC = regexp.MustCompile("\x1b\\][^\a]*\a")
	countOne   = regexp.MustCompile(`(?m)^1\r?$`)
)

// countIsOne: the output holds a line that is just `1` (the `find /c` count) once ConPTY's cursor and title sequences are gone; they can
// glue to the line start (cursor-show before it, a title bell) so a literal "\r\n1\r\n" is not reliable.
func countIsOne(output string) bool {
	return countOne.MatchString(consoleOSC.ReplaceAllString(consoleCSI.ReplaceAllString(output, ""), ""))
}

func nativeAdapter(t *testing.T) (*Adapter, string) {
	t.Helper()
	root := t.TempDir()
	adapter, err := NewAdapter(root)
	if err != nil {
		t.Fatal(err)
	}
	return adapter, root
}

func startNativePTY(t *testing.T, path string, args []string) (*Process, *nativeOutput) {
	t.Helper()
	adapter, root := nativeAdapter(t)
	process, err := adapter.Start(Command{Path: path, Args: args, CWD: root, Dimensions: Dimensions{Columns: 80, Rows: 25}})
	if err != nil {
		t.Fatalf("start native PTY: %v", err)
	}
	return process, collectNativeOutput(process)
}

func nativePowerShell(t *testing.T) string {
	t.Helper()
	path := filepath.Join(os.Getenv("WINDIR"), "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestNativeConPTYCtrlCInterruptsForegroundAndPreservesShell(t *testing.T) {
	commandShell := os.Getenv("ComSpec")
	if commandShell == "" {
		commandShell = filepath.Join(os.Getenv("WINDIR"), "System32", "cmd.exe")
	}
	process, output := startNativePTY(t, commandShell, []string{"/d", "/q"})
	defer process.CloseIO()
	if _, err := process.Write([]byte("echo paperboat-shell-ready\r\n")); err != nil {
		t.Fatal(err)
	}
	output.waitFor(t, "paperboat-shell-ready")
	if _, err := process.Write([]byte("ping -t 127.0.0.1\r\n")); err != nil {
		t.Fatal(err)
	}
	output.waitFor(t, "Reply from")
	if err := process.Signal(Interrupt); err != nil {
		t.Fatalf("send Ctrl+C: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	if _, err := process.Write([]byte("echo paperboat-after-ctrl-c\r\nexit /b 7\r\n")); err != nil {
		t.Fatalf("shell did not survive Ctrl+C: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := process.Wait(ctx)
	if err != nil {
		t.Fatalf("wait after Ctrl+C: %v; output=%q", err, output.String())
	}
	output.waitFor(t, "paperboat-after-ctrl-c")
	if result.Code != 7 {
		t.Fatalf("exit code=%d, want 7; output=%q", result.Code, output.String())
	}
}

// A paste into a flood the program never reads fills the ConPTY input pipe; Ctrl-C must still overtake it (the unix twin
// is TestInterruptOvertakesAPasteTheProgramNeverReads).
func TestNativeConPTYInterruptOvertakesAStuckPaste(t *testing.T) {
	process, output := startNativePTY(t, nativePowerShell(t), []string{"-NoLogo", "-NoProfile"})
	defer process.CloseIO()
	flood := "while(1){[Console]::Out.WriteLine('y')}\r\n"
	if _, err := process.Write([]byte(flood)); err != nil {
		t.Fatal(err)
	}
	output.waitFor(t, "y\r\ny\r\n")
	stuck := make(chan error, 1)
	go func() {
		_, err := process.Write([]byte(strings.Repeat("0123456789abcdef", 1<<20))) // 16 MiB nobody reads; 256 KiB fits the pipe and conhost buffer on the runner
		stuck <- err
	}()
	time.Sleep(2 * time.Second) // the pipe is full and the write is blocked
	select {
	case err := <-stuck:
		t.Fatalf("the 16 MiB paste finished before Ctrl-C (err=%v): the fixture is not stuck", err)
	default:
	}
	started := time.Now()
	handled, err := process.Interrupt(0x03, nil)
	if err != nil || !handled {
		t.Fatalf("Interrupt handled=%v err=%v", handled, err)
	}
	select {
	case err := <-stuck:
		if !errors.Is(err, ErrInterrupted) { // the journal finalizes only this error; anything else is resent after Ctrl-C
			t.Fatalf("the cut paste ended with %v, want ErrInterrupted", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the stuck paste was not cancelled")
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("Ctrl-C took %v behind a stuck paste", elapsed)
	}
	// Not asserted: that the flood dies. ConPTY has no input flush and no out-of-band interrupt (GenerateConsoleCtrlEvent from a
	// helper inside the console is dropped by conhost, measured on Windows 11), so ETX queues behind the bytes conhost already
	// accepted and the shell sees it only after reading them. What Interrupt guarantees is a bounded, final end of the stuck write.
}

// Interrupt must stop a foreground program that never reads its input and ignores the pipe ETX (cmd.exe running ping -t): Signal does it
// through the descendant fallback, and the overtake path used to leave it running (it wrote the ETX and reported handled).
func TestNativeConPTYInterruptStopsAForegroundProgramThatIgnoresETX(t *testing.T) {
	commandShell := os.Getenv("ComSpec")
	if commandShell == "" {
		commandShell = filepath.Join(os.Getenv("WINDIR"), "System32", "cmd.exe")
	}
	process, output := startNativePTY(t, commandShell, []string{"/d", "/q"})
	defer process.CloseIO()
	if _, err := process.Write([]byte("ping -t 127.0.0.1\r\n")); err != nil {
		t.Fatal(err)
	}
	output.waitFor(t, "Reply from")
	if handled, err := process.Interrupt(0x03, nil); err != nil || !handled {
		t.Fatalf("Interrupt handled=%v err=%v", handled, err)
	}
	if _, err := process.Write([]byte("echo paperboat-after-interrupt\r\nexit /b 9\r\n")); err != nil {
		t.Fatalf("shell did not survive: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	result, err := process.Wait(ctx)
	if err != nil || result.Code != 9 {
		t.Fatalf("ping was not stopped by Interrupt: result=%+v err=%v", result, err)
	}
}

// A PowerShell started from cmd.exe handles Ctrl-C itself: the command it runs stops and its prompt stays. The descendant fallback
// used to terminate it as "the program cmd is waiting on", returning to cmd.exe (seen from a browser terminal, X-23).
func TestNativeConPTYInterruptKeepsAPowerShellStartedFromCmd(t *testing.T) {
	commandShell := os.Getenv("ComSpec")
	if commandShell == "" {
		commandShell = filepath.Join(os.Getenv("WINDIR"), "System32", "cmd.exe")
	}
	process, output := startNativePTY(t, commandShell, []string{"/d", "/q"})
	defer process.CloseIO()
	if _, err := process.Write([]byte(nativePowerShell(t) + " -NoLogo -NoProfile\r\n")); err != nil {
		t.Fatal(err)
	}
	output.waitFor(t, "PS ")
	if _, err := process.Write([]byte("Start-Sleep 300\r\n")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Second)
	if _, err := process.Interrupt(0x03, nil); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second)
	if _, err := process.Write([]byte("'paperboat-ps-still-' + (20 + 2)\r\n")); err != nil {
		t.Fatalf("shell did not survive: %v", err)
	}
	output.waitForWithin(t, "paperboat-ps-still-22", 10*time.Second)
}

// Ctrl-C ends the foreground program only: a program the shell started earlier in the background keeps running, like the unix twin that
// signals the foreground process group. (cmd: `start /b` background ping, then a foreground ping that ignores the pipe ETX.)
func TestNativeConPTYInterruptLeavesBackgroundWorkAlone(t *testing.T) {
	commandShell := os.Getenv("ComSpec")
	if commandShell == "" {
		commandShell = filepath.Join(os.Getenv("WINDIR"), "System32", "cmd.exe")
	}
	process, output := startNativePTY(t, commandShell, []string{"/d", "/q"})
	defer process.CloseIO()
	if _, err := process.Write([]byte("start /b ping -n 300 127.0.0.2 >nul\r\n")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second)
	if _, err := process.Write([]byte("ping -t 127.0.0.1\r\n")); err != nil {
		t.Fatal(err)
	}
	output.waitFor(t, "Reply from")
	if handled, err := process.Interrupt(0x03, nil); err != nil || !handled {
		t.Fatalf("Interrupt handled=%v err=%v", handled, err)
	}
	// the shell answers again (the foreground ping is gone) and the background ping is still there
	if _, err := process.Write([]byte("echo paperboat-after-%ERRORLEVEL%-x\r\ntasklist /fi \"imagename eq PING.EXE\" /nh | find /c \"PING.EXE\"\r\n")); err != nil {
		t.Fatalf("shell did not survive: %v", err)
	}
	output.waitForWithin(t, "paperboat-after-", 10*time.Second)
	deadline := time.Now().Add(10 * time.Second)
	for !countIsOne(output.String()) {
		if time.Now().After(deadline) {
			t.Fatalf("expected exactly one PING.EXE left (the background one); output tail=%q", output.String()[max(0, len(output.String())-300):])
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// Ctrl-C at an idle prompt (no program running) leaves the shell alive and at a fresh prompt, like a console: the interrupt path the host now offers
// while idle must not hurt the shell.
func TestNativeConPTYInterruptAtAnIdlePromptKeepsTheShell(t *testing.T) {
	commandShell := os.Getenv("ComSpec")
	if commandShell == "" {
		commandShell = filepath.Join(os.Getenv("WINDIR"), "System32", "cmd.exe")
	}
	process, output := startNativePTY(t, commandShell, []string{"/d", "/q"})
	defer process.CloseIO()
	if _, err := process.Write([]byte("echo paperboat-idle-ready\r\n")); err != nil {
		t.Fatal(err)
	}
	output.waitFor(t, "paperboat-idle-ready")
	if handled, err := process.Interrupt(0x03, nil); err != nil || !handled {
		t.Fatalf("Interrupt handled=%v err=%v", handled, err)
	}
	if _, err := process.Write([]byte("echo paperboat-idle-after\r\nexit /b 4\r\n")); err != nil {
		t.Fatalf("the shell did not survive Ctrl-C at an idle prompt: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := process.Wait(ctx)
	if err != nil || result.Code != 4 {
		t.Fatalf("shell after an idle Ctrl-C: result=%+v err=%v", result, err)
	}
	output.waitFor(t, "paperboat-idle-after")
}

// At an idle cmd.exe prompt with only a background child, Ctrl-C must not end the background program (Unix: Ctrl-C at the prompt never touches background
// jobs): the shell holds no process handle to a `start /b` child, so there is no foreground program.
func TestNativeConPTYInterruptAtAnIdlePromptLeavesBackgroundWorkAlone(t *testing.T) {
	commandShell := os.Getenv("ComSpec")
	if commandShell == "" {
		commandShell = filepath.Join(os.Getenv("WINDIR"), "System32", "cmd.exe")
	}
	process, output := startNativePTY(t, commandShell, []string{"/d", "/q"})
	defer process.CloseIO()
	if _, err := process.Write([]byte("start /b ping -n 300 127.0.0.2 >nul\r\n")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Second)
	if handled, err := process.Interrupt(0x03, nil); err != nil || !handled {
		t.Fatalf("Interrupt handled=%v err=%v", handled, err)
	}
	time.Sleep(500 * time.Millisecond)
	if _, err := process.Write([]byte("echo paperboat-bg-%ERRORLEVEL%-x\r\ntasklist /fi \"imagename eq PING.EXE\" /nh | find /c \"PING.EXE\"\r\n")); err != nil {
		t.Fatalf("shell did not survive: %v", err)
	}
	output.waitForWithin(t, "paperboat-bg-", 10*time.Second)
	deadline := time.Now().Add(10 * time.Second)
	for !countIsOne(output.String()) {
		if time.Now().After(deadline) {
			t.Fatalf("the background ping did not survive an idle Ctrl-C; output tail=%q", output.String()[max(0, len(output.String())-300):])
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// interruptAtThePowerShellPrompt sends a Ctrl-C at an idle PowerShell prompt the way session.Manager.Interrupt's caller does.
// PSReadLine clears ENABLE_PROCESSED_INPUT while it reads a line, so there the key is data: Interrupt must report false (nothing
// cut or discarded) and the caller writes it, which PSReadLine answers with ^C and a fresh prompt. That ^C is the proof the byte
// reached PowerShell's input; a dropped Ctrl-C shows none.
func interruptAtThePowerShellPrompt(t *testing.T, process *Process, output *nativeOutput) {
	t.Helper()
	handled, err := process.Interrupt(0x03, nil)
	if err != nil || handled {
		t.Fatalf("Interrupt at a PowerShell prompt = %v, %v; want false (PSReadLine has the console in raw mode)", handled, err)
	}
	if _, err := process.Write([]byte{0x03}); err != nil {
		t.Fatal(err)
	}
	output.waitFor(t, "^C")
}

// Another shell than cmd.exe: nothing is ever terminated by a Ctrl-C (no positive foreground evidence there), whatever the shell has started.
func TestNativeConPTYInterruptWithAPowerShellShellTerminatesNothing(t *testing.T) {
	process, output := startNativePTY(t, nativePowerShell(t), []string{"-NoLogo", "-NoProfile"})
	defer process.CloseIO()
	if _, err := process.Write([]byte("Start-Process -FilePath ping -ArgumentList '-n','300','127.0.0.2' -WindowStyle Hidden; 'paperboat-ps-started'\r\n")); err != nil {
		t.Fatal(err)
	}
	output.waitFor(t, "paperboat-ps-started")
	time.Sleep(time.Second)
	interruptAtThePowerShellPrompt(t, process, output)
	time.Sleep(time.Second)
	if _, err := process.Write([]byte("'paperboat-ps-count-' + @(Get-Process ping -ErrorAction SilentlyContinue).Count\r\n")); err != nil {
		t.Fatalf("shell did not survive: %v", err)
	}
	output.waitForWithin(t, "paperboat-ps-count-1", 10*time.Second)
}

// A command PowerShell is running has the console back in cooked mode (PSReadLine restores it around the command), so Ctrl-C
// is the interrupt there: Interrupt reports true and the running ping stops (it prints Control-C), with the shell alive. This is
// the proof the raw-mode check did not break interrupting a running command in PowerShell.
func TestNativeConPTYInterruptStopsAPingRunInPowerShell(t *testing.T) {
	// The enrolled host is started in its own process group, so it ignores Ctrl+C and its children inherit that
	// (measured on the runner: flag 1 on session-owner, cmd, powershell, ping). Start from the same state: the
	// adapter must clear it for the session child, or no ETX ever reaches ping.
	if err := setIgnoreCtrlC(true); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = setIgnoreCtrlC(false) })
	process, output := startNativePTY(t, nativePowerShell(t), []string{"-NoLogo", "-NoProfile"})
	defer process.CloseIO()
	if _, err := process.Write([]byte("ping -t 127.0.0.1\r\n")); err != nil {
		t.Fatal(err)
	}
	output.waitFor(t, "Reply from")
	handled, err := process.Interrupt(0x03, nil)
	if err != nil || !handled {
		t.Fatalf("Interrupt during a running ping = %v, %v; want true (the console is cooked while a command runs)", handled, err)
	}
	output.waitForWithin(t, "Control-C", 10*time.Second)
	time.Sleep(time.Second)
	if _, err := process.Write([]byte("'paperboat-ping-count-' + @(Get-Process ping -ErrorAction SilentlyContinue).Count\r\n")); err != nil {
		t.Fatalf("shell did not survive: %v", err)
	}
	output.waitForWithin(t, "paperboat-ping-count-0", 10*time.Second)
}

// PowerShell jobs run in a worker child the shell holds a handle to while idle (the reason a handle rule is only used for cmd.exe): a Ctrl-C at
// the prompt must leave a Start-Job (powershell.exe 5.1) and a background `&` job (pwsh 7, skipped when not installed) running.
func TestNativeConPTYInterruptLeavesPowerShellJobsAlone(t *testing.T) {
	cases := []struct {
		name, shell, start string
	}{
		{"Start-Job in powershell.exe", "", "Start-Job { ping -n 300 127.0.0.2 } | Out-Null"},
		{"background operator in pwsh", "pwsh", "ping -n 300 127.0.0.2 > $null &"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			shell := test.shell
			if shell == "" {
				shell = nativePowerShell(t)
			} else if found, err := exec.LookPath(shell); err != nil {
				t.Skipf("%s is not installed", shell)
			} else {
				shell = found
			}
			process, output := startNativePTY(t, shell, []string{"-NoLogo", "-NoProfile"})
			defer process.CloseIO()
			if _, err := process.Write([]byte(test.start + "; 'paperboat-job-started'\r\n")); err != nil {
				t.Fatal(err)
			}
			output.waitFor(t, "paperboat-job-started")
			// the job's worker is up and its ping runs: a cold first Start-Job takes up to 15 s, so ask until the ping is there
			for ready, deadline := false, time.Now().Add(40*time.Second); !ready; {
				if time.Now().After(deadline) {
					t.Fatalf("the job's ping never started; output tail=%q", output.String()[max(0, len(output.String())-200):])
				}
				if _, err := process.Write([]byte("'paperboat-job-up-' + @(Get-Process ping -ErrorAction SilentlyContinue).Count\r\n")); err != nil {
					t.Fatal(err)
				}
				time.Sleep(2 * time.Second)
				ready = strings.Contains(output.String(), "paperboat-job-up-1")
			}
			interruptAtThePowerShellPrompt(t, process, output)
			time.Sleep(time.Second)
			if _, err := process.Write([]byte("'paperboat-job-count-' + @(Get-Process ping -ErrorAction SilentlyContinue).Count\r\n")); err != nil {
				t.Fatalf("shell did not survive: %v", err)
			}
			output.waitForWithin(t, "paperboat-job-count-1", 10*time.Second)
		})
	}
}

func TestNativeConPTYResizeAndUTF8VT(t *testing.T) {
	process, output := startNativePTY(t, nativePowerShell(t), []string{"-NoLogo", "-NoProfile"})
	if err := process.Resize(Dimensions{Columns: 101, Rows: 37}); err != nil {
		t.Fatal(err)
	}
	command := "[Console]::OutputEncoding=[Text.UTF8Encoding]::new(); Write-Output ('SIZE={0}x{1}' -f $Host.UI.RawUI.WindowSize.Width,$Host.UI.RawUI.WindowSize.Height); Write-Output ([char]27+'[32m紙船-✓'+[char]27+'[0m'); exit 0\r\n"
	if _, err := process.Write([]byte(command)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := process.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(250 * time.Millisecond)
	_ = process.CloseIO()
	<-output.done
	if result.Code != 0 {
		t.Fatalf("exit code=%d output=%q", result.Code, output.String())
	}
	body := output.String()
	if !strings.Contains(body, "SIZE=101x37") || !strings.Contains(body, "紙船-✓") || !strings.Contains(body, "\x1b[32m") {
		t.Fatalf("resize/UTF-8/VT contract missing from %q", body)
	}
}

func TestNativeConPTYJobCloseKillsDescendant(t *testing.T) {
	script := "$p=Start-Process powershell.exe -ArgumentList '-NoLogo','-NoProfile','-Command','Start-Sleep -Seconds 120' -PassThru; Write-Output ('CHILD='+$p.Id)"
	process, output := startNativePTY(t, nativePowerShell(t), []string{"-NoLogo", "-NoProfile", "-NonInteractive", "-Command", script})
	defer process.CloseIO()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if result, err := process.Wait(ctx); err != nil || result.Code != 0 {
		t.Fatalf("parent result=%+v err=%v output=%q", result, err, output.String())
	}
	output.waitFor(t, "CHILD=")
	match := regexp.MustCompile(`CHILD=(\d+)`).FindStringSubmatch(output.String())
	if len(match) != 2 {
		t.Fatalf("child PID missing from %q", output.String())
	}
	pid, _ := strconv.ParseUint(match[1], 10, 32)
	if err := process.CloseIO(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
		if err != nil {
			return
		}
		var code uint32
		err = windows.GetExitCodeProcess(handle, &code)
		windows.CloseHandle(handle)
		if err != nil || code != stillActive {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("descendant PID %d remained active after closing the kill-on-close PTY job", pid)
}

func TestNativeConPTYTerminateRetainsCleanupFailureAndRecovers(t *testing.T) {
	command := filepath.Join(os.Getenv("WINDIR"), "System32", "cmd.exe")
	process, output := startNativePTY(t, command, []string{"/d", "/c", "exit 0"})
	defer process.CloseIO()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	before, err := process.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-output.done:
	case <-ctx.Done():
		t.Fatal("ConPTY output worker did not join")
	}
	// The waiter closes only its pseudoconsole. Fail the first owned file Close.
	if err := process.input.Close(); err != nil {
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
	fresh, freshOutput := startNativePTY(t, command, []string{"/d", "/c", "exit 0"})
	defer fresh.CloseIO()
	if _, err := fresh.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-freshOutput.done:
	case <-ctx.Done():
		t.Fatal("healthy output worker did not join")
	}
	if result, err := fresh.Terminate(ctx, 0); err != nil || result.ExitedAt.IsZero() {
		t.Fatal("fresh healthy cleanup did not recover")
	}
}

// F3-11 finding 5: a program that cleared ENABLE_PROCESSED_INPUT gets 0x03 as data, as on unix without ISIG: Interrupt
// reports false (nothing cut or discarded), and the byte the caller then writes arrives as the key.
func TestNativeConPTYInterruptInRawModeIsData(t *testing.T) {
	script := "[Console]::TreatControlCAsInput=$true; Write-Output 'raw-ready'; $k=[Console]::ReadKey($true); Write-Output ('KEY:'+[int]$k.KeyChar)"
	process, output := startNativePTY(t, nativePowerShell(t), []string{"-NoProfile", "-NonInteractive", "-Command", script})
	defer process.CloseIO()
	output.waitFor(t, "raw-ready")
	time.Sleep(500 * time.Millisecond) // the program is in ReadKey with the mode cleared
	handled, err := process.Interrupt(0x03, nil)
	if err != nil || handled {
		t.Fatalf("Interrupt in raw mode = %v, %v; want false (the byte is data)", handled, err)
	}
	if _, err := process.Write([]byte{0x03}); err != nil {
		t.Fatal(err)
	}
	output.waitFor(t, "KEY:3")
}

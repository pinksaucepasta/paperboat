//go:build darwin || linux

package managedssh

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

func TestOpenSSHExecutorHelperProcess(t *testing.T) {
	mode := os.Getenv("PB_EXECUTOR_HELPER")
	if mode == "" {
		return
	}
	switch mode {
	case "supervise_signal":
		binary := "/bin/sh"
		arguments := []string{"-c", `echo $$ > "$PB_EXECUTOR_CHILD"; exec sleep 60`}
		err := (OpenSSHExecutor{}).Execute(context.Background(), binary, arguments, os.Environ())
		var native NativeExitError
		if !errors.As(err, &native) {
			os.Exit(14)
		}
		os.WriteFile(os.Getenv("PB_EXECUTOR_RESULT"), []byte(strconv.Itoa(native.ExitCode())), 0600)
		os.Exit(0)
	case "arguments":
		arguments := os.Args[stringsIndex(os.Args, "--")+1:]
		data, _ := json.Marshal(struct {
			Args  []string
			Value string
		}{arguments, os.Getenv("PB_EXECUTOR_PRIVATE")})
		if err := os.WriteFile(os.Getenv("PB_EXECUTOR_RESULT"), data, 0600); err != nil {
			os.Exit(8)
		}
		os.Exit(0)
	case "ignore_term":
		signal.Ignore(syscall.SIGTERM)
		os.WriteFile(os.Getenv("PB_EXECUTOR_RESULT"), []byte(strconv.Itoa(os.Getpid())), 0600)
		for {
			time.Sleep(time.Hour)
		}
	case "terminal_child":
		group, _ := unix.IoctlGetInt(0, unix.TIOCGPGRP)
		if !term.IsTerminal(0) || group != syscall.Getpgrp() {
			os.Exit(9)
		}
		previous, err := term.MakeRaw(0)
		if err != nil {
			os.Exit(10)
		}
		defer term.Restore(0, previous)
		os.Stdout.WriteString("TTY_READY\n")
		var value [1]byte
		if _, err := io.ReadFull(os.Stdin, value[:]); err != nil {
			os.Exit(11)
		}
		term.Restore(0, previous)
		return
	case "terminal_parent":
		previous, err := term.GetState(0)
		if err != nil {
			os.Exit(12)
		}
		binary, _ := os.Executable()
		environment := append(os.Environ(), "PB_EXECUTOR_HELPER=terminal_child")
		err = (OpenSSHExecutor{}).Execute(context.Background(), binary, []string{"-test.run=TestOpenSSHExecutorHelperProcess"}, environment)
		after, stateErr := term.GetState(0)
		if err != nil || stateErr != nil || *previous != *after {
			os.Exit(13)
		}
		os.Stdout.WriteString("TTY_RESTORED\n")
		os.Exit(0)
	}
}
func stringsIndex(values []string, value string) int {
	for index, v := range values {
		if v == value {
			return index
		}
	}
	return len(values) - 1
}

func TestOpenSSHExecutorPreservesArgumentsAndEnvironment(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	result := filepath.Join(t.TempDir(), "result")
	arguments := []string{"-test.run=TestOpenSSHExecutorHelperProcess", "--", "-vv", "deploy@build.local.pprbt.dev", "printf", "%s", "hello world"}
	environment := []string{"PB_EXECUTOR_HELPER=arguments", "PB_EXECUTOR_RESULT=" + result, "PB_EXECUTOR_PRIVATE=PRIVATE_VALUE"}
	if err := (OpenSSHExecutor{}).Execute(context.Background(), binary, arguments, environment); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Args  []string
		Value string
	}
	data, err := os.ReadFile(result)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Value != "PRIVATE_VALUE" || strings.Join(got.Args, "\x00") != strings.Join(arguments[2:], "\x00") {
		t.Fatal("native argv/environment changed")
	}
}
func TestOpenSSHExecutorRejectsInvalidInputsAndRetainsLaunchCause(t *testing.T) {
	for _, test := range []struct{ args, environment []string }{{}, {args: []string{"host\x00bad"}}, {args: []string{"host"}, environment: []string{"MISSING_VALUE"}}, {args: []string{"host"}, environment: []string{"=value"}}} {
		if err := (OpenSSHExecutor{}).Execute(context.Background(), "/bin/sh", test.args, test.environment); !errors.Is(err, ErrOpenSSHExecution) {
			t.Fatal("invalid execution accepted")
		}
	}
	private := filepath.Join(t.TempDir(), "PRIVATE_BINARY")
	err := (OpenSSHExecutor{}).Execute(context.Background(), private, []string{"host"}, nil)
	var pathError *os.PathError
	if !errors.Is(err, ErrOpenSSHExecution) || !errors.As(err, &pathError) || !errors.Is(err, os.ErrNotExist) || strings.Contains(err.Error(), "PRIVATE") {
		t.Fatal("typed private launch cause lost")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := (OpenSSHExecutor{}).Execute(ctx, "/bin/sh", []string{"-c", "exit 0"}, nil); !errors.Is(err, context.Canceled) {
		t.Fatal("prelaunch cancellation ignored")
	}
}
func TestOpenSSHExecutorPreservesNativeExitStatus(t *testing.T) {
	for _, status := range []int{0, 7, 255} {
		err := (OpenSSHExecutor{}).Execute(context.Background(), "/bin/sh", []string{"-c", "exit " + strconv.Itoa(status)}, nil)
		if status == 0 {
			if err != nil {
				t.Fatal(err)
			}
			continue
		}
		var native NativeExitError
		var original *exec.ExitError
		if !errors.As(err, &native) || native.ExitCode() != status || !errors.As(err, &original) {
			t.Fatalf("native status %d lost: %v", status, err)
		}
	}
	err := (OpenSSHExecutor{}).Execute(context.Background(), "/bin/sh", []string{"-c", "kill -TERM $$"}, nil)
	var native NativeExitError
	if !errors.As(err, &native) || native.ExitCode() != 128+int(syscall.SIGTERM) {
		t.Fatal("native signal status lost")
	}
}
func TestOpenSSHExecutorCancellationReapsNativeProcess(t *testing.T) {
	for _, ignoreTerm := range []bool{false, true} {
		t.Run(strconv.FormatBool(ignoreTerm), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := filepath.Join(t.TempDir(), "pid")
			binary := "/bin/sh"
			args := []string{"-c", `echo $$ > "$PB_EXECUTOR_RESULT"; exec sleep 60`}
			environment := append(os.Environ(), "PB_EXECUTOR_RESULT="+result)
			if ignoreTerm {
				binary, _ = os.Executable()
				args = []string{"-test.run=TestOpenSSHExecutorHelperProcess"}
				environment = append(environment, "PB_EXECUTOR_HELPER=ignore_term")
			}
			done := make(chan error, 1)
			go func() { done <- (OpenSSHExecutor{}).Execute(ctx, binary, args, environment) }()
			pid := waitPID(t, result, done)
			began := time.Now()
			cancel()
			select {
			case err := <-done:
				var native NativeExitError
				want := 128 + int(syscall.SIGTERM)
				if ignoreTerm {
					want = 128 + int(syscall.SIGKILL)
				}
				if !errors.As(err, &native) || native.ExitCode() != want {
					t.Fatalf("cancel native status=%v", err)
				}
			case <-time.After(nativeSSHCancelGrace + 2*time.Second):
				syscall.Kill(pid, syscall.SIGKILL)
				t.Fatal("native child not reaped")
			}
			if time.Since(began) > nativeSSHCancelGrace+time.Second || !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
				t.Fatal("native cancellation leaked process")
			}
		})
	}
}
func waitPID(t *testing.T, path string, done <-chan error) int {
	t.Helper()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		data, _ := os.ReadFile(path)
		pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
		if pid > 0 {
			return pid
		}
		select {
		case err := <-done:
			t.Fatalf("native process exited before ready: %v", err)
		case <-deadline.C:
			t.Fatal("native process readiness timed out")
		case <-ticker.C:
		}
	}
}
func TestOpenSSHExecutorInheritsForegroundTTYAndRestoresTerminal(t *testing.T) {
	binary, _ := os.Executable()
	shell, err := exec.LookPath("bash")
	if err != nil {
		t.Fatal("PTY job-control fixture requires bash")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, shell, "--noprofile", "--norc", "-i")
	command.Env = append(os.Environ(), "PB_EXECUTOR_HELPER=terminal_parent", "PS1=PB_PROMPT> ")
	terminal, err := pty.Start(command)
	if err != nil {
		t.Fatal(err)
	}
	defer terminal.Close()
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	nativeGroup := 0
	finished := false
	defer func() {
		if nativeGroup > 0 && nativeGroup != command.Process.Pid {
			_ = syscall.Kill(-nativeGroup, syscall.SIGKILL)
		}
		if !finished {
			_ = command.Process.Kill()
			<-done
		}
	}()
	updates := make(chan string, 64)
	go func() {
		defer close(updates)
		var output strings.Builder
		var data [256]byte
		for {
			n, err := terminal.Read(data[:])
			if output.Len()+n > 16384 {
				return
			}
			output.Write(data[:n])
			select {
			case updates <- output.String():
			default:
			}
			if err != nil {
				return
			}
		}
	}()
	lastOutput := ""
	waitOutput := func(marker string, ready func(string) bool) {
		t.Helper()
		for {
			if ready(lastOutput) {
				return
			}
			select {
			case output, ok := <-updates:
				if !ok {
					t.Fatalf("PTY closed before %s", marker)
				}
				lastOutput = output
			case <-ctx.Done():
				t.Fatalf("PTY timed out before %s", marker)
			}
		}
	}
	waitText := func(marker string) {
		waitOutput(marker, func(output string) bool { return strings.Contains(output, marker) })
	}
	waitText("PB_PROMPT>")
	quoted := "'" + strings.ReplaceAll(binary, "'", "'\"'\"'") + "'"
	if _, err := terminal.Write([]byte(quoted + " -test.run=TestOpenSSHExecutorHelperProcess\n")); err != nil {
		t.Fatal(err)
	}
	waitText("TTY_READY")
	nativeGroup, err = unix.IoctlGetInt(int(terminal.Fd()), unix.TIOCGPGRP)
	if err != nil || nativeGroup <= 0 || nativeGroup == command.Process.Pid {
		t.Fatal("native tool does not share the shell-owned foreground job")
	}
	if err := syscall.Kill(-nativeGroup, syscall.SIGTSTP); err != nil {
		t.Fatal(err)
	}
	waitOutput("shell prompt after stopped job", func(output string) bool {
		return strings.Contains(output, "Stopped") && strings.LastIndex(output, "PB_PROMPT>") > strings.LastIndex(output, "Stopped")
	})
	if _, err := terminal.Write([]byte("fg\n")); err != nil {
		t.Fatal(err)
	}
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for {
		group, _ := unix.IoctlGetInt(int(terminal.Fd()), unix.TIOCGPGRP)
		if group == nativeGroup {
			break
		}
		select {
		case <-deadline.C:
			t.Fatalf("native job did not resume in foreground: %s", lastOutput)
		case output := <-updates:
			lastOutput = output
		case <-time.After(5 * time.Millisecond):
		}
	}
	// The shell restores canonical mode while a job is stopped. Unlike real
	// OpenSSH this small child has no SIGCONT raw-mode handler, so finish its
	// read with a newline after verifying foreground restoration.
	if _, err := terminal.Write([]byte("x\n")); err != nil {
		t.Fatal(err)
	}
	waitText("TTY_RESTORED")
	if _, err := terminal.Write([]byte("exit\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		finished = true
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("PTY shell did not exit")
	}
}

func TestOpenSSHExecutorForwardsDirectedAndGroupSignals(t *testing.T) {
	binary, _ := os.Executable()
	for _, test := range []struct {
		name   string
		group  bool
		signal syscall.Signal
	}{{"direct_term", false, syscall.SIGTERM}, {"group_interrupt", true, syscall.SIGINT}, {"direct_hangup", false, syscall.SIGHUP}} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
			defer cancel()
			directory := t.TempDir()
			result := filepath.Join(directory, "status")
			child := filepath.Join(directory, "child")
			command := exec.CommandContext(ctx, binary, "-test.run=TestOpenSSHExecutorHelperProcess")
			command.Env = append(os.Environ(), "PB_EXECUTOR_HELPER=supervise_signal", "PB_EXECUTOR_RESULT="+result, "PB_EXECUTOR_CHILD="+child)
			command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			defer command.Process.Kill()
			wait := make(chan error, 1)
			go func() { wait <- command.Wait() }()
			pid := waitPID(t, child, wait)
			received := test.signal
			if test.group {
				if err := syscall.Kill(-command.Process.Pid, received); err != nil {
					t.Fatal(err)
				}
			} else if err := command.Process.Signal(received); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-wait:
				if err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				syscall.Kill(pid, syscall.SIGKILL)
				t.Fatal("forwarded signal did not retire native process")
			}
			data, err := os.ReadFile(result)
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != strconv.Itoa(128+int(received)) || !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
				t.Fatal("forwarded native status or cleanup changed")
			}
		})
	}
}
